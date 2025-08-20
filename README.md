# mcpc is an MCP Client for Go

一个轻量级的 **Model Context Protocol (MCP) 客户端实现**，支持：
- ✅ WebSocket (WS) 传输
- ✅ Server-Sent Events (SSE) 传输
- ✅ JSON-RPC 2.0 调用与通知
- ✅ 自动 pending 管理与断线清理
- ✅ Hook 调试接口
- ✅ Context 控制超时与重连

---

## 安装

```bash
go get github.com/llmdo/mcpc
```

---

## 快速开始

### WebSocket

```go
transport := mcpc.NewWSTransport("ws://localhost:8080/mcp", opts)
client := mcpc.NewMCPClient(transport, &mcpc.DialOptions{}, opts)
defer client.Close()

resp, err := client.Call(context.Background(), "ping", nil)
```

### SSE

```go
transport := mcpc.NewSSETransport("http://localhost:8080/mcp", opts)
client := mcpc.NewMCPClient(transport, &mcpc.DialOptions{}, opts)
defer client.Close()

_ = client.Notify(context.Background(), "log", map[string]any{"msg": "hello"})
```

### STDIO

```go
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
transport, err := mcpc.NewStdioSubprocess(
    filepath.Join(filepath.Dir(exePath), "../mcpserver_for_test/mcpserver_for_test.exe"), 
    []string{"stdio"}, 
    opts)
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
```


---

## 特性

* `Call`：RPC 请求，带超时
* `Notify`：通知，不等待返回
* `CallBatch`：批量请求
* `Hook`：调试事件
* `Context`：超时控制 & 强制退出重连
* `IsConnected`：连接状态检查
