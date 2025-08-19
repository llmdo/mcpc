package mcpc

import (
	"context"
	"encoding/json"
)

/* =========================
   Transport 抽象
   ========================= */

type Transport interface {
	Send(ctx context.Context, payload []byte) error
	Recv() <-chan []byte
	Close() error
	IsConnected() bool
}

/* =========================
   内部特殊通知（断线）  工具函数
   ========================= */

const InternalDisconnectedMethod = "$transport/disconnected"

func makeInternalDisconnectedNote(err error) []byte {
	msg := map[string]any{
		"jsonrpc": JsonrpcVersion,
		"method":  InternalDisconnectedMethod,
		"params":  map[string]any{"error": err.Error()},
	}
	b, _ := json.Marshal(msg)
	return b
}
