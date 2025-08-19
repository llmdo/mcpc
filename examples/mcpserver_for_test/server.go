// server.go
// 功能：JSON-RPC 2.0 + MCP Demo 服务端
// - 支持 WebSocket:   ws://localhost:8081/mcp/ws
// - 支持 SSE：        POST http://localhost:8080/mcp  +  GET http://localhost:8080/mcp/events
// - 鉴权与路由：使用 "Authorization: Bearer <token>" 作为 clientID
// - 订阅者分区：SSE 每个 token 一个订阅者通道；WS 每个连接绑定一个 token
//
// 注意：为简化 Demo，这里不做持久化与复杂鉴权逻辑。
//      生产中请替换为真正的鉴权、连接管理与响应路由。

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

/* =========================
   JSON-RPC 基本类型
   ========================= */

const jsonrpcVersion = "2.0"

type rpcReq struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
}
type rpcResp struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *rpcErr          `json:"error,omitempty"`
}
type rpcErr struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

/* =========================
   简单工具实现（echo / reverse / uppercase）
   ========================= */

type toolsListResult struct {
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	} `json:"tools"`
}

func handleToolsList(id *string) *rpcResp {
	result := toolsListResult{
		Tools: []struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
		}{
			{"echo", "返回原文"},
			{"reverse", "反转字符串"},
			{"uppercase", "转大写"},
		},
	}
	b, _ := json.Marshal(result)
	raw := json.RawMessage(b)
	return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Result: &raw}
}

func handleToolsCall(id *string, params *json.RawMessage) *rpcResp {
	// 期望 params 形如：{"name":"echo","params":{"message":"..."}}
	var p struct {
		Name   string           `json:"name"`
		Params *json.RawMessage `json:"params"`
	}
	if params == nil || json.Unmarshal(*params, &p) != nil {
		return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcErr{Code: -32602, Message: "Invalid params"}}
	}
	var pp struct {
		Message string `json:"message"`
	}
	if p.Params != nil {
		_ = json.Unmarshal(*p.Params, &pp)
	}

	out := pp.Message
	switch strings.ToLower(p.Name) {
	case "echo":
		// 不变
	case "reverse":
		r := []rune(out)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		out = string(r)
	case "uppercase":
		out = strings.ToUpper(out)
	default:
		return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcErr{Code: -32601, Message: "Unknown tool"}}
	}
	b, _ := json.Marshal(map[string]any{"output": out})
	raw := json.RawMessage(b)
	return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Result: &raw}
}

func dispatchRPC(req *rpcReq) *rpcResp {
	switch req.Method {
	case "tools/list":
		return handleToolsList(req.ID)
	case "tools/call":
		return handleToolsCall(req.ID, req.Params)
	default:
		// notification 或未知方法
		if req.ID == nil {
			return nil
		}
		return &rpcResp{JSONRPC: jsonrpcVersion, ID: req.ID, Error: &rpcErr{Code: -32601, Message: "Method not found"}}
	}
}

/* =========================
   鉴权与 ClientID
   ========================= */

func extractClientIDFromAuth(r *http.Request) (string, error) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", errors.New("missing Authorization")
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", errors.New("invalid Authorization header")
	}
	return parts[1], nil // 直接把 token 作为 clientID（demo）
}

/* =========================
   SSE 实现（按 clientID 分区）
   ========================= */

type sseSubscriber struct {
	ch   chan []byte
	done chan struct{}
}

var (
	sseMu   sync.Mutex
	sseSubs = map[string]*sseSubscriber{} // clientID -> subscriber
)

func sseAdd(clientID string) *sseSubscriber {
	sseMu.Lock()
	defer sseMu.Unlock()
	// 若已存在旧订阅者，先关闭并替换（支持单客户端重连）
	if old, ok := sseSubs[clientID]; ok {
		close(old.done)
		close(old.ch)
	}
	s := &sseSubscriber{
		ch:   make(chan []byte, 256),
		done: make(chan struct{}),
	}
	sseSubs[clientID] = s
	return s
}
func sseDel(clientID string) {
	sseMu.Lock()
	defer sseMu.Unlock()
	if s, ok := sseSubs[clientID]; ok {
		delete(sseSubs, clientID)
		close(s.done)
		close(s.ch)
	}
}
func sseSendTo(clientID string, b []byte) {
	sseMu.Lock()
	s, ok := sseSubs[clientID]
	sseMu.Unlock()
	if !ok {
		return
	}
	select {
	case s.ch <- b:
	default:
	}
}

