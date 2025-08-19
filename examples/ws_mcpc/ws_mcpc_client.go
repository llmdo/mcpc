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
	}).WithDefaults()
	// 初始化 WS 传输层
	transport, err := mcpc.NewWebSocketTransport("ws://localhost:18081/mcp/ws", opts)
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

	client := mcpc.NewMCPClient(transport, opts, hooks)
	defer client.Close()

	// RPC 调用
	// !!! sse示例的方法在ws都可以使用，Call CallBatch ToolsCall ToolsList 都是可用的方法

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	params := map[string]any{"message": "batch test"}
	paramsRaw, _ := json.Marshal(params)

	payload := map[string]any{"name": "echo", "params": json.RawMessage(paramsRaw)}
	payloadRaw := json.RawMessage(mcpc.MustJSON(payload))
	//id := client.NextID()
	rpcResponse, err := client.Call(ctx, "tools/call", &payloadRaw)
	if err != nil {
		log.Printf("[call] err: %v", err)
	} else {
		log.Printf("[call] result: %s", string(*rpcResponse.Result))
	}

	tools := []string{"echo", "reverse", "uppercase"}
	var batch []mcpc.RPCRequest
	for _, tool := range tools {
		id := client.NextID()
		payload := map[string]any{"name": tool, "params": json.RawMessage(paramsRaw)}
		payloadRaw := json.RawMessage(mcpc.MustJSON(payload))
		batch = append(batch, mcpc.RPCRequest{
			JSONRPC: mcpc.JsonrpcVersion,
			ID:      &id,
			Method:  "tools/call",
			Params:  &payloadRaw,
		})
	}
	resps, _ := client.CallBatch(ctx, batch)
	for _, r := range resps {
		log.Println("batch resp:", string(*r.Result))
	}
}
