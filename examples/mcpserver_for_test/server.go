// server.go
// 改进版 MCP Demo Server
// - 支持 WS 和 SSE
// - 使用 JSON-RPC 2.0
// - 简单鉴权 (Bearer <token> 作为 clientID)

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

// 工具返回结构
type toolsListResult struct {
	Tools []struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	} `json:"tools"`
}

/* =========================
   工具实现
   ========================= */

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
	if params == nil {
		return rpcError(id, -32602, "Invalid params")
	}

	var p struct {
		Name   string           `json:"name"`
		Params *json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(*params, &p); err != nil {
		return rpcError(id, -32700, "Parse error")
	}

	var pp struct {
		Message string `json:"message"`
	}
	if p.Params != nil {
		_ = json.Unmarshal(*p.Params, &pp)
	}

	if pp.Message == "" {
		return rpcError(id, -32602, "missing param: message")
	}

	out := pp.Message
	switch strings.ToLower(p.Name) {
	case "echo":
	case "reverse":
		r := []rune(out)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		out = string(r)
	case "uppercase":
		out = strings.ToUpper(out)
	default:
		return rpcError(id, -32601, "Unknown tool")
	}

	b, _ := json.Marshal(map[string]any{"output": out})
	raw := json.RawMessage(b)
	return &rpcResp{JSONRPC: jsonrpcVersion, ID: id, Result: &raw}
}

func dispatchRPC(req *rpcReq) *rpcResp {
	if req.JSONRPC != jsonrpcVersion {
		return rpcError(req.ID, -32600, "Invalid Request")
	}

	switch req.Method {
	case "tools/list":
		return handleToolsList(req.ID)
	case "tools/call":
		return handleToolsCall(req.ID, req.Params)
	default:
		if req.ID == nil {
			return nil
		}
		return rpcError(req.ID, -32601, "Method not found")
	}
}

func rpcError(id *string, code int, msg string) *rpcResp {
	return &rpcResp{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &rpcErr{Code: code, Message: msg},
	}
}

/* =========================
   鉴权
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
	return parts[1], nil
}

/* =========================
   SSE
   ========================= */

type sseSubscriber struct {
	ch   chan []byte
	done chan struct{}
}

var (
	sseMu   sync.Mutex
	sseSubs = map[string]*sseSubscriber{}
)

func sseAdd(clientID string) *sseSubscriber {
	sseMu.Lock()
	defer sseMu.Unlock()
	if old, ok := sseSubs[clientID]; ok {
		safeClose(old.done)
		safeClose(old.ch)
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
		safeClose(s.done)
		safeClose(s.ch)
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
		log.Printf("SSE buffer full for client %s, dropping message", clientID)
	}
}

func sseEvents(w http.ResponseWriter, r *http.Request) {
	clientID, err := extractClientIDFromAuth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	sub := sseAdd(clientID)
	defer sseDel(clientID)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

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
				log.Printf("SSE write error: %v", err)
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-ticker.C:
			if err := writeData(map[string]any{
				"jsonrpc": jsonrpcVersion,
				"method":  "server/ping",
				"params":  map[string]any{"t": time.Now().Format(time.RFC3339), "clientID": clientID},
			}); err != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-sub.done:
			return
		}
	}
}

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

	if trim[0] == '[' {
		var reqs []rpcReq
		if err := json.Unmarshal(trim, &reqs); err != nil {
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
		sseSendTo(clientID, out)
	} else {
		var req rpcReq
		if err := json.Unmarshal(trim, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp := dispatchRPC(&req)
		out, _ := json.Marshal(resp)
		sseSendTo(clientID, out)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

/* =========================
   WebSocket
   ========================= */

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	clientID, err := extractClientIDFromAuth(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer c.Close()

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
				if err := c.WriteJSON(notify); err != nil {
					log.Printf("WS write error: %v", err)
					close(stop)
					return
				}
			case <-stop:
				return
			}
		}
	}()

	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			log.Printf("WS read error: %v", err)
			close(stop)
			return
		}
		trim := bytes.TrimSpace(msg)
		if len(trim) == 0 {
			continue
		}

		if trim[0] == '[' {
			var reqs []rpcReq
			if err := json.Unmarshal(trim, &reqs); err != nil {
				_ = c.WriteJSON(rpcError(nil, -32700, "Parse error"))
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
				_ = c.WriteJSON(rpcError(nil, -32700, "Parse error"))
				continue
			}
			resp := dispatchRPC(&req)
			_ = c.WriteJSON(resp)
		}
	}
}

/* =========================
   工具函数
   ========================= */

func safeClose[T any](ch chan T) {
	defer func() { _ = recover() }()
	close(ch)
}

/* =========================
   主函数
   ========================= */

func main() {
	http.HandleFunc("/mcp/ws", wsHandler)
	go func() {
		addrWS := ":18081"
		log.Printf("WS server on %s", addrWS)
		log.Fatal(http.ListenAndServe(addrWS, nil))
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/events", sseEvents)
	mux.HandleFunc("/mcp", ssePost)

	addr := ":18080"
	log.Printf("HTTP(SSE) server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
