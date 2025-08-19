package mcpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/* =========================
   SSE Transport（含重连）
   - GET 事件流自动重连
   - POST 失败重试
   - 断线时注入内部通知
   ========================= */

type SSETransport struct {
	postURL   string
	eventsURL string
	opts      *DialOptions
	client    *http.Client

	recvC  chan []byte
	alive  atomic.Bool
	stopCh chan struct{}
	wg     sync.WaitGroup

	reconnector *Reconnector
}

func NewSSETransport(postURL string, opts *DialOptions) (*SSETransport, error) {
	opt := opts.WithDefaults()
	if opt.SSEEventsURL == "" {
		return nil, errors.New("SSE requires DialOptions.SSEEventsURL")
	}
	t := &SSETransport{
		postURL:   postURL,
		eventsURL: opt.SSEEventsURL,
		opts:      opt,
		client:    opt.HTTPClient,
		recvC:     make(chan []byte, 256),
		stopCh:    make(chan struct{}),
	}
	t.reconnector = NewReconnector(opt)

	// start manager
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		t.reconnector.Manage(t.connectAndServe, t.recvC, &t.alive, t.stopCh)
	}()
	return t, nil
}

func (t *SSETransport) connectAndServe(connected chan<- struct{}) error {
	opt := t.opts

	req, _ := http.NewRequestWithContext(opt.CancelCtx, http.MethodGet, t.eventsURL, nil)
	req.Header = opt.Headers.Clone()

	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("SSE bad status: %s", resp.Status)
	}

	// connected
	select {
	case connected <- struct{}{}:
	default:
	}

	reader := bufio.NewReader(resp.Body)
	var dataLines []string

	for {
		select {
		case <-t.stopCh:
			return nil
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) > 0 {
				full := strings.Join(dataLines, "\n")
				dataLines = nil
				if full != "" {
					if opt.OnMessage != nil {
						opt.OnMessage([]byte(full))
					}
					select {
					case t.recvC <- []byte(full):
					case <-t.stopCh:
						return nil
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataContent := strings.TrimSpace(line[5:])
			dataLines = append(dataLines, dataContent)
		}
	}
}
func (t *SSETransport) Send(ctx context.Context, payload []byte) error {
	opt := t.opts
	// if not connected, wait for reconnect (controlled by ctx)
	if !t.alive.Load() {
		if !t.reconnector.WaitForReconnect(ctx) {
			return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
		}
	}

	var resp *http.Response
	var err error
	for i := 0; i < opt.MaxRetries; i++ {
		req, rerr := http.NewRequestWithContext(ctx, "POST", t.postURL, bytes.NewReader(payload))
		if rerr != nil {
			return rerr
		}
		req.Header = opt.Headers.Clone()
		resp, err = t.client.Do(req)
		if err == nil {
			break
		}
		select {
		case <-time.After(time.Duration(i+1) * 300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		case <-t.stopCh:
			return ErrTransportClosed
		}
	}
	if err != nil {
		select {
		case t.recvC <- makeInternalDisconnectedNote(err):
		default:
		}
		if opt.OnDisconnected != nil {
			opt.OnDisconnected(err)
		}
		return &TransportError{Op: "write", Err: err, Temporary: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("sse post status=%s body=%s", resp.Status, string(b))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (t *SSETransport) Recv() <-chan []byte { return t.recvC }

func (t *SSETransport) Close() error {
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	t.wg.Wait()
	select {
	default:
		close(t.recvC)
	}
	return nil
}

func (t *SSETransport) IsConnected() bool { return t.alive.Load() }
