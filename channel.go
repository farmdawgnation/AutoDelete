package autodelete

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type smallMessage struct {
	MessageID string
	PostedAt  time.Time
}

const minTimeBetweenDeletion = time.Second * 5
const minTimeBetweenLoadBacklog = time.Millisecond * 30
const backlogReloadLimit = 100
const backlogAutoReloadPreFraction = 0.8
const backlogAutoReloadDeleteFraction = 0.25

// A ManagedChannel holds all the AutoDelete-related state for a Discord channel.
type ManagedChannel struct {
	bot         *Bot
	ChannelID   string
	ChannelName string
	GuildID     string

	mu              sync.Mutex
	backlogMu       sync.Mutex // only for LoadBacklog()
	minNextDelete   time.Time  // channel cannot get sent to deletion before this time
	lastLoadBacklog time.Time  // last LoadBacklog call
	// Messages posted to the channel get deleted after
	MessageLiveTime time.Duration
	MaxMessages     int
	KeepMessages    []string
	// if lower than CriticalMsgSequence, need to send one
	LastSentUpdate int
	IsDonor        bool
	needsExport    bool

	// If true, this ManagedChannel has been disabled; the Bot might have a
	// new version. The reaper thread should throw it out.
	// Observed in the return of collectMessagesToDelete.
	killBit bool

	// if false, need to check channel history for messages
	isStarted chan struct{}
	// liveMessages contains a list of message IDs and the timestamp they
	// were posted at, listing the candidates for deletion in this channel.
	// It should always be sorted with the oldest messages at index 0 and
	// the newer messages at higher indices.
	liveMessages []smallMessage
	// Set of message IDs that need to be kept and not deleted.
	keepLookup map[string]bool
	// Used in queue.go for exponential backoff
	loadFailures time.Duration
}

func InitChannel(b *Bot, chConf ManagedChannelMarshal) (*ManagedChannel, error) {
	disCh, err := b.Channel(chConf.ID)
	if err != nil {
		return nil, err
	}
	needsExport := false
	if disCh.GuildID != chConf.GuildID {
		needsExport = true
	}
	return &ManagedChannel{
		bot:             b,
		ChannelID:       disCh.ID,
		ChannelName:     disCh.Name,
		GuildID:         disCh.GuildID,
		minNextDelete:   time.Now(),
		MessageLiveTime: chConf.LiveTime,
		MaxMessages:     chConf.MaxMessages,
		LastSentUpdate:  chConf.LastSentUpdate,
		KeepMessages:    chConf.KeepMessages,
		IsDonor:         chConf.IsDonor,
		needsExport:     needsExport,
		isStarted:       make(chan struct{}),
		liveMessages:    nil,
		keepLookup:      make(map[string]bool),
	}, nil
}

func (c *ManagedChannel) Export() ManagedChannelMarshal {
	c.mu.Lock()
	defer c.mu.Unlock()

	return ManagedChannelMarshal{
		ID:             c.ChannelID,
		GuildID:        c.GuildID,
		LiveTime:       c.MessageLiveTime,
		MaxMessages:    c.MaxMessages,
		LastSentUpdate: c.LastSentUpdate,
		KeepMessages:   c.KeepMessages,
		IsDonor:        c.IsDonor,
	}
}

func (c *ManagedChannel) String() string {
	return fmt.Sprintf("%s #%s", c.ChannelID, c.ChannelName)
}

// Remove this channel from all relevant datastructures.
//
// Must be called with no locks held. Takes Bot, self, and reapq locks.
// Can be called on a fake ManagedChannel instance (e.g. (&ManagedChannel{ChannelID: ...}).Disable()), so the only member assumed valid is bot and ChannelID.
func (c *ManagedChannel) Disable() {
	// first: block anything from finding us
	c.bot.mu.Lock()
	delete(c.bot.channels, c.ChannelID)
	c.bot.mu.Unlock()

	// reset internal state
	c.mu.Lock()
	c.liveMessages = nil
	c.keepLookup = nil

	c.killBit = true // ensure reapq gets our drop message
	c.mu.Unlock()

	// drop from reapq
	c.bot.CancelReap(c)
}

