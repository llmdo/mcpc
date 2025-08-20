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
		AuthToken:               mcpc.EnvStr("MCP_TOKEN", ""),
		RequestTimeout:          30 * time.Second,
		PingInterval:            10 * time.Second,
		PongWait:                35 * time.Second,
		InsecureSkipVerify:      true, // demo
		Headers:                 http.Header{},
		OnDisconnected:          func(e error) { log.Printf("[transport] disconnected: %v", e) },
		OnReconnected:           func() { log.Printf("[transport] reconnected") },
		MaxRetries:              3,
		ReconnectInitialBackoff: 500 * time.Millisecond,
		ReconnectMaxBackoff:     8 * time.Second,
		//SSEEventsURL:            "https://mcp.amap.com/sse?key=",
		SSEEventsURL: "http://localhost:8888/sse", // go-mcp-file-server 本地的一个服务器
	}).WithDefaults()
	// 初始化 SSE 传输层
	transport, err := mcpc.NewSSETransport("", opts)
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
	ctx := context.Background()
	jsonb, err := client.Initialize(ctx, nil)
	if err != nil {
		log.Fatalf("call initialize failed: %v", err)
	} else {
		log.Printf("initialize result: %s\n", string(jsonb))
	}

	list, err := client.ToolsList(ctx)
	if err != nil {
		log.Fatalf("ToolsList err：%v", err)
	}

	for i, tool := range list.Tools {
		log.Printf("  %d. %s - %s", i+1, tool.Name, tool.Description)
	}

	resp, err := client.ToolsCall(context.Background(), "file_query", map[string]any{"filename": "第一序列"})
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	log.Println("pong:", string(resp))

	// 发起 RPC 请求
	// !!! sse示例的方法在ws都可以使用，Call CallBatch ToolsCall ToolsList 都是可用的方法
	//resp, err := client.ToolsCall(context.Background(), "echo", map[string]any{"message": "Hello from MCP client"})
	//if err != nil {
	//	log.Fatalf("call failed: %v", err)
	//}
	//log.Println("pong:", string(resp))

	// 发送通知
	//_ = client.Notify(context.Background(), "log", map[string]string{"msg": "hello from SSE"})
}
