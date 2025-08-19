package main

import (
	"context"
	"encoding/json"
	"log"
	"mcpc/mcpc"
	"net/http"
	"strings"
	"time"
)

func main() {
	mode := mcpc.EnvStr("MCP_MODE", "SSE") // WS | SSE | STDIO

	opts := (&mcpc.DialOptions{
		AuthToken:               mcpc.EnvStr("MCP_TOKEN", "MCP_TOKEN-12345679"),
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
	}).WithDefaults()

	var tr mcpc.Transport
	var err error

	switch strings.ToUpper(mode) {
	case "WS":
		wsURL := mcpc.EnvStr("MCP_WS_URL", "ws://localhost:8081/mcp/ws")
		tr, err = mcpc.NewWebSocketTransport(wsURL, opts)
		if err != nil {
			log.Fatal(err)
		}
	case "SSE":
		postURL := mcpc.EnvStr("MCP_POST_URL", "http://localhost:8080/mcp")
		//eventsURL := mcpc.EnvStr("MCP_EVENTS_URL", "http://localhost:8080/mcp/events")
		//opts.SSEEventsURL = eventsURL
		tr, err = mcpc.NewSSETransport(postURL, opts)
		if err != nil {
			log.Fatal(err)
		}
	case "STDIO":
		tr = mcpc.NewStdioTransport()
	default:
		log.Fatalf("unknown MCP_MODE %s", mode)
	}

	client := mcpc.NewMCPClient(tr, opts)
	defer client.Close()

	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		if method == mcpc.InternalDisconnectedMethod {
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

}