// Get a discord Channel. Results are cached in the library State.
func (b *Bot) Channel(channelID string) (*discordgo.Channel, error) {
	ch, err := b.s.State.Channel(channelID)
	if ch != nil {
		return ch, nil
	}
	ch, err = b.s.Channel(channelID)
	if err != nil {
		return ch, err
	}
	b.s.State.ChannelAdd(ch)
	return ch, nil
}

func (c *ManagedChannel) loadPins() ([]*discordgo.Message, error) {
	disCh, err := c.bot.Channel(c.ChannelID)
	if err != nil {
		return nil, err
	}

	if disCh.LastPinTimestamp == "" {
		return nil, nil
	} else {
		return c.bot.s.ChannelMessagesPinned(c.ChannelID)
	}
}

func (c *ManagedChannel) LoadBacklogNow() {
	err := c.LoadBacklog()
	if isRetryableLoadError(err) {
		c.bot.QueueLoadBacklog(c, true)
	}
}

func (c *ManagedChannel) LoadBacklog() error {
	// prevent reentrancy, even during web requests
	c.backlogMu.Lock()
	defer c.backlogMu.Unlock()

	// Early exit if we got multiple calls
	earlyExit := false
	c.mu.Lock()
	if c.lastLoadBacklog.Add(minTimeBetweenLoadBacklog).After(time.Now()) {
		earlyExit = true
	} else {
		c.lastLoadBacklog = time.Now()
	}
	c.mu.Unlock()
	if earlyExit {
		fmt.Println("[WARN] Cancelling LoadBacklog for", c, "due to <30s elapsed")
		return nil
	}
	// Clear the progress flag if we set it
	// Set time even on errors
	defer func() {
		c.mu.Lock()
		c.lastLoadBacklog = time.Now()
		c.mu.Unlock()
	}()

	// Load messages & pins
	msgs, err := c.bot.s.ChannelMessages(c.ChannelID, 100, "", "", "")
	if err != nil {
		fmt.Println("[ERR ] could not load backlog for", c, err)
		return err
	}
	pins, pinsErr := c.loadPins()
	if pinsErr != nil {
		fmt.Println("[ERR ] could not load pins for", c, pinsErr)

		// experiment with a notice
		//c.bot.s.ChannelMessageSend(c.ChannelID,
		//	":warning: Failed to load channel pins, may accidentally delete them",
		//)
		return pinsErr
	}

	defer c.bot.QueueReap(c) // requires mutex unlocked
	c.mu.Lock()
	defer c.mu.Unlock()

	c.keepLookup = make(map[string]bool)
	for i := range pins {
		c.keepLookup[pins[i].ID] = true
	}
	for _, v := range c.KeepMessages {
		c.keepLookup[v] = true
	}

	c.liveMessages = make([]smallMessage, 0, len(msgs))
	// Iterate backwards so we swap the order
	for i := len(msgs); i > 0; i-- {
		v := msgs[i-1]

		// Check for non-deletion
		if c.keepLookup[v.ID] {
			continue
		}

		ts, err := v.Timestamp.Parse()
		if err != nil {
			panic("Timestamp format change")
		}
		if ts.IsZero() {
			continue
		}
		c.liveMessages = append(c.liveMessages, smallMessage{
			MessageID: v.ID,
			PostedAt:  ts,
		})
	}

	// mark as ready for AddMessage()
	inited := "reloaded"
	select {
	case <-c.isStarted:
	default:
		close(c.isStarted)
		inited = "initialized"
	}
	fmt.Printf("[load] %s %s, %d msgs %d keeps\n", c.String(), inited, len(c.liveMessages), len(c.keepLookup))
	return nil
}

