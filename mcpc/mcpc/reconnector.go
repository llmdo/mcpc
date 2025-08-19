package mcpc

import (
	"context"
	"math"
	"sync/atomic"
	"time"
)

// Reconnector centralizes reconnect logic and notifications for transports (SSE/WS).
// It serializes connect attempts, sends internal disconnect notes to recvC, invokes callbacks,
// and exposes a WaitForReconnect method to allow Send to wait for the next successful reconnect.

type Reconnector struct {
	opts         *DialOptions
	reconnectedC chan struct{}
}

func NewReconnector(opts *DialOptions) *Reconnector {
	if opts == nil {
		opts = &DialOptions{}
	}
	return &Reconnector{opts: opts.WithDefaults(), reconnectedC: make(chan struct{}, 1)}
}

// Manage runs a loop that calls connectAndServe; when connectAndServe signals 'connected' (by writing to the channel),
// Manage marks alive, calls OnReconnected, and waits until connectAndServe returns (connection closed).
// On errors it sends internal-notes and invokes OnDisconnected, then waits with backoff before retrying.
func (r *Reconnector) Manage(connectAndServe func(connected chan<- struct{}) error, recvC chan<- []byte, alive *atomic.Bool, stopCh <-chan struct{}) {
	opts := r.opts
	backoff := opts.ReconnectInitialBackoff
	attempt := 1
	for {
		// observe stop signals
		select {
		case <-stopCh:
			return
		default:
		}

		connected := make(chan struct{}, 1)
		errCh := make(chan error, 1)

		go func() {
			err := connectAndServe(connected)
			errCh <- err
		}()

		var err error

		select {
		case <-connected:
			alive.Store(true)
			if opts.OnReconnected != nil {
				opts.OnReconnected()
			}
			// notify any waiter
			select {
			case r.reconnectedC <- struct{}{}:
			default:
			}

			// wait for connection closure or stop
			select {
			case err = <-errCh:
			case <-stopCh:
				// attempt graceful shutdown
				err = <-errCh
			}
		case err = <-errCh:
			// connectAndServe failed early (e.g. dial failure)
		}

		if err == nil {
			alive.Store(false)
			return
		}

		// connection failed/closed
		alive.Store(false)
		// try to notify upper layer via recvC (non-blocking)
		select {
		case recvC <- makeInternalDisconnectedNote(err):
		default:
		}
		if opts.OnDisconnected != nil {
			opts.OnDisconnected(err)
		}

		// reconnection attempt hook
		if opts.OnReconnectAttempt != nil {
			opts.OnReconnectAttempt(attempt, backoff)
		}

		// backoff wait
		select {
		case <-time.After(backoff):
		case <-stopCh:
			return
		case <-opts.CancelCtx.Done():
			return
		}

		backoff = time.Duration(math.Min(float64(opts.ReconnectMaxBackoff), float64(backoff)*1.8))
		attempt++
	}
}

// WaitForReconnect waits until the next successful reconnect or ctx.Done(). Returns true if a reconnect happened.
func (r *Reconnector) WaitForReconnect(ctx context.Context) bool {
	select {
	case <-r.reconnectedC:
		return true
	default:
	}
	select {
	case <-r.reconnectedC:
		return true
	case <-ctx.Done():
		return false
	}
}
