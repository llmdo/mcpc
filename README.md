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