func (b *Bot) LoadAllBacklogs() {
	b.mu.RLock()
	for _, v := range b.channels {
		if v != nil {
			go v.LoadBacklogNow()
		}
	}
	b.mu.RUnlock()
}

func (c *ManagedChannel) AddMessage(m *discordgo.Message) {
	<-c.isStarted
	needReap := false

	// if m.Type == discordgo.MessageTypeChannelPinnedMessage {
	//	fmt.Println("[DEBUG]", "Got pinning message", m)
	// }

	c.mu.Lock()
	// Check for nondeletion
	if c.keepLookup[m.ID] {
		c.mu.Unlock()
		return
	}

	if len(c.liveMessages) == 0 {
		needReap = true
	} else if c.MaxMessages > 0 && len(c.liveMessages) == c.MaxMessages {
		needReap = true
	}

	c.liveMessages = append(c.liveMessages, smallMessage{
		MessageID: m.ID,
		PostedAt:  time.Now(),
	})
	c.mu.Unlock()

	if needReap {
		c.bot.QueueReap(c)
	}
}

// UpdatePins gets called in two situations - a pin was added, a pin was
// removed, or more than one of those happened too fast for us to notice.
func (c *ManagedChannel) UpdatePins(newLpts string) {
	var dropMsgs []string
	defer func() {
		// This is not the best, as the pins will be deleted
		// non-chronologically, but it avoids chopping the backlog back to 100
		// messages.
		for _, v := range dropMsgs {
			msg, err := c.bot.s.ChannelMessage(c.ChannelID, v)
			if err == nil {
				c.AddMessage(msg)
			}
		}
	}()
	c.mu.Lock()
	defer c.mu.Unlock()

	pins, err := c.bot.s.ChannelMessagesPinned(c.ChannelID)
	if err != nil {
		fmt.Println("[pins] could not load pins for", c, err)
		return
	}

	newKeep := make(map[string]bool)

	for _, v := range pins {
		newKeep[v.ID] = true
	}
	for _, v := range c.KeepMessages {
		newKeep[v] = true
	}

	for id := range c.keepLookup {
		if !newKeep[id] {
			dropMsgs = append(dropMsgs, id)
		}
	}

	fmt.Println("[pins] update for", c, "-", len(newKeep), "keep", len(dropMsgs), "drop")
	c.keepLookup = newKeep
	// deferred function calls AddMessage for each of dropMsgs
}

// DoNotDeleteMessage marks a message ID as not for deletion.
// only called from UpdatePins()
func (c *ManagedChannel) DoNotDeleteMessage(msgID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := -1

	for i, v := range c.liveMessages {
		if v.MessageID == msgID {
			idx = i
		}
	}
	if idx == -1 {
		fmt.Println("[BUG] DoNotDeleteMessage called with non-live message")
		return
	}
	lenMinus1 := len(c.liveMessages) - 1
	// Delete item
	copy(c.liveMessages[idx:], c.liveMessages[idx+1:])
	c.liveMessages[lenMinus1] = smallMessage{}
	c.liveMessages = c.liveMessages[:lenMinus1]
}

func (c *ManagedChannel) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.killBit && (c.MessageLiveTime > 0 || c.MaxMessages > 0)
}

func (c *ManagedChannel) SetLiveTime(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MessageLiveTime = d
}

func (c *ManagedChannel) SetMaxMessages(max int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.MaxMessages = max
}

func (c *ManagedChannel) GetNextDeletionTime() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	for len(c.liveMessages) > 0 {
		// Recheck keepLookup
		if c.keepLookup[c.liveMessages[0].MessageID] {
			c.liveMessages = c.liveMessages[1:]
			continue
		}
		break
	}
	if len(c.liveMessages) == 0 {
		return time.Now().Add(240 * time.Hour)
	}

	if c.MaxMessages > 0 && len(c.liveMessages) > c.MaxMessages {
		return c.minNextDelete
	}
	if c.MessageLiveTime != 0 {
		ts := c.liveMessages[0].PostedAt.Add(c.MessageLiveTime)
		if ts.Before(c.minNextDelete) {
			return c.minNextDelete
		}
		return ts
	}
	return time.Now().Add(240 * time.Hour)
}

