package mcpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMCPClient(t *testing.T) {

	tests := []struct {
		name  string
		model string
	}{
		{"SSE", "SSE"},
		{"WebSocket", "WS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if e := totest(tt.model); e != nil {
				t.Errorf("MCPClient do something got error: %v", e)
			}
		})
	}
}

func totest(model string) error {
	mode := EnvStr("MCP_MODE", model) // WS | SSE | STDIO

	opts := (&DialOptions{
		AuthToken:               EnvStr("MCP_TOKEN", "MCP_TOKEN-123456789"),
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

	var tr Transport
	var err error

	switch strings.ToUpper(mode) {
	case "WS":
		wsURL := EnvStr("MCP_WS_URL", "ws://localhost:18081/mcp/ws")
		tr, err = NewWebSocketTransport(wsURL, opts)
		if err != nil {
			return err
		}
	case "SSE":
		postURL := EnvStr("MCP_POST_URL", "http://localhost:18080/mcp")
		eventsURL := EnvStr("MCP_EVENTS_URL", "http://localhost:18080/mcp/events")
		opts.SSEEventsURL = eventsURL
		tr, err = NewSSETransport(postURL, opts)
		if err != nil {
			return err
		}
	//case "STDIO":
	//	tr = NewStdioTransport(nil)
	default:
		log.Fatalf("unknown MCP_MODE %s", mode)
	}

	clientHooks := &ClientHooks{
		OnSend: func(id, method string) {
			log.Printf("OnSend(%s,%s)", id, method)
		},
		OnResponse: func(id string, err *RPCError) {
			log.Printf("OnResponse(%s,%+v)", id, err)
		},
		OnNotify: func(method string) {
			log.Printf("OnNotify(%s)", method)
		},
		OnDisconnect: func(temporary bool) {
			log.Printf("OnDisconnect(%v)", temporary)
		},
	}
	client := NewMCPClient(tr, opts, clientHooks)
	defer client.Close()

	client.SetNotificationHandler(func(method string, params json.RawMessage) {
		if method == InternalDisconnectedMethod {
			log.Printf("[notify] transport disconnected: %s", string(params))
			return
		}
		log.Printf("[notify] %s: %s", method, string(params))
	})

	ctx := context.Background()

	// 1) 列出工具
	list, err := client.ToolsList(ctx)
	if err != nil {
		return errors.New(fmt.Sprintf("ToolsList error: %v", err))
	}
	log.Printf("Available tools (%d):", len(list.Tools))
	for i, tool := range list.Tools {
		log.Printf("  %d. %s - %s", i+1, tool.Name, tool.Description)
	}

	// 2) 调用工具（取第一个）
	if len(list.Tools) > 0 {
		first := list.Tools[0].Name
		log.Printf("Calling tool: %s", first)
		res, err := client.ToolsCall(ctx, first, map[string]any{"message": "Hello from MCP client"})
		if err != nil {
			return errors.New(fmt.Sprintf("ToolsCall error: %v", err))
		}
		log.Printf("Tool response: %s", string(res))
	}

	// 3) 批量调用
	log.Println("Sending batch request...")
	tools := []string{"echo", "reverse", "uppercase"}
	var batch []RPCRequest
	for _, tool := range tools {
		id := client.NextID()
		params := map[string]any{"message": "batch test"}
		paramsRaw, _ := json.Marshal(params)
		payload := map[string]any{"name": tool, "params": json.RawMessage(paramsRaw)}
		payloadRaw := json.RawMessage(MustJSON(payload))
		batch = append(batch, RPCRequest{
			JSONRPC: JsonrpcVersion,
			ID:      &id,
			Method:  "tools/call",
			Params:  &payloadRaw,
		})
	}
	bres, berr := client.CallBatch(ctx, batch)
	if berr != nil {
		return errors.New(fmt.Sprintf("Batch results (error: %v):", berr))
	}

	for _, resp := range bres {
		if resp.ID == nil {
			continue
		}
		if resp.Error != nil {
			return errors.New(fmt.Sprintf("  [%s] ERROR: %s", *resp.ID, resp.Error.Message))
		} else if resp.Result != nil {
			log.Printf("  [%s] RESULT: %s", *resp.ID, string(*resp.Result))
		} else {
			return errors.New(fmt.Sprintf("  [%s] EMPTY", *resp.ID))
		}
	}
	return nil
}
