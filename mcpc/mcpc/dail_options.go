package mcpc

import (
	"crypto/tls"
	"net"
	"net/http"
	"time"
)

/* =========================
   配置与工具
   ========================= */

type DialOptions struct {
	// 通用
	Headers        http.Header
	AuthToken      string        // 可选：Bearer
	RequestTimeout time.Duration // 每次调用的默认超时

	// 心跳（WS）
	PingInterval time.Duration
	PongWait     time.Duration

	// 回调
	OnDisconnected func(error)
	OnReconnected  func()

	// TLS / Transport
	InsecureSkipVerify bool

	// SSE: 分离的发送与事件流地址
	SSEEventsURL string // e.g. http(s)://host/mcp/events

	// HTTPClient（可注入自定义传输/代理）
	HTTPClient *http.Client

	// 重试
	MaxRetries int

	// 重连策略
	ReconnectInitialBackoff time.Duration // e.g. 500ms
	ReconnectMaxBackoff     time.Duration // e.g. 10s
}

func (o *DialOptions) WithDefaults() *DialOptions {
	cp := *o
	if cp.RequestTimeout <= 0 {
		cp.RequestTimeout = 30 * time.Second
	}
	if cp.PingInterval <= 0 {
		cp.PingInterval = 20 * time.Second
	}
	if cp.PongWait <= 0 {
		cp.PongWait = 60 * time.Second
	}
	if cp.MaxRetries <= 0 {
		cp.MaxRetries = 3
	}
	if cp.ReconnectInitialBackoff <= 0 {
		cp.ReconnectInitialBackoff = 500 * time.Millisecond
	}
	if cp.ReconnectMaxBackoff <= 0 {
		cp.ReconnectMaxBackoff = 10 * time.Second
	}
	if cp.HTTPClient == nil {
		tr := &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: cp.InsecureSkipVerify}, // demo 用
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2: true,
		}
		cp.HTTPClient = &http.Client{Transport: tr, Timeout: cp.RequestTimeout}
	}
	if cp.Headers == nil {
		cp.Headers = make(http.Header)
	}
	if cp.AuthToken != "" && cp.Headers.Get("Authorization") == "" {
		cp.Headers.Set("Authorization", "Bearer "+cp.AuthToken)
	}
	cp.Headers.Set("Content-Type", "application/json")
	return &cp
}