const errCodeBulkDeleteOld = 50034

func (c *ManagedChannel) Reap(msgs []string) (int, error) {
	var err error
	count := 0

nobulk:
	switch {
	case true:
		for len(msgs) > 50 {
			err := c.bot.s.ChannelMessagesBulkDelete(c.ChannelID, msgs[:50])
			if rErr, ok := err.(*discordgo.RESTError); ok {
				if rErr.Message != nil && rErr.Message.Code == errCodeBulkDeleteOld {
					break nobulk
				}
				return count, err
			} else if err != nil {
				return count, err
			}
			msgs = msgs[50:]
			count += 50
		}
		err = c.bot.s.ChannelMessagesBulkDelete(c.ChannelID, msgs)
		count += len(msgs)
		if rErr, ok := err.(*discordgo.RESTError); ok {
			if rErr.Message != nil && rErr.Message.Code == errCodeBulkDeleteOld {
				break nobulk
			}
			return count, err
		} else if err != nil {
			return count, err
		}
		return count, nil
	}

	// single delete required
	// Spin up a separate goroutine - this could take a while
	go func() {
		for _, msg := range msgs {
			err = c.bot.s.ChannelMessageDelete(c.ChannelID, msg)
			if err != nil {
				fmt.Printf("[ERR ] %s: single-message delete: %v (on %v)\n", c, err, msg)
			}
		}
		// re-load the backlog in case this surfaced more things to delete
		c.bot.QueueLoadBacklog(c, true)
	}()
	return -1, nil
}

// returns and removes the messages that need to be deleted right now.
//
// also sets the minNextDelete and returns whether we think there could be more
// messages past the backlog horizon
func (c *ManagedChannel) collectMessagesToDelete() (m []string, needsQueueBacklog, isDisabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minNextDelete = time.Now().Add(minTimeBetweenDeletion)

	// Mechanism for getting channels dropped from the reaper
	if c.killBit {
		return nil, false, true
	}

	var toDelete []string
	var oldest time.Time
	var zero time.Time

	nLiveMessages := len(c.liveMessages)

	if c.MaxMessages > 0 {
		for len(c.liveMessages) > c.MaxMessages {
			if !c.keepLookup[c.liveMessages[0].MessageID] {
				toDelete = append(toDelete, c.liveMessages[0].MessageID)
				if oldest == zero {
					oldest = c.liveMessages[0].PostedAt
				}
			}
			c.liveMessages = c.liveMessages[1:]
		}
	}
	if c.MessageLiveTime > 0 {
		cutoff := time.Now().Add(-c.MessageLiveTime)
		for len(c.liveMessages) > 0 && c.liveMessages[0].PostedAt.Before(cutoff) {
			if !c.keepLookup[c.liveMessages[0].MessageID] {
				toDelete = append(toDelete, c.liveMessages[0].MessageID)
				if oldest == zero {
					oldest = c.liveMessages[0].PostedAt
				}
			}
			c.liveMessages = c.liveMessages[1:]
		}
		// Collect additional messages within 1.5sec of deleted message
		if oldest != zero {
			cutoff = oldest.Add(1500 * time.Millisecond)
			for len(c.liveMessages) > 0 && c.liveMessages[0].PostedAt.Before(cutoff) {
				if !c.keepLookup[c.liveMessages[0].MessageID] {
					toDelete = append(toDelete, c.liveMessages[0].MessageID)
				}
				c.liveMessages = c.liveMessages[1:]
			}
		}
	}

	return toDelete, ((nLiveMessages >= backlogReloadLimit*backlogAutoReloadPreFraction) &&
		(len(toDelete) > backlogReloadLimit*backlogAutoReloadDeleteFraction)), false
}
