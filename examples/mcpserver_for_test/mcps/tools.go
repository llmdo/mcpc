package mcps

import (
	"encoding/json"
	"strings"
)

/* =========================
   工具实现
   ========================= */

func handleToolsList(id *string) *RpcResp {
	result := toolsListResult{
		Tools: []toolInfo{
			{
				Name:         "echo",
				Description:  "返回原文",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"output":{"type":"string"}}}`),
			},
			{
				Name:         "reverse",
				Description:  "反转字符串",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"output":{"type":"string"}}}`),
			},
			{
				Name:         "uppercase",
				Description:  "转大写",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
				OutputSchema: json.RawMessage(`{"type":"object","properties":{"output":{"type":"string"}}}`),
			},
		},
	}
	b, _ := json.Marshal(result)
	raw := json.RawMessage(b)
	return &RpcResp{JSONRPC: JsonrpcVersion, ID: id, Result: &raw}
}

func handleToolsCall(id *string, params *json.RawMessage) *RpcResp {
	if params == nil {
		return RpcError(id, -32602, "Invalid params")
	}

	var p struct {
		Name   string           `json:"name"`
		Params *json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(*params, &p); err != nil {
		return RpcError(id, -32700, "Parse error")
	}

	var pp struct {
		Message string `json:"message"`
	}
	if p.Params != nil {
		_ = json.Unmarshal(*p.Params, &pp)
	}

	if pp.Message == "" {
		return RpcError(id, -32602, "missing param: message")
	}

	out := pp.Message
	switch strings.ToLower(p.Name) {
	case "echo":
	case "reverse":
		r := []rune(out)
		for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
			r[i], r[j] = r[j], r[i]
		}
		out = string(r)
	case "uppercase":
		out = strings.ToUpper(out)
	default:
		return RpcError(id, -32601, "Unknown tool")
	}

	b, _ := json.Marshal(map[string]any{"output": out})
	raw := json.RawMessage(b)
	return &RpcResp{JSONRPC: JsonrpcVersion, ID: id, Result: &raw}
}

// initialize / shutdown
func handleInitialize(id *string, params *json.RawMessage) *RpcResp {
	result := map[string]any{
		"serverInfo": map[string]any{
			"name":    "demo-mcp-server",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{
			"tools": true,
		},
	}
	b, _ := json.Marshal(result)
	raw := json.RawMessage(b)
	return &RpcResp{JSONRPC: JsonrpcVersion, ID: id, Result: &raw}
}

func handleShutdown(id *string) *RpcResp {
	return &RpcResp{JSONRPC: JsonrpcVersion, ID: id, Result: nil}
}

func DispatchRPC(req *RpcReq) *RpcResp {
	if req.JSONRPC != JsonrpcVersion {
		return RpcError(req.ID, -32600, "Invalid Request")
	}

	switch req.Method {
	case "tools/list":
		return handleToolsList(req.ID)
	case "tools/call":
		return handleToolsCall(req.ID, req.Params)
	case "initialize":
		return handleInitialize(req.ID, req.Params)
	case "shutdown":
		return handleShutdown(req.ID)
	default:
		if req.ID == nil {
			return nil
		}
		return RpcError(req.ID, -32601, "Method not found")
	}
}

func RpcError(id *string, code int, msg string) *RpcResp {
	return &RpcResp{
		JSONRPC: JsonrpcVersion,
		ID:      id,
		Error:   &rpcErr{Code: code, Message: msg},
	}
}
