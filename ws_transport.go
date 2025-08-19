package mcpc

import (
	"context"
	"crypto/tls"
	"github.com/gorilla/websocket"
	"sync"
	"sync/atomic"
	"time"
)

/* =========================
   WebSocket Transport
   - 断线自动重连（指数退避）
   - 断开时向上游发内部通知，便于 pending 清理
   ========================= */

type WebSocketTransport struct {
	url  string
	opts *DialOptions

	conn   *websocket.Conn
	muW    sync.Mutex
	recvC  chan []byte
	alive  atomic.Bool
	stopCh chan struct{}
	wg     sync.WaitGroup

	reconnector *Reconnector
}

func NewWebSocketTransport(urlStr string, opts *DialOptions) (*WebSocketTransport, error) {
	opt := opts.WithDefaults()
	t := &WebSocketTransport{
		url:         urlStr,
		opts:        opt,
		recvC:       make(chan []byte, 256),
		stopCh:      make(chan struct{}),
		reconnector: NewReconnector(opt),
	}
	// start manager
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.reconnector.Manage(t.connectAndServe, t.recvC, &t.alive, t.stopCh)
	}()
	return t, nil
}

func (t *WebSocketTransport) connectAndServe(connected chan<- struct{}) error {
	opt := t.opts
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: opt.InsecureSkipVerify},
	}
	conn, _, err := dialer.Dial(t.url, opt.Headers)
	if err != nil {
		return err
	}
	t.conn = conn

	_ = t.conn.SetReadDeadline(time.Now().Add(opt.PongWait))
	t.conn.SetPongHandler(func(string) error {
		_ = t.conn.SetReadDeadline(time.Now().Add(opt.PongWait))
		return nil
	})

	select {
	case connected <- struct{}{}:
	default:
	}

	// ping goroutine
	pingStop := make(chan struct{})
	var pingWg sync.WaitGroup
	pingWg.Add(1)
	go func() {
		defer pingWg.Done()
		ticker := time.NewTicker(opt.PingInterval)
		defer ticker.Stop()
		consecutiveFailures := 0
		threshold := opt.PingFailureThreshold
		if threshold <= 0 {
			threshold = 3
		}
		for {
			select {
			case <-ticker.C:
				t.muW.Lock()
				err := t.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				t.muW.Unlock()
				if err != nil {
					consecutiveFailures++
					if consecutiveFailures >= threshold {
						_ = t.conn.Close()
						return
					}
				} else {
					consecutiveFailures = 0
				}
			case <-pingStop:
				return
			case <-t.stopCh:
				return
			case <-opt.CancelCtx.Done():
				return
			}
		}
	}()

	// stop monitor
	go func() {
		select {
		case <-t.stopCh:
			_ = t.conn.Close()
		case <-opt.CancelCtx.Done():
			_ = t.conn.Close()
		}
	}()

	for {
		_, msg, err := t.conn.ReadMessage()
		if err != nil {
			close(pingStop)
			pingWg.Wait()
			_ = t.conn.Close()
			t.conn = nil
			return err
		}
		if opt.OnMessage != nil {
			opt.OnMessage(msg)
		}
		select {
		case t.recvC <- msg:
		case <-t.stopCh:
			close(pingStop)
			pingWg.Wait()
			return nil
		}
	}
}

func (t *WebSocketTransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		// wait for reconnect (controlled by caller ctx)
		if !t.reconnector.WaitForReconnect(ctx) {
			return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
		}
	}

	t.muW.Lock()
	defer t.muW.Unlock()
	if t.conn == nil {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = t.conn.SetWriteDeadline(dl)
	} else {
		_ = t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	if err := t.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		_ = t.conn.Close()
		return &TransportError{Op: "write", Err: err, Temporary: true}
	}
	return nil
}

func (t *WebSocketTransport) Recv() <-chan []byte { return t.recvC }

func (t *WebSocketTransport) Close() error {
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	t.wg.Wait()
	if t.conn != nil {
		_ = t.conn.Close()
		t.conn = nil
	}
	select {
	default:
		close(t.recvC)
	}
	return nil
}

func (t *WebSocketTransport) IsConnected() bool { return t.alive.Load() }
