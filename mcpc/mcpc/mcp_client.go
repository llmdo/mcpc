package mcpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

/* =========================
   MCP Client
   - pending map: id -> chan response
   - 断线内部通知：清理所有 pending（立即失败）
   - 新增改进：
     1. readLoop 在 Close 后也会清理 pending
     2. CallBatch 用 ticker 替代 time.After
     3. 定义客户端内部错误码 (-32099)
     4. Close 等待读 goroutine 退出，避免泄漏
     5. 预留 Debug hooks
   ========================= */

const (
	ClientTransportClosedCode = -32099 // 客户端内部：传输关闭
)

type NotificationHandler func(method string, params json.RawMessage)

type ClientHooks struct {
	OnSend       func(id, method string)
	OnResponse   func(id string, err *RPCError)
	OnNotify     func(method string)
	OnDisconnect func(temporary bool)
}

type MCPClient struct {
	transport Transport
	opts      *DialOptions

	pendingMu sync.Mutex
	pending   map[string]chan *RPCResponse

	notifyMu sync.RWMutex
	onNotify NotificationHandler

	seq atomic.Uint64

	closed atomic.Bool

	hooks *ClientHooks

	wg sync.WaitGroup // 用于等待 readLoop 完成
}

func NewMCPClient(t Transport, opts *DialOptions, hooks *ClientHooks) *MCPClient {
	c := &MCPClient{
		transport: t,
		opts:      opts.WithDefaults(),
		pending:   make(map[string]chan *RPCResponse),
		hooks:     hooks,
	}
	c.wg.Add(1)
	go c.readLoop()
	return c
}

func (c *MCPClient) Close() error {
	c.closed.Store(true)
	err := c.transport.Close()
	// 等待读 goroutine 完全退出，避免泄漏
	c.wg.Wait()
	return err
}

func (c *MCPClient) IsConnected() bool { return c.transport.IsConnected() }

func (c *MCPClient) SetNotificationHandler(h NotificationHandler) {
	c.notifyMu.Lock()
	c.onNotify = h
	c.notifyMu.Unlock()
}

func (c *MCPClient) NextID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + "-" + strconv.FormatUint(c.seq.Add(1), 10)
}

func (c *MCPClient) readLoop() {
	defer c.wg.Done()
	for msg := range c.transport.Recv() {
		if c.closed.Load() {
			break
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

	// Transport Recv 关闭时，清理所有 pending
	c.failAllPending(&TransportError{Op: "recv", Err: ErrTransportClosed, Temporary: false})

	// 调用 Hook
	if c.hooks != nil && c.hooks.OnDisconnect != nil {
		c.hooks.OnDisconnect(false)
	}
}

func (c *MCPClient) dispatchOne(raw json.RawMessage) {
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
	if probe.JSONRPC != JsonrpcVersion {
		return
	}

	// 内部断线通知：清理所有 pending
	if probe.ID == nil && probe.Method == InternalDisconnectedMethod {
		c.failAllPending(&TransportError{Op: "recv", Err: ErrTransportClosed, Temporary: true})
		if c.hooks != nil && c.hooks.OnDisconnect != nil {
			c.hooks.OnDisconnect(true)
		}
		return
	}

	// Notification
	if probe.ID == nil && probe.Method != "" {
		c.notifyMu.RLock()
		cb := c.onNotify
		c.notifyMu.RUnlock()
		if cb != nil && probe.Params != nil {
			cb(probe.Method, *probe.Params)
		}
		if c.hooks != nil && c.hooks.OnNotify != nil {
			c.hooks.OnNotify(probe.Method)
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

	if c.hooks != nil && c.hooks.OnResponse != nil {
		c.hooks.OnResponse(*resp.ID, resp.Error)
	}
}

func (c *MCPClient) failAllPending(err error) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for id, ch := range c.pending {
		select {
		case ch <- &RPCResponse{
			JSONRPC: JsonrpcVersion,
			ID:      &id,
			Error: &RPCError{
				Code:    ClientTransportClosedCode,
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
		req.JSONRPC = JsonrpcVersion
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

	if c.hooks != nil && c.hooks.OnSend != nil {
		c.hooks.OnSend(id, req.Method)
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
	id := c.NextID()
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
		JSONRPC: JsonrpcVersion,
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
	req := &RPCRequest{JSONRPC: JsonrpcVersion, Method: method, Params: p}
	return c.transport.Send(ctx, MustJSON(req))
}

// CallBatch：使用轮询等待响应，避免 goroutine 爆炸
func (c *MCPClient) CallBatch(ctx context.Context, batch []RPCRequest) ([]RPCResponse, error) {
	for i := range batch {
		batch[i].JSONRPC = JsonrpcVersion
		if batch[i].ID == nil {
			id := c.NextID()
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

	ticker := time.NewTicker(13 * time.Millisecond) // 改进：替代 time.After
	defer ticker.Stop()

	for left > 0 {
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
						JSONRPC: JsonrpcVersion,
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
		case <-ticker.C:
		}
	}
	return out, nil
}
