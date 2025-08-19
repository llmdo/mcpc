// client.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

/* =========================
   JSON-RPC 2.0 基础类型
   ========================= */

const jsonrpcVersion = "2.0"

type RPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`     // notification 时为 nil
	Method  string           `json:"method"`           // e.g. "tools/call", "tools/list"
	Params  *json.RawMessage `json:"params,omitempty"` // 任意 JSON
}

type RPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error: code=%d message=%s", e.Code, e.Message)
}

/* =========================
   统一错误类型（区分传输层 vs 协议层）
   ========================= */

var (
	// 可用于识别：传输层已关闭或不可用（可重试）
	ErrTransportClosed = errors.New("transport closed")
)

// TransportError 用于标识传输层错误（可能可重试）
type TransportError struct {
	Op        string // 读 / 写 / 连接
	Err       error  // 原始错误
	Temporary bool   // 是否临时错误（建议重试）
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transport %s error: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

/* =========================
   配置与工具
   ========================= */

type DialOptions struct {
	// 通用
	Headers        http.Header
	AuthToken      string        // 可选：Bearer
	RequestTimeout time.Duration // 每次调用的默认超时

	// 心跳（WS）
	PingInterval time.Duration
	PongWait     time.Duration

	// 回调
	OnDisconnected func(error)
	OnReconnected  func()

	// TLS / Transport
	InsecureSkipVerify bool

	// SSE: 分离的发送与事件流地址
	SSEEventsURL string // e.g. http(s)://host/mcp/events

	// HTTPClient（可注入自定义传输/代理）
	HTTPClient *http.Client

	// 重试
	MaxRetries int

	// 重连策略
	ReconnectInitialBackoff time.Duration // e.g. 500ms
	ReconnectMaxBackoff     time.Duration // e.g. 10s
}

func (o *DialOptions) withDefaults() *DialOptions {
	cp := *o
	if cp.RequestTimeout <= 0 {
		cp.RequestTimeout = 30 * time.Second
	}
	if cp.PingInterval <= 0 {
		cp.PingInterval = 20 * time.Second
	}
	if cp.PongWait <= 0 {
		cp.PongWait = 60 * time.Second
	}
	if cp.MaxRetries <= 0 {
		cp.MaxRetries = 3
	}
	if cp.ReconnectInitialBackoff <= 0 {
		cp.ReconnectInitialBackoff = 500 * time.Millisecond
	}
	if cp.ReconnectMaxBackoff <= 0 {
		cp.ReconnectMaxBackoff = 10 * time.Second
	}
	if cp.HTTPClient == nil {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cp.InsecureSkipVerify}, // demo 用
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
		}
		cp.HTTPClient = &http.Client{Transport: tr, Timeout: cp.RequestTimeout}
	}
	if cp.Headers == nil {
		cp.Headers = make(http.Header)
	}
	if cp.AuthToken != "" && cp.Headers.Get("Authorization") == "" {
		cp.Headers.Set("Authorization", "Bearer "+cp.AuthToken)
	}
	cp.Headers.Set("Content-Type", "application/json")
	return &cp
}

/* =========================
   Transport 抽象
   ========================= */

type Transport interface {
	Send(ctx context.Context, payload []byte) error
	Recv() <-chan []byte
	Close() error
	IsConnected() bool
}

/* =========================
   内部特殊通知（断线）
   ========================= */

const internalDisconnectedMethod = "$transport/disconnected"

func makeInternalDisconnectedNote(err error) []byte {
	msg := map[string]any{
		"jsonrpc": jsonrpcVersion,
		"method":  internalDisconnectedMethod,
		"params":  map[string]any{"error": err.Error()},
	}
	b, _ := json.Marshal(msg)
	return b
}

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
		opts:         opts.withDefaults(),
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

/* =========================
   SSE Transport（含重连）
   - GET 事件流自动重连
   - POST 失败重试
   - 断线时注入内部通知
   ========================= */

type SSETransport struct {
	postURL   string
	eventsURL string
	opts      *DialOptions
	client    *http.Client

	recvC  chan []byte
	alive  atomic.Bool
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func NewSSETransport(postURL string, opts *DialOptions) (*SSETransport, error) {
	opt := opts.withDefaults()
	if opt.SSEEventsURL == "" {
		return nil, errors.New("SSE requires DialOptions.SSEEventsURL")
	}
	t := &SSETransport{
		postURL:   postURL,
		eventsURL: opt.SSEEventsURL,
		opts:      opt,
		client:    opt.HTTPClient,
		recvC:     make(chan []byte, 256),
		stopCh:    make(chan struct{}),
	}
	t.alive.Store(true)
	t.wg.Add(1)
	go t.eventLoop()
	return t, nil
}

func (t *SSETransport) eventLoop() {
	defer t.wg.Done()
	backoff := t.opts.ReconnectInitialBackoff

	for {
		select {
		case <-t.stopCh:
			close(t.recvC)
			return
		default:
		}

		req, _ := http.NewRequestWithContext(context.Background(), "GET", t.eventsURL, nil)
		req.Header = t.opts.Headers.Clone()

		resp, err := t.client.Do(req)
		if err != nil {
			// 连接失败：发内部通知，回调，然后退避重连
			select {
			case t.recvC <- makeInternalDisconnectedNote(err):
			default:
			}
			if t.opts.OnDisconnected != nil {
				t.opts.OnDisconnected(err)
			}
			time.Sleep(backoff)
			backoff = time.Duration(math.Min(float64(t.opts.ReconnectMaxBackoff), float64(backoff)*1.8))
			continue
		}

		// 成功连接
		if t.opts.OnReconnected != nil {
			t.opts.OnReconnected()
		}
		backoff = t.opts.ReconnectInitialBackoff

		func() {
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("SSE bad status: %s", resp.Status)
				select {
				case t.recvC <- makeInternalDisconnectedNote(err):
				default:
				}
				if t.opts.OnDisconnected != nil {
					t.opts.OnDisconnected(err)
				}
				return
			}

			reader := bufio.NewReader(resp.Body)
			var dataLines []string

			for {
				select {
				case <-t.stopCh:
					return
				default:
				}

				line, err := reader.ReadString('\n')
				if err != nil {
					if !errors.Is(err, io.EOF) {
						// 非 EOF 也算断线
						select {
						case t.recvC <- makeInternalDisconnectedNote(err):
						default:
						}
						if t.opts.OnDisconnected != nil {
							t.opts.OnDisconnected(err)
						}
					}
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					if len(dataLines) > 0 {
						full := strings.Join(dataLines, "\n")
						dataLines = nil
						if full != "" {
							select {
							case t.recvC <- []byte(full):
							case <-t.stopCh:
								return
							}
						}
					}
					continue
				}
				if strings.HasPrefix(line, "data:") {
					dataContent := strings.TrimSpace(line[5:])
					dataLines = append(dataLines, dataContent)
				}
			}
		}()
		// 走到这里说明需要重连
	}
}

func (t *SSETransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.postURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header = t.opts.Headers.Clone()

	var resp *http.Response
	for i := 0; i < t.opts.MaxRetries; i++ {
		resp, err = t.client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
	}
	if err != nil {
		// 发送失败：视作临时传输错误
		select {
		case t.recvC <- makeInternalDisconnectedNote(err):
		default:
		}
		if t.opts.OnDisconnected != nil {
			t.opts.OnDisconnected(err)
		}
		return &TransportError{Op: "write", Err: err, Temporary: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sse post status=%s body=%s", resp.Status, string(b))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *SSETransport) Recv() <-chan []byte { return t.recvC }

func (t *SSETransport) Close() error {
	if !t.alive.Swap(false) {
		return nil
	}
	close(t.stopCh)
	t.wg.Wait()
	return nil
}

func (t *SSETransport) IsConnected() bool { return t.alive.Load() }

/* =========================
   Stdio Transport（与原版一致，增加日志健壮性）
   ========================= */

type StdioTransport struct {
	r     *bufio.Reader
	w     *bufio.Writer
	muW   sync.Mutex
	recvC chan []byte
	alive atomic.Bool
	stop  chan struct{}
	wg    sync.WaitGroup
}

func NewStdioTransport() *StdioTransport {
	t := &StdioTransport{
		r:     bufio.NewReader(os.Stdin),
		w:     bufio.NewWriter(os.Stdout),
		recvC: make(chan []byte, 128),
		stop:  make(chan struct{}),
	}
	t.alive.Store(true)
	t.wg.Add(1)
	go t.readLoop()
	return t
}

func (t *StdioTransport) readLoop() {
	defer close(t.recvC)
	defer t.wg.Done()
	for t.alive.Load() {
		// 读头部
		length := -1
		for {
			line, err := t.r.ReadString('\n')
			if err != nil {
				t.alive.Store(false)
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break // 头结束
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("content-length:"):])
				n, _ := strconv.Atoi(v)
				length = n
			}
		}
		if length < 0 {
			log.Printf("[stdio] invalid header: missing Content-Length")
			continue
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(t.r, body); err != nil {
			t.alive.Store(false)
			return
		}
		t.recvC <- body
	}
}

func (t *StdioTransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	t.muW.Lock()
	defer t.muW.Unlock()
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := t.w.WriteString(header); err != nil {
		return err
	}
	if _, err := t.w.Write(payload); err != nil {
		return err
	}
	return t.w.Flush()
}

func (t *StdioTransport) Recv() <-chan []byte { return t.recvC }

func (t *StdioTransport) Close() error {
	if !t.alive.Swap(false) {
		return nil
	}
	close(t.stop)
	_ = t.w.Flush()
	t.wg.Wait()
	return nil
}

func (t *StdioTransport) IsConnected() bool { return t.alive.Load() }

/* =========================
   MCP Client
   - pending map: id -> chan response
   - 断线内部通知：清理所有 pending（立即失败）
   ========================= */

type NotificationHandler func(method string, params json.RawMessage)

type MCPClient struct {
	transport Transport
	opts      *DialOptions

	pendingMu sync.Mutex
	pending   map[string]chan *RPCResponse

	notifyMu sync.RWMutex
	onNotify NotificationHandler

	seq atomic.Uint64

	closed atomic.Bool
}

func NewMCPClient(t Transport, opts *DialOptions) *MCPClient {
	c := &MCPClient{
		transport: t,
		opts:      opts.withDefaults(),
		pending:   make(map[string]chan *RPCResponse),
	}
	go c.readLoop()
	return c
}

func (c *MCPClient) Close() error {
	c.closed.Store(true)
	return c.transport.Close()
}

func (c *MCPClient) IsConnected() bool { return c.transport.IsConnected() }

func (c *MCPClient) SetNotificationHandler(h NotificationHandler) {
	c.notifyMu.Lock()
	c.onNotify = h
	c.notifyMu.Unlock()
}

func (c *MCPClient) nextID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + "-" + strconv.FormatUint(c.seq.Add(1), 10)
}

func (c *MCPClient) readLoop() {
	for msg := range c.transport.Recv() {
		if c.closed.Load() {
			return
		}
		if isJSONArray(msg) {
			var arr []json.RawMessage
			if err := json.Unmarshal(msg, &arr); err != nil {
				log.Printf("batch unmarshal error: %v", err)
				continue
			}
			for _, item := range arr {
				c.dispatchOne(item)
			}
		} else {
			c.dispatchOne(json.RawMessage(msg))
		}
	}

	// Recv 关闭：说明 Transport 真正关闭，清理 pending
	c.failAllPending(&TransportError{Op: "recv", Err: ErrTransportClosed, Temporary: false})
}

func (c *MCPClient) dispatchOne(raw json.RawMessage) {
	// 可能是 Response 或 Notification
	var probe struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      *string          `json:"id,omitempty"`
		Method  string           `json:"method,omitempty"`
		Result  *json.RawMessage `json:"result,omitempty"`
		Error   *RPCError        `json:"error,omitempty"`
		Params  *json.RawMessage `json:"params,omitempty"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		log.Printf("dispatch unmarshal error: %v", err)
		return
	}
	if probe.JSONRPC != jsonrpcVersion {
		return
	}

	// 内部断线通知：清理所有 pending
	if probe.ID == nil && probe.Method == internalDisconnectedMethod {
		c.failAllPending(&TransportError{Op: "recv", Err: ErrTransportClosed, Temporary: true})
		return
	}

	if probe.ID == nil && probe.Method != "" {
		// Notification
		c.notifyMu.RLock()
		cb := c.onNotify
		c.notifyMu.RUnlock()
		if cb != nil && probe.Params != nil {
			cb(probe.Method, *probe.Params)
		}
		return
	}

	// Response
	if probe.ID == nil {
		return
	}
	resp := &RPCResponse{
		JSONRPC: probe.JSONRPC,
		ID:      probe.ID,
		Result:  probe.Result,
		Error:   probe.Error,
	}

	c.pendingMu.Lock()
	ch := c.pending[*resp.ID]
	delete(c.pending, *resp.ID)
	c.pendingMu.Unlock()

	if ch != nil {
		select {
		case ch <- resp:
		default:
			log.Printf("response channel full for id %s", *resp.ID)
		}
	}
}

