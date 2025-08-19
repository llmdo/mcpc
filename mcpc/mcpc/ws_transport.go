package mcpc

import (
	"context"
	"crypto/tls"
	"github.com/gorilla/websocket"
	"math"
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

	conn    *websocket.Conn
	muW     sync.Mutex
	recvC   chan []byte
	alive   atomic.Bool
	closed  atomic.Bool
	closeMu sync.Mutex
	once    sync.Once

	// 控制
	stopCh       chan struct{}
	reconnectedC chan struct{}
	wg           sync.WaitGroup
}

func NewWebSocketTransport(urlStr string, opts *DialOptions) (*WebSocketTransport, error) {
	t := &WebSocketTransport{
		url:          urlStr,
		opts:         opts.WithDefaults(),
		recvC:        make(chan []byte, 256),
		stopCh:       make(chan struct{}),
		reconnectedC: make(chan struct{}, 1),
	}
	if err := t.connect(); err != nil {
		return nil, err
	}
	t.wg.Add(1)
	go t.loop()
	return t, nil
}

func (t *WebSocketTransport) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout:  15 * time.Second,
		EnableCompression: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: t.opts.InsecureSkipVerify},
	}
	conn, _, err := dialer.Dial(t.url, t.opts.Headers)
	if err != nil {
		return err
	}
	t.conn = conn
	t.alive.Store(true)

	_ = t.conn.SetReadDeadline(time.Now().Add(t.opts.PongWait))
	t.conn.SetPongHandler(func(string) error {
		_ = t.conn.SetReadDeadline(time.Now().Add(t.opts.PongWait))
		return nil
	})
	return nil
}

func (t *WebSocketTransport) loop() {
	defer t.wg.Done()

	// 心跳
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		ticker := time.NewTicker(t.opts.PingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !t.alive.Load() {
					continue
				}
				t.muW.Lock()
				err := t.conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				t.muW.Unlock()
				if err != nil {
					t.onDisconnect(&TransportError{Op: "ping", Err: err, Temporary: true})
				}
			case <-t.stopCh:
				return
			}
		}
	}()

	// 读
	for {
		if !t.alive.Load() {
			// 阻塞等待重连或关闭
			select {
			case <-t.reconnectedC:
				// 重连成功，继续读
			case <-t.stopCh:
				close(t.recvC)
				return
			}
		}

		_, msg, err := t.conn.ReadMessage()
		if err != nil {
			t.onDisconnect(&TransportError{Op: "read", Err: err, Temporary: true})
			continue
		}
		select {
		case t.recvC <- msg:
		case <-t.stopCh:
			close(t.recvC)
			return
		}
	}
}

func (t *WebSocketTransport) onDisconnect(cause error) {
	if !t.alive.Swap(false) {
		return
	}
	// 关闭旧连接
	if t.conn != nil {
		_ = t.conn.Close()
	}
	// 通知上层（内部通知 + 回调）
	select {
	case t.recvC <- makeInternalDisconnectedNote(cause):
	default:
	}
	if t.opts.OnDisconnected != nil {
		t.opts.OnDisconnected(cause)
	}

	// 重连
	go t.reconnectLoop()
}

func (t *WebSocketTransport) reconnectLoop() {
	backoff := t.opts.ReconnectInitialBackoff
	for {
		select {
		case <-t.stopCh:
			return
		default:
		}

		err := t.connect()
		if err == nil {
			// 成功
			if t.opts.OnReconnected != nil {
				t.opts.OnReconnected()
			}
			t.alive.Store(true)
			select { // 非阻塞唤醒读循环
			case t.reconnectedC <- struct{}{}:
			default:
			}
			return
		}
		// 退避
		time.Sleep(backoff)
		backoff = time.Duration(math.Min(float64(t.opts.ReconnectMaxBackoff), float64(backoff)*1.8))
	}
}

func (t *WebSocketTransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	t.muW.Lock()
	defer t.muW.Unlock()

	deadline, ok := ctx.Deadline()
	if ok {
		_ = t.conn.SetWriteDeadline(deadline)
	} else {
		_ = t.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	}
	if err := t.conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		// 写失败也触发断开并进入重连
		t.onDisconnect(err)
		return &TransportError{Op: "write", Err: err, Temporary: true}
	}
	return nil
}

func (t *WebSocketTransport) Recv() <-chan []byte { return t.recvC }

func (t *WebSocketTransport) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	if t.closed.Load() {
		return nil
	}
	t.closed.Store(true)
	close(t.stopCh)
	if t.conn != nil {
		_ = t.conn.Close()
	}
	t.wg.Wait()
	return nil
}

func (t *WebSocketTransport) IsConnected() bool { return t.alive.Load() }
