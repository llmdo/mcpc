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
   Stdio Transport（与原版一致，增加日志健壮性）
   ========================= */

type StdioTransport struct {
	r     *bufio.Reader
	w     *bufio.Writer
	muW   sync.Mutex
	recvC chan []byte
	alive atomic.Bool
	stop  chan struct{}
	wg    sync.WaitGroup
}

func NewStdioTransport() *StdioTransport {
	t := &StdioTransport{
		r:     bufio.NewReader(os.Stdin),
		w:     bufio.NewWriter(os.Stdout),
		recvC: make(chan []byte, 128),
		stop:  make(chan struct{}),
	}
	t.alive.Store(true)
	t.wg.Add(1)
	go t.readLoop()
	return t
}

func (t *StdioTransport) readLoop() {
	defer close(t.recvC)
	defer t.wg.Done()
	for t.alive.Load() {
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
				break // 头结束
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
		return err
	}
	if _, err := t.w.Write(payload); err != nil {
		return err
	}
	return t.w.Flush()
}

func (t *StdioTransport) Recv() <-chan []byte { return t.recvC }

func (t *StdioTransport) Close() error {
	if !t.alive.Swap(false) {
		return nil
	}
	close(t.stop)
	_ = t.w.Flush()
	t.wg.Wait()
	return nil
}

func (t *StdioTransport) IsConnected() bool { return t.alive.Load() }