func (c *MCPClient) failAllPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- &RPCResponse{
			JSONRPC: jsonrpcVersion,
			ID:      &id,
			Error: &RPCError{
				Code:    -32000, // JSON-RPC Server error 范围
				Message: err.Error(),
			},
		}:
		default:
		}
	}
	c.pending = make(map[string]chan *RPCResponse)
}

func (c *MCPClient) sendAndWait(ctx context.Context, req *RPCRequest) (*RPCResponse, error) {
	if req.JSONRPC == "" {
		req.JSONRPC = jsonrpcVersion
	}
	if req.ID != nil && *req.ID == "" {
		req.ID = nil
	}

	if req.ID == nil {
		// Notification：不等待
		bs, _ := json.Marshal(req)
		return nil, c.transport.Send(ctx, bs)
	}

	id := *req.ID
	respCh := make(chan *RPCResponse, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	// 发送
	bs, _ := json.Marshal(req)
	if err := c.transport.Send(ctx, bs); err != nil {
		// 发送失败，清理 pending
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, err
	}

	// 等待响应或超时
	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return nil, ctx.Err()
	case resp := <-respCh:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp, nil
	}
}

func (c *MCPClient) Call(ctx context.Context, method string, params any) (*RPCResponse, error) {
	id := c.nextID()
	var p *json.RawMessage
	if params != nil {
		if raw, ok := params.(json.RawMessage); ok {
			p = &raw
		} else {
			bs, _ := json.Marshal(params)
			r := json.RawMessage(bs)
			p = &r
		}
	}
	req := &RPCRequest{
		JSONRPC: jsonrpcVersion,
		ID:      &id,
		Method:  method,
		Params:  p,
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}
	return c.sendAndWait(ctx, req)
}

