package mcpc

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
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
	t.alive.Store(true)
	t.wg.Add(1)
	go t.eventLoop()
	return t, nil
}

func (t *SSETransport) eventLoop() {
	defer t.wg.Done()
	backoff := t.opts.ReconnectInitialBackoff

	for {
		select {
		case <-t.stopCh:
			close(t.recvC)
			return
		default:
		}

		req, _ := http.NewRequestWithContext(context.Background(), "GET", t.eventsURL, nil)
		req.Header = t.opts.Headers.Clone()

		resp, err := t.client.Do(req)
		if err != nil {
			// 连接失败：发内部通知，回调，然后退避重连
			select {
			case t.recvC <- makeInternalDisconnectedNote(err):
			default:
			}
			if t.opts.OnDisconnected != nil {
				t.opts.OnDisconnected(err)
			}
			time.Sleep(backoff)
			backoff = time.Duration(math.Min(float64(t.opts.ReconnectMaxBackoff), float64(backoff)*1.8))
			continue
		}

		// 成功连接
		if t.opts.OnReconnected != nil {
			t.opts.OnReconnected()
		}
		backoff = t.opts.ReconnectInitialBackoff

		func() {
			defer resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				err = fmt.Errorf("SSE bad status: %s", resp.Status)
				select {
				case t.recvC <- makeInternalDisconnectedNote(err):
				default:
				}
				if t.opts.OnDisconnected != nil {
					t.opts.OnDisconnected(err)
				}
				return
			}

			reader := bufio.NewReader(resp.Body)
			var dataLines []string

			for {
				select {
				case <-t.stopCh:
					return
				default:
				}

				line, err := reader.ReadString('\n')
				if err != nil {
					if !errors.Is(err, io.EOF) {
						// 非 EOF 也算断线
						select {
						case t.recvC <- makeInternalDisconnectedNote(err):
						default:
						}
						if t.opts.OnDisconnected != nil {
							t.opts.OnDisconnected(err)
						}
					}
					return
				}
				line = strings.TrimRight(line, "\r\n")
				if line == "" {
					if len(dataLines) > 0 {
						full := strings.Join(dataLines, "\n")
						dataLines = nil
						if full != "" {
							select {
							case t.recvC <- []byte(full):
							case <-t.stopCh:
								return
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
		}()
		// 走到这里说明需要重连
	}
}

func (t *SSETransport) Send(ctx context.Context, payload []byte) error {
	if !t.alive.Load() {
		return &TransportError{Op: "write", Err: ErrTransportClosed, Temporary: true}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", t.postURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header = t.opts.Headers.Clone()

	var resp *http.Response
	for i := 0; i < t.opts.MaxRetries; i++ {
		resp, err = t.client.Do(req)
		if err == nil {
			break
		}
		time.Sleep(time.Duration(i+1) * 300 * time.Millisecond)
	}
	if err != nil {
		// 发送失败：视作临时传输错误
		select {
		case t.recvC <- makeInternalDisconnectedNote(err):
		default:
		}
		if t.opts.OnDisconnected != nil {
			t.opts.OnDisconnected(err)
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
	if !t.alive.Swap(false) {
		return nil
	}
	close(t.stopCh)
	t.wg.Wait()
	return nil
}

func (t *SSETransport) IsConnected() bool { return t.alive.Load() }
