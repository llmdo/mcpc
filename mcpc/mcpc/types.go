package mcpc

import (
	"encoding/json"
	"fmt"
)

/* =========================
   JSON-RPC 2.0 基础类型
   ========================= */

const jsonrpcVersion = "2.0"

type RPCRequest struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`     // notification 时为 nil
	Method  string           `json:"method"`           // e.g. "tools/call", "tools/list"
	Params  *json.RawMessage `json:"params,omitempty"` // 任意 JSON
}

type RPCResponse struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *string          `json:"id,omitempty"`
	Result  *json.RawMessage `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error: code=%d message=%s", e.Code, e.Message)
}
