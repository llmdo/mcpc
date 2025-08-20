package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/llmdo/mcpc"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal(err)
	} else {
		fmt.Println(exePath)
	}

	opts := (&mcpc.DialOptions{
		Headers:        http.Header{"X-Client": []string{"go-mcp-demo"}},
		OnDisconnected: func(e error) { log.Printf("[transport] disconnected: %v", e) },
		OnReconnected:  func() { log.Printf("[transport] reconnected") },
	}).WithDefaults()
	// 初始化 SSE 传输层
	transport, err := mcpc.NewStdioSubprocess(filepath.Join(filepath.Dir(exePath), "../mcpserver_for_test/mcpserver_for_test.exe"), []string{"stdio"}, opts)
	if err != nil {
		log.Fatal(err)
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

	// 发起 RPC 请求
	// !!! sse示例的方法在ws都可以使用，Call CallBatch ToolsCall ToolsList 都是可用的方法
	resp, err := client.ToolsCall(context.Background(), "echo", map[string]any{"message": "Hello from MCP client"})
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	fmt.Println("pong:", string(resp))

	// 发送通知
	_ = client.Notify(context.Background(), "log", map[string]string{"msg": "hello from SSE"})

	time.Sleep(3 * time.Second)

	resp, err = client.ToolsCall(context.Background(), "uppercase", map[string]any{"message": "Hello from MCP client"})
	if err != nil {
		log.Fatalf("call failed: %v", err)
	}
	fmt.Println("pong:", string(resp))

}
