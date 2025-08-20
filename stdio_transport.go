package mcpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

/* =========================
   Stdio Transport
   - 与 WS/SSE 风格统一
   - 支持 CancelCtx 控制 opts中提供上下文
   - 统一错误模型 (TransportError)
   - 支持 Hook (OnMessage / OnDisconnect) opt中提供hook函数
   ========================= */

type StdioTransport struct {
	r     *bufio.Reader
	w     *bufio.Writer
	muW   sync.Mutex
	recvC chan []byte
	alive atomic.Bool
	wg    sync.WaitGroup

	opts *DialOptions
}

func NewStdioTransport(opts *DialOptions) *StdioTransport {
	if opts == nil {
		opts = &DialOptions{}
	}
	opts = opts.WithDefaults()

	t := &StdioTransport{
		r:     bufio.NewReader(os.Stdin),
		w:     bufio.NewWriter(os.Stdout),
		recvC: make(chan []byte, 128),
		opts:  opts,
	}
	t.alive.Store(true)
	t.wg.Add(1)
	go t.readLoop()
	return t
}

func (t *StdioTransport) readLoop() {
	defer close(t.recvC)
	defer t.wg.Done()
	defer func() {
		if h := t.opts.OnDisconnected; h != nil {
			h(nil)
		}
	}()

	ctx := context.Background()
	if t.opts.CancelCtx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(t.opts.CancelCtx)
		defer cancel()
	}

	for t.alive.Load() {
		select {
		case <-ctx.Done():
			t.alive.Store(false)
			return
		default:
		}

		// 读头部
		length := -1
		for {
			line, err := t.r.ReadString('\n')
			if err != nil {
				t.alive.Store(false)
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break // header end
			}
			if strings.HasPrefix(strings.ToLower(line), "content-length:") {
				v := strings.TrimSpace(line[len("content-length:"):])
				n, _ := strconv.Atoi(v)
				length = n
			}
		}
		if length < 0 {
			log.Printf("[stdio] invalid header: missing Content-Length")
			continue
		}

		body := make([]byte, length)
		if _, err := io.ReadFull(t.r, body); err != nil {
			t.alive.Store(false)
			return
		}

		// Hook
		if h := t.opts.OnMessage; h != nil {
			h(body)
		}

		t.recvC <- body
	}
}

func (t *StdioTransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	t.muW.Lock()
	defer t.muW.Unlock()

	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(payload))
	if _, err := t.w.WriteString(header); err != nil {
		return &TransportError{Op: "write", Err: err, Temporary: false}
	}
	if _, err := t.w.Write(payload); err != nil {
		return &TransportError{Op: "write", Err: err, Temporary: false}
	}
	if err := t.w.Flush(); err != nil {
		return &TransportError{Op: "write", Err: err, Temporary: false}
	}
	return nil
}

func (t *StdioTransport) Recv() <-chan []byte { return t.recvC }

func (t *StdioTransport) Close() error {
	if !t.alive.Swap(false) {
		return nil
	}
	t.wg.Wait()
	_ = t.w.Flush()
	return nil
}

func (t *StdioTransport) IsConnected() bool { return t.alive.Load() }
