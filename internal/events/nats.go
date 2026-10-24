package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"

	"divisor/internal/config"
)

// NATS subscribes to a core NATS subject. Each message is one completed round.
type NATS struct{}

// NewNATS returns the NATS event source.
func NewNATS() *NATS {
	return &NATS{}
}

var _ Source = (*NATS)(nil)

// roundNotice is a best-effort read of a scoring.rounds.closed payload.
// The message arriving is the signal. round_id is a UUID string when present.
type roundNotice struct {
	RoundID   string
	ClosedAt  string
	DecodeErr error
}

// Listen connects to NATS_URL and calls onRound for every message on the subject.
// A broken connection is returned so the caller can reconnect. The payload is
// decoded only for logging and never decides whether to reconfigure.
func (n *NATS) Listen(ctx context.Context, onRound func() error) error {
	cfg := config.Load()
	slog.Debug("Connecting to NATS...", "subject", cfg.NATSSubject)

	// NoReconnect surfaces a dropped server to the caller, matching the Redis loop.
	// Credentials, when used, are the userinfo or token already in NATS_URL.
	closed := make(chan struct{})
	var closeOnce sync.Once
	nc, err := nats.Connect(cfg.NATSURL,
		nats.NoReconnect(),
		nats.ClosedHandler(func(*nats.Conn) {
			closeOnce.Do(func() { close(closed) })
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}
	defer nc.Close()
	slog.Debug("Connected to NATS")

	msgs := make(chan *nats.Msg, 64)
	sub, err := nc.ChanSubscribe(cfg.NATSSubject, msgs)
	if err != nil {
		return fmt.Errorf("failed to subscribe to %s: %w", cfg.NATSSubject, err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-closed:
			return fmt.Errorf("nats connection closed")
		case msg, ok := <-msgs:
			if !ok {
				return fmt.Errorf("nats subscription closed")
			}
			notice := decodeRoundNotice(msg.Data)
			if notice.DecodeErr != nil {
				slog.Warn("round-closed payload decoded leniently", "error", notice.DecodeErr)
			}
			args := make([]any, 0, 4)
			if notice.RoundID != "" {
				args = append(args, "round_id", notice.RoundID)
			}
			if notice.ClosedAt != "" {
				args = append(args, "closed_at", notice.ClosedAt)
			}
			slog.Info("round closed event received, reconfiguring interface", args...)
			if err := onRound(); err != nil {
				slog.Error("Failed to reconfigure network", "error", err)
			}
		}
	}
}

func decodeRoundNotice(data []byte) roundNotice {
	var payload struct {
		RoundID  json.RawMessage `json:"round_id"`
		ClosedAt json.RawMessage `json:"closed_at"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return roundNotice{DecodeErr: fmt.Errorf("malformed round closed payload: %w", err)}
	}

	var notice roundNotice
	if id, err := jsonStringField(payload.RoundID); err != nil {
		notice.DecodeErr = fmt.Errorf("malformed round closed payload: round_id: %w", err)
	} else {
		notice.RoundID = id
	}
	if closedAt, err := jsonStringField(payload.ClosedAt); err != nil {
		if notice.DecodeErr == nil {
			notice.DecodeErr = fmt.Errorf("malformed round closed payload: closed_at: %w", err)
		}
	} else {
		notice.ClosedAt = closedAt
	}
	return notice
}

// jsonStringField reads an optional JSON string. Null and absence are empty.
// A non-string value is reported and does not discard the rest of the message.
func jsonStringField(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}
