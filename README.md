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
transport := mcpc.NewWSTransport("ws://localhost:8080/mcp", nil)
client := mcpc.NewMCPClient(transport, &mcpc.DialOptions{}, nil)
defer client.Close()

resp, err := client.Call(context.Background(), "ping", nil)
```

### SSE

```go
transport := mcpc.NewSSETransport("http://localhost:8080/mcp", nil)
client := mcpc.NewMCPClient(transport, &mcpc.DialOptions{}, nil)
defer client.Close()

_ = client.Notify(context.Background(), "log", map[string]any{"msg": "hello"})
```

---

## 特性

* `Call`：RPC 请求，带超时
* `Notify`：通知，不等待返回
* `CallBatch`：批量请求
* `Hook`：调试事件
* `Context`：超时控制 & 强制退出重连
* `IsConnected`：连接状态检查
