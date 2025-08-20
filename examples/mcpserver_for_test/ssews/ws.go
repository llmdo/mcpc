package ssews

import (
	"bytes"
	"encoding/json"
	"github.com/gorilla/websocket"
	"log"
	"mcpserver_for_test/mcps"
	"net/http"
	"time"
)

/* =========================
   WebSocket
   ========================= */

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func WsHandler(w http.ResponseWriter, r *http.Request) {
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
					"jsonrpc": mcps.JsonrpcVersion,
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
			var reqs []mcps.RpcReq
			if err := json.Unmarshal(trim, &reqs); err != nil {
				_ = c.WriteJSON(mcps.RpcError(nil, -32700, "Parse error"))
				continue
			}
			var resps []mcps.RpcResp
			for i := range reqs {
				if resp := mcps.DispatchRPC(&reqs[i]); resp != nil {
					resps = append(resps, *resp)
				}
			}
			_ = c.WriteJSON(resps)
		} else {
			var req mcps.RpcReq
			if err := json.Unmarshal(trim, &req); err != nil {
				_ = c.WriteJSON(mcps.RpcError(nil, -32700, "Parse error"))
				continue
			}
			resp := mcps.DispatchRPC(&req)
			_ = c.WriteJSON(resp)
		}
	}
}
