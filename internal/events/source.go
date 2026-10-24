// Package events delivers scoring-round notifications to network reconfiguration.
package events

import "context"

// Source listens for one closed round at a time.
// Listen returns when the connection ends; the caller reconnects.
// Errors from onRound are logged and do not stop Listen.
type Source interface {
	Listen(ctx context.Context, onRound func() error) error
}
