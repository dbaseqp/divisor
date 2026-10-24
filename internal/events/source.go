// Package events delivers scoring-round notifications to network reconfiguration.
package events

import (
	"context"

	"divisor/internal/config"
)

// Source listens for one closed round at a time.
// Listen returns when the connection ends; the caller reconnects.
// Errors from onRound are logged and do not stop Listen.
type Source interface {
	Listen(ctx context.Context, onRound func() error) error
}

// Open returns the event source selected by QUOTIENT_BACKEND.
func Open(cfg config.Config) (Source, error) {
	switch cfg.Backend {
	case config.BackendRedis:
		return NewRedis(), nil
	case config.BackendNATS:
		return NewNATS(), nil
	default:
		return nil, cfg.ValidateBackend()
	}
}