func (c *MCPClient) Notify(ctx context.Context, method string, params any) error {
	var p *json.RawMessage
	if params != nil {
		bs, _ := json.Marshal(params)
		r := json.RawMessage(bs)
		p = &r
	}
	req := &RPCRequest{JSONRPC: jsonrpcVersion, Method: method, Params: p}
	return c.transport.Send(ctx, mustJSON(req))
}

// CallBatch：去掉 N 个 goroutine；统一收集响应，避免 goroutine 爆炸
func (c *MCPClient) CallBatch(ctx context.Context, batch []RPCRequest) ([]RPCResponse, error) {
	for i := range batch {
		batch[i].JSONRPC = jsonrpcVersion
		if batch[i].ID == nil {
			id := c.nextID()
			batch[i].ID = &id
		}
	}
	respMap := make(map[string]chan *RPCResponse, len(batch))
	c.pendingMu.Lock()
	for i := range batch {
		ch := make(chan *RPCResponse, 1)
		respMap[*batch[i].ID] = ch
		c.pending[*batch[i].ID] = ch
	}
	c.pendingMu.Unlock()

	payload, _ := json.Marshal(batch)
	if err := c.transport.Send(ctx, payload); err != nil {
		c.pendingMu.Lock()
		for _, b := range batch {
			delete(c.pending, *b.ID)
		}
		c.pendingMu.Unlock()
		return nil, err
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout)
		defer cancel()
	}

	out := make([]RPCResponse, len(batch))
	left := len(batch)

	for left > 0 {
		// 简单轮询等待（不新增 goroutine）
		c.pendingMu.Lock()
		for i := range batch {
			id := *batch[i].ID
			if ch, ok := respMap[id]; ok {
				select {
				case rsp := <-ch:
					out[i] = *rsp
					delete(respMap, id)
					delete(c.pending, id)
					left--
				default:
				}
			}
		}
		c.pendingMu.Unlock()

		if left == 0 {
			break
		}

		select {
		case <-ctx.Done():
			// 超时：把未完成的标记为超时
			c.pendingMu.Lock()
			for i := range batch {
				id := *batch[i].ID
				if _, ok := respMap[id]; ok {
					out[i] = RPCResponse{
						JSONRPC: jsonrpcVersion,
						ID:      &id,
						Error: &RPCError{
							Code:    -1,
							Message: ctx.Err().Error(),
						},
					}
					delete(c.pending, id)
				}
			}
			c.pendingMu.Unlock()
			return out, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return out, nil
}

/* =========================
   MCP 协议方法封装
   ========================= */

type ToolInfo struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema *json.RawMessage `json:"inputSchema,omitempty"`
}

