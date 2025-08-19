package mcpc

import (
	"errors"
	"fmt"
)

/* =========================
   统一错误类型（区分传输层 vs 协议层）
   ========================= */

var (
	// 可用于识别：传输层已关闭或不可用（可重试）
	ErrTransportClosed = errors.New("transport closed")
)

// TransportError 用于标识传输层错误（可能可重试）
type TransportError struct {
	Op        string // 读 / 写 / 连接
	Err       error  // 原始错误
	Temporary bool   // 是否临时错误（建议重试）
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("transport %s error: %v", e.Op, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }
