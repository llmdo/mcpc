package mcpc

import (
	"context"
	"encoding/json"
	"errors"
)

/* =========================
   MCP 协议方法封装
   ========================= */

type ToolInfo struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema *json.RawMessage `json:"inputSchema,omitempty"`
}

type ToolsListResult struct {
	Tools []ToolInfo `json:"tools"`
}

func (c *MCPClient) ToolsList(ctx context.Context) (*ToolsListResult, error) {
	resp, err := c.Call(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out ToolsListResult
	if resp.Result == nil {
		return nil, errors.New("tools/list: empty result")
	}
	if err := json.Unmarshal(*resp.Result, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *MCPClient) ToolsCall(ctx context.Context, name string, params any) (json.RawMessage, error) {
	var payload any
	if raw, ok := params.(json.RawMessage); ok {
		payload = map[string]any{"name": name, "params": raw}
	} else {
		payload = map[string]any{"name": name, "params": params}
	}
	resp, err := c.Call(ctx, "tools/call", payload)
	if err != nil {
		return nil, err
	}
	if resp.Result == nil {
		return nil, errors.New("tools/call: empty result")
	}
	return *resp.Result, nil
}

func (c *MCPClient) Initialize(ctx context.Context, params any) (json.RawMessage, error) {
	var payload any
	if raw, ok := params.(json.RawMessage); ok {
		payload = map[string]any{"params": raw}
	} else {
		payload = map[string]any{"params": params}
	}

	resp, err := c.Call(ctx, "initialize", payload)
	if err != nil {
		return nil, err
	}

	if resp.Result == nil {
		return []byte(""), nil
	}

	return *resp.Result, nil
}