func sseEvents(w http.ResponseWriter, r *http.Request) {
	// 1) 鉴权：取 clientID
	clientID, err := extractClientIDFromAuth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// 2) 建立订阅
	sub := sseAdd(clientID)
	defer sseDel(clientID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// 每个客户端独立的心跳/通知
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	writeData := func(payload any) error {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return nil
	}

	for {
		select {
		case b := <-sub.ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-ticker.C:
			_ = writeData(map[string]any{
				"jsonrpc": jsonrpcVersion,
				"method":  "server/ping",
				"params":  map[string]any{"t": time.Now().Format(time.RFC3339), "clientID": clientID},
			})
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		}
	}
}

// POST /mcp：接收 JSON-RPC（单个或批量），处理后只投递给“同一 clientID”订阅者
func ssePost(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	clientID, err := extractClientIDFromAuth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	b, _ := io.ReadAll(r.Body)
	trim := bytes.TrimSpace(b)
	if len(trim) == 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// 解析并处理
	if trim[0] == '[' {
		var reqs []rpcReq
		if json.Unmarshal(trim, &reqs) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var resps []rpcResp
		for i := range reqs {
			if resp := dispatchRPC(&reqs[i]); resp != nil {
				resps = append(resps, *resp)
			}
		}
		out, _ := json.Marshal(resps)
		// 精准路由：只发给该 clientID
		sseSendTo(clientID, out)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	var req rpcReq
	if json.Unmarshal(trim, &req) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp := dispatchRPC(&req)
	out, _ := json.Marshal(resp)
	sseSendTo(clientID, out) // 精准路由
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

/* =========================
   WebSocket 实现（会话级鉴权与路由）
   - 每条连接绑定一个 clientID
   - 请求从该连接来，响应也回该连接
   ========================= */

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true }, // Demo 放宽跨域
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	// 1) 鉴权
	clientID, err := extractClientIDFromAuth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// 2) 升级连接
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer c.Close()

	// 3) 周期通知（仅对该连接/会话）
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				notify := map[string]any{
					"jsonrpc": jsonrpcVersion,
					"method":  "server/ping",
					"params":  map[string]any{"t": time.Now().Format(time.RFC3339), "clientID": clientID},
				}
				_ = c.WriteJSON(notify)
			case <-stop:
				return
			}
		}
	}()

	// 4) 处理来自该会话的请求并直接回写
	for {
		var raw json.RawMessage
		if err := c.ReadJSON(&raw); err != nil {
			close(stop)
			log.Println("ws read:", err)
			return
		}
		trim := bytes.TrimSpace([]byte(raw))
		if len(trim) == 0 {
			continue
		}

		if trim[0] == '[' {
			var reqs []rpcReq
			if err := json.Unmarshal(trim, &reqs); err != nil {
				continue
			}
			var resps []rpcResp
			for i := range reqs {
				if resp := dispatchRPC(&reqs[i]); resp != nil {
					resps = append(resps, *resp)
				}
			}
			_ = c.WriteJSON(resps)
		} else {
			var req rpcReq
			if err := json.Unmarshal(trim, &req); err != nil {
				continue
			}
			resp := dispatchRPC(&req)
			_ = c.WriteJSON(resp)
		}
	}
}

/* =========================
   主函数：同时启动 WS 与 HTTP(SSE)
   ========================= */

func main() {
	// WS（独立端口）
	http.HandleFunc("/mcp/ws", wsHandler)
	go func() {
		addrWS := ":18081"
		log.Printf("WS server on %s", addrWS)
		log.Fatal(http.ListenAndServe(addrWS, nil))
	}()

	// SSE（HTTP）
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/events", sseEvents)
	mux.HandleFunc("/mcp", ssePost)

	addr := ":18080"
	log.Printf("HTTP(SSE) server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
