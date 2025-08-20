// server.go
// 改进版 MCP Demo Server
// - 支持 WS 和 SSE
// - 使用 JSON-RPC 2.0
// - 简单鉴权 (Bearer <token> 作为 clientID)

package main

import (
	"github.com/llmdo/mcpc/examples/mcpserver_for_test/ssews"
	"github.com/llmdo/mcpc/examples/mcpserver_for_test/stdio"
	"log"

	"net/http"
	"os"
	"strings"
)

/* =========================
   主函数
   ========================= */

func main() {
	mode := ""
	// 切换mode来测试不同的客户端
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}

	// 此处 为演示，应该在客户端中直接调用
	if strings.ToUpper(mode) == "STDIO" {
		stdio.StdioLoop()
		return
	}

	http.HandleFunc("/mcp/ws", ssews.WsHandler)
	go func() {
		addrWS := ":18081"
		log.Printf("WS server on %s", addrWS)
		log.Fatal(http.ListenAndServe(addrWS, nil))
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/events", ssews.SseEvents)
	mux.HandleFunc("/mcp", ssews.SsePost)

	addr := ":18080"
	log.Printf("HTTP(SSE) server on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func EnvStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
