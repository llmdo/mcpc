package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/llmdo/mcpc"
)

func main() {
	opts := (&mcpc.DialOptions{
		AuthToken:               mcpc.EnvStr("MCP_TOKEN", "MCP_TOKEN-123456789"),
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
	// 初始化 WS 传输层
	transport, err := mcpc.NewWebSocketTransport("ws://localhost:8080/mcp", opts)
	if err != nil {
		log.Fatalf("new sse transport: %v", err)
	}

	hooks := &mcpc.ClientHooks{
		OnSend: func(id, method string) {
			log.Printf("[send] id=%s method=%s", id, method)
		},
		OnResponse: func(id string, err *mcpc.RPCError) {
			log.Printf("[resp] id=%s err=%v", id, err)
		},
		OnDisconnect: func(temp bool) {
			log.Printf("[disconnect] temporary=%v", temp)
		},
	}

	client := mcpc.NewMCPClient(transport, &mcpc.DialOptions{
		RequestTimeout: 5 * time.Second,
	}, hooks)
	defer client.Close()

	// RPC 调用
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := client.Call(ctx, "echo", map[string]string{"msg": "hello ws"})
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	fmt.Println("echo resp:", string(*resp.Result))

	Pa := json.RawMessage(mcpc.MustJSON(map[string]string{"msg": "batch"}))
	// 批量调用
	batch := []mcpc.RPCRequest{
		{Method: "ping"},
		{Method: "echo", Params: &Pa},
	}
	resps, _ := client.CallBatch(context.Background(), batch)
	for _, r := range resps {
		fmt.Println("batch resp:", r)
	}
}
