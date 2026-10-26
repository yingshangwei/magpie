package peerwire

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

type incoming struct {
	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{}
	mu     sync.Mutex
	window int
	credit int
}

func (i *incoming) addWindow(n int) bool {
	i.mu.Lock()
	valid := n > 0 && n <= i.window-i.credit
	if valid {
		i.credit += n
	}
	i.mu.Unlock()
	if valid {
		signal(i.wake)
	}
	return valid
}

func (i *incoming) take(n int) error {
	for {
		if err := i.ctx.Err(); err != nil {
			return err
		}
		i.mu.Lock()
		if i.credit >= n {
			i.credit -= n
			i.mu.Unlock()
			return nil
		}
		i.mu.Unlock()
		select {
		case <-i.wake:
		case <-i.ctx.Done():
			return i.ctx.Err()
		}
	}
}

func (c *Conn) serve(id uint64, i *incoming, req *http.Request) {
	w := &responseWriter{conn: c, id: id, stream: i, header: make(http.Header)}
	defer func() {
		streamError := ""
		if recover() != nil {
			if !w.started {
				w.WriteHeader(http.StatusInternalServerError)
			} else {
				streamError = "handler interrupted"
			}
		}
		w.start(http.StatusOK)
		if w.err == nil {
			_ = c.send(i.ctx, message{Type: "end", ID: id, Error: streamError})
		}
		i.cancel()
		_ = req.Body.Close()
		c.mu.Lock()
		delete(c.in, id)
		c.mu.Unlock()
	}()
	c.handler.ServeHTTP(w, req)
}

// responseWriter intentionally supports only ordinary HTTP and Flush. Upgrades,
// trailers and informational responses are outside the peer HTTP contract.
type responseWriter struct {
	conn    *Conn
	id      uint64
	stream  *incoming
	header  http.Header
	started bool
	err     error
}

func (w *responseWriter) Header() http.Header { return w.header }

func (w *responseWriter) start(status int) {
	if w.started {
		return
	}
	w.started = true
	header := w.header.Clone()
	if !validHeader(header) {
		status, header = http.StatusInternalServerError, nil
	}
	w.err = w.conn.send(w.stream.ctx, message{Type: "response", ID: w.id, Status: status, Header: header})
}

func (w *responseWriter) WriteHeader(status int) {
	if status < 100 || status > 599 {
		panic("peerwire: invalid response status")
	}
	if status >= 200 {
		w.start(status)
	}
}

func (w *responseWriter) Write(p []byte) (int, error) {
	w.start(http.StatusOK)
	if w.err != nil {
		return 0, w.err
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), chunkBytes)
		if w.err = w.stream.take(n); w.err != nil {
			return written, w.err
		}
		// send can return early on cancellation, so it cannot retain the caller's
		// slice after Write returns, even while a socket write is finishing.
		part := append([]byte(nil), p[:n]...)
		w.err = w.conn.send(w.stream.ctx, message{Type: "data", ID: w.id, Body: part})
		if w.err != nil {
			return written, w.err
		}
		p = p[n:]
		written += n
	}
	return written, nil
}

func (w *responseWriter) Flush() { _ = w.FlushError() }

func (w *responseWriter) FlushError() error {
	w.start(http.StatusOK)
	return w.err // Every Write is already an individual WebSocket message.
}

// responseBody uses a fixed byte ring instead of one queue entry per frame. A
// peer sending one-byte chunks therefore cannot amplify the receive window.
type responseBody struct {
	mu       sync.Mutex
	buffer   []byte
	capacity int
	head     int
	size     int
	terminal error
	closed   bool
	wake     chan struct{}
	onRead   func(int)
	onEOF    func()
	onClose  func()
}

func newResponseBody(capacity int, onRead func(int), onEOF, onClose func()) *responseBody {
	return &responseBody{capacity: capacity, wake: make(chan struct{}, 1), onRead: onRead, onEOF: onEOF, onClose: onClose}
}

func (b *responseBody) push(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.terminal != nil {
		return true // Cancellation may race with a frame already being read.
	}
	if len(p) > b.capacity-b.size {
		return false
	}
	if b.buffer == nil {
		b.buffer = make([]byte, b.capacity)
	}
	start := (b.head + b.size) % b.capacity
	n := copy(b.buffer[start:], p)
	copy(b.buffer, p[n:])
	b.size += len(p)
	signal(b.wake)
	return true
}

func (b *responseBody) finish(err error) {
	b.mu.Lock()
	if b.terminal == nil || !errors.Is(err, io.EOF) {
		b.terminal = err
		if !errors.Is(err, io.EOF) {
			b.buffer, b.size = nil, 0
		}
	}
	b.mu.Unlock()
	signal(b.wake)
}

func (b *responseBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		b.mu.Lock()
		if b.size > 0 {
			n := min(len(p), b.size)
			first := copy(p[:n], b.buffer[b.head:min(b.head+n, b.capacity)])
			copy(p[first:n], b.buffer[:n-first])
			b.head = (b.head + n) % b.capacity
			b.size -= n
			ended := b.terminal != nil
			drained := ended && b.size == 0
			b.mu.Unlock()
			if drained {
				b.onEOF()
			} else if !ended {
				b.onRead(n)
			}
			return n, nil
		}
		err := b.terminal
		b.mu.Unlock()
		if err != nil {
			b.onEOF()
			return 0, err
		}
		<-b.wake
	}
}

func (b *responseBody) Close() error {
	b.mu.Lock()
	first := !b.closed
	b.closed, b.buffer, b.size, b.terminal = true, nil, 0, context.Canceled
	b.mu.Unlock()
	signal(b.wake)
	if first {
		b.onClose()
	}
	return nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
