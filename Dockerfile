FROM golang:1.25-alpine AS builder

RUN apk add --no-cache build-base
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o divisor .

FROM alpine:3.21
RUN apk add --no-cache \
    iproute2 \
    iptables \
    bash

WORKDIR /app
COPY --from=builder /app/divisor /app/divisor
COPY .env /app/.env

ENTRYPOINT ["/app/divisor"]
