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
		opts:      opts.WithDefaults(),
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
	if probe.ID == nil && probe.Method == InternalDisconnectedMethod {
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
