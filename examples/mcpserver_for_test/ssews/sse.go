package ssews

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mcpserver_for_test/mcps"
	"net/http"
	"sync"
	"time"
)

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

func SseEvents(w http.ResponseWriter, r *http.Request) {
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
				"jsonrpc": mcps.JsonrpcVersion,
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

func SsePost(w http.ResponseWriter, r *http.Request) {
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
		var reqs []mcps.RpcReq
		if err := json.Unmarshal(trim, &reqs); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var resps []mcps.RpcResp
		for i := range reqs {
			if resp := mcps.DispatchRPC(&reqs[i]); resp != nil {
				resps = append(resps, *resp)
			}
		}
		out, _ := json.Marshal(resps)
		sseSendTo(clientID, out)
	} else {
		var req mcps.RpcReq
		if err := json.Unmarshal(trim, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		resp := mcps.DispatchRPC(&req)
		out, _ := json.Marshal(resp)
		sseSendTo(clientID, out)
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