type ToolsListResult struct {
	Tools []ToolInfo `json:"tools"`
}

func (c *MCPClient) ToolsList(ctx context.Context) (*ToolsListResult, error) {
	resp, err := c.Call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out ToolsListResult
	if resp.Result == nil {
		return nil, errors.New("tools/list: empty result")
	}
	if err := json.Unmarshal(*resp.Result, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *MCPClient) ToolsCall(ctx context.Context, name string, params any) (json.RawMessage, error) {
	var payload any
	if raw, ok := params.(json.RawMessage); ok {
		payload = map[string]any{"name": name, "params": raw}
	} else {
		payload = map[string]any{"name": name, "params": params}
	}
	resp, err := c.Call(ctx, "tools/call", payload)
	if err != nil {
		return nil, err
	}
	if resp.Result == nil {
		return nil, errors.New("tools/call: empty result")
	}
	return *resp.Result, nil
}

/* =========================
   工具函数
   ========================= */

func mustJSON(v any) []byte {
	bs, _ := json.Marshal(v)
	return bs
}

func isJSONArray(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '['
}

/* =========================
   演示 main（与原版一致，默认 SSE）
   ========================= */

func main() {
	mode := envStr("MCP_MODE", "SSE") // WS | SSE | STDIO

	opts := (&DialOptions{
		AuthToken:               envStr("MCP_TOKEN", "MCP_TOKEN-12345679"),
		RequestTimeout:          1000 * time.Second,
		PingInterval:            10 * time.Second,
		PongWait:                3000 * time.Second,
		InsecureSkipVerify:      true, // demo
		Headers:                 http.Header{"X-Client": []string{"go-mcp-demo"}},
		OnDisconnected:          func(e error) { log.Printf("[transport] disconnected: %v", e) },
		OnReconnected:           func() { log.Printf("[transport] reconnected") },
		MaxRetries:              3,
		ReconnectInitialBackoff: 500 * time.Millisecond,
		ReconnectMaxBackoff:     8 * time.Second,
	}).withDefaults()

	var tr Transport
	var err error

	switch strings.ToUpper(mode) {
	case "WS":
		wsURL := envStr("MCP_WS_URL", "ws://localhost:8081/mcp/ws")
		tr, err = NewWebSocketTransport(wsURL, opts)
		if err != nil {
			log.Fatal(err)
		}
	case "SSE":
		postURL := envStr("MCP_POST_URL", "http://localhost:8080/mcp")
		eventsURL := envStr("MCP_EVENTS_URL", "http://localhost:8080/mcp/events")
		opts.SSEEventsURL = eventsURL
		tr, err = NewSSETransport(postURL, opts)
		if err != nil {
			log.Fatal(err)
		}
	case "STDIO":
		tr = NewStdioTransport()
	default:
		log.Fatalf("unknown MCP_MODE %s", mode)
	}

	client := NewMCPClient(tr, opts)
	defer client.Close()

	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		if method == internalDisconnectedMethod {
			log.Printf("[notify] transport disconnected: %s", string(params))
			return
		}
		log.Printf("[notify] %s: %s", method, string(params))
	})

	ctx := context.Background()

	// 1) 列出工具
	list, err := client.ToolsList(ctx)
	if err != nil {
		log.Fatalf("ToolsList error: %v", err)
	}
	log.Printf("Available tools (%d):", len(list.Tools))
	for i, tool := range list.Tools {
		log.Printf("  %d. %s - %s", i+1, tool.Name, tool.Description)
	}

	// 2) 调用工具（取第一个）
	if len(list.Tools) > 0 {
		first := list.Tools[0].Name
		log.Printf("Calling tool: %s", first)
		res, err := client.ToolsCall(ctx, first, map[string]any{"message": "Hello from MCP client"})
		if err != nil {
			log.Fatalf("ToolsCall error: %v", err)
		}
		log.Printf("Tool response: %s", string(res))
	}

	// 3) 批量调用
	log.Println("Sending batch request...")
	tools := []string{"echo", "reverse", "uppercase"}
	var batch []RPCRequest
	for _, tool := range tools {
		id := client.nextID()
		params := map[string]any{"message": "batch test"}
		paramsRaw, _ := json.Marshal(params)
		payload := map[string]any{"name": tool, "params": json.RawMessage(paramsRaw)}
		payloadRaw := json.RawMessage(mustJSON(payload))
		batch = append(batch, RPCRequest{
			JSONRPC: jsonrpcVersion,
			ID:      &id,
			Method:  "tools/call",
			Params:  &payloadRaw,
		})
	}
	bres, berr := client.CallBatch(ctx, batch)
	log.Printf("Batch results (error: %v):", berr)
	for _, resp := range bres {
		if resp.ID == nil {
			continue
		}
		if resp.Error != nil {
			log.Printf("  [%s] ERROR: %s", *resp.ID, resp.Error.Message)
		} else if resp.Result != nil {
			log.Printf("  [%s] RESULT: %s", *resp.ID, string(*resp.Result))
		} else {
			log.Printf("  [%s] EMPTY", *resp.ID)
		}
	}

	time.Sleep(10000 * time.Second)
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
