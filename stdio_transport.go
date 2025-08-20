package mcpc

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// =========================
// Stdio Transport
// - 支持传入任意 r/w
// - 支持子进程启动模式
// =========================

type StdioTransport struct {
	r     *bufio.Reader
	w     *bufio.Writer
	muW   sync.Mutex
	recvC chan []byte
	alive atomic.Bool
	wg    sync.WaitGroup

	opts *DialOptions

	// 如果是子进程模式
	cmd *exec.Cmd
}

func NewStdioTransport(r io.Reader, w io.Writer, opts *DialOptions) *StdioTransport {
	if opts == nil {
		opts = &DialOptions{}
	}
	opts = opts.WithDefaults()

	t := &StdioTransport{
		r:     bufio.NewReader(r),
		w:     bufio.NewWriter(w),
		recvC: make(chan []byte, 128),
		opts:  opts,
	}
	t.alive.Store(true)
	t.wg.Add(1)
	go t.readLoop()
	return t
}

// 启动一个子进程，并使用其 stdio 作为传输层
func NewStdioSubprocess(serverPath string, args []string, opts *DialOptions) (*StdioTransport, error) {
	cmd := exec.Command(serverPath, args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// stderr 也可以重定向方便调试
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	t := NewStdioTransport(stdout, stdin, opts)
	t.cmd = cmd
	return t, nil
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

		length := -1
		for {
			line, err := t.r.ReadString('\n')
			if err != nil {
				t.alive.Store(false)
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				break
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

	// 如果是子进程模式，顺带清理
	if t.cmd != nil && t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
		_, _ = t.cmd.Process.Wait()
	}
	return nil
}

func (t *StdioTransport) IsConnected() bool { return t.alive.Load() }
