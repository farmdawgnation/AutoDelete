FROM golang:latest AS builder
#ENV GO111MODULE=off

COPY . /autodelete
WORKDIR /autodelete

RUN go get

RUN go build -v -o autodelete cmd/autodelete/main.go

FROM gcr.io/distroless/base

COPY --from=builder /autodelete/autodelete /autodelete

ENV HOME=/

EXPOSE 2202

ENTRYPOINT ["/autodelete"]
