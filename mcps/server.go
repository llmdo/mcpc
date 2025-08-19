// server.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"io"

	"github.com/gorilla/websocket"
)

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

/* ===== 工具实现 ===== */

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
			{"echo", "Echo message"},
			{"reverse", "Reverse message"},
			{"uppercase", "Uppercase message"},
		},
	}
	b, _ := json.Marshal(result)
	raw := json.RawMessage(b)
	return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Result: &raw}
}

func handleToolsCall(id *string, params *json.RawMessage) *rpcResp {
	// params: {"name": "...", "params": {"message": "..."}}
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
		// no-op
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

/* ===== WebSocket ===== */

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer c.Close()

	// 周期通知
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
					"params":  map[string]any{"t": time.Now().Format(time.RFC3339)},
				}
				_ = c.WriteJSON(notify)
			case <-stop:
				return
			}
		}
	}()

	for {
		var raw json.RawMessage
		err := c.ReadJSON(&raw)
		if err != nil {
			close(stop)
			log.Println("ws read:", err)
			return
		}

		trim := bytes.TrimSpace([]byte(raw))
		if len(trim) == 0 {
			continue
		}

		if trim[0] == '[' {
			// 批量
			var reqs []rpcReq
			if err := json.Unmarshal(trim, &reqs); err != nil {
				continue
			}
			var resps []rpcResp
			for i := range reqs {
				resps = append(resps, *dispatchRPC(&reqs[i]))
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

func dispatchRPC(req *rpcReq) *rpcResp {
	switch req.Method {
	case "tools/list":
		return handleToolsList(req.ID)
	case "tools/call":
		return handleToolsCall(req.ID, req.Params)
	default:
		// notification / unknown
		if req.ID == nil {
			// 忽略通知
			return nil
		}
		return &rpcResp{JSONRPC: jsonrpcVersion, ID: req.ID, Error: &rpcErr{Code: -32601, Message: "Method not found"}}
	}
}

/* ===== SSE（极简实现：所有订阅者共享广播）===== */

type subscriber struct {
	ch   chan []byte
	done chan struct{}
}

var (
	subsMu sync.Mutex
	subs   = map[*subscriber]struct{}{}
)

func addSub() *subscriber {
	s := &subscriber{ch: make(chan []byte, 256), done: make(chan struct{})}
	subsMu.Lock()
	subs[s] = struct{}{}
	subsMu.Unlock()
	return s
}
func delSub(s *subscriber) {
	subsMu.Lock()
	delete(subs, s)
	close(s.ch)
	close(s.done)
	subsMu.Unlock()
}
func broadcast(b []byte) {
	subsMu.Lock()
	for s := range subs {
		select {
		case s.ch <- b:
		default:
		}
	}
	subsMu.Unlock()
}

func sseEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	s := addSub()
	defer delSub(s)

	// 周期通知
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	notify := func(msg any) {
		b, _ := json.Marshal(msg)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	for {
		select {
		case b := <-s.ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-ticker.C:
			notify(map[string]any{
				"jsonrpc": jsonrpcVersion,
				"method":  "server/ping",
				"params":  map[string]any{"t": time.Now().Format(time.RFC3339)},
			})
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		}
	}
}

// POST /mcp：接收 JSON-RPC（单个或批量），处理后广播响应（所有订阅者都能收到）
// 仅用于 demo，真实生产应按 client 维度路由响应
func ssePost(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	b, _ := ioReadAll(r.Body)
	trim := bytes.TrimSpace(b)
	if len(trim) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if trim[0] == '[' {
		var reqs []rpcReq
		if json.Unmarshal(trim, &reqs) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var resps []rpcResp
		for i := range reqs {
			resp := dispatchRPC(&reqs[i])
			if resp != nil {
				resps = append(resps, *resp)
			}
		}
		out, _ := json.Marshal(resps)
		broadcast(out)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
		return
	}

	var req rpcReq
	if json.Unmarshal(trim, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	resp := dispatchRPC(&req)
	out, _ := json.Marshal(resp)
	broadcast(out)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func ioReadAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

func main() {
	// WS
	http.HandleFunc("/mcp/ws", wsHandler)
	// SSE
	http.HandleFunc("/mcp/events", sseEvents)
	http.HandleFunc("/mcp", ssePost)

	addr := ":8080"
	addrWS := ":8081"

	go func() {
		log.Printf("WS server on %s", addrWS)
        // 独立 WS 端口
		log.Fatal(http.ListenAndServe(addrWS, nil))
	}()

	log.Printf("HTTP(SSE) server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
