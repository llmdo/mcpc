package main

import (
	"context"
	"encoding/json"
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
		SSEEventsURL:            "http://localhost:18080/mcp/events",
	}).WithDefaults()
	// 初始化 SSE 传输层
	transport, err := mcpc.NewSSETransport("http://localhost:18080/mcp", opts)
	if err != nil {
		log.Fatalf("new sse transport: %v", err)
	}

	// 设置 Hook
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

	// 创建客户端
	client := mcpc.NewMCPClient(transport, opts, hooks)
	defer client.Close()

	// 注册通知回调
	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		log.Printf("[notify] method=%s params=%s", method, string(params))
	})

	rpcResponse, err := client.Call(context.Background(), "initialize", map[string]any{"client": "client 123"})
	if err != nil {
		log.Fatalf("call initialize failed: %v", err)
	} else {
		log.Printf("initialize result: %s\n", string(*rpcResponse.Result))
	}

	// 发起 RPC 请求
	// !!! sse示例的方法在ws都可以使用，Call CallBatch ToolsCall ToolsList 都是可用的方法
	resp, err := client.ToolsCall(context.Background(), "echo", map[string]any{"message": "Hello from MCP client"})
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	log.Println("pong:", string(resp))

	// 发送通知
	_ = client.Notify(context.Background(), "log", map[string]string{"msg": "hello from SSE"})
}
