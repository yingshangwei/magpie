package peering

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
)

type outcomeKey struct{}

// Outcome records a failed peer exchange, whose upstream execution may
// already have started. The caller must not replay it or try a fallback.
type Outcome struct{ failed atomic.Bool }

func Observe(ctx context.Context) (context.Context, *Outcome) {
	o := &Outcome{}
	return context.WithValue(ctx, outcomeKey{}, o), o
}

func (o *Outcome) Failed() bool { return o != nil && o.failed.Load() }

func noteFailure(ctx context.Context, err error) {
	if err == nil || errors.Is(err, io.EOF) {
		return
	}
	if o, _ := ctx.Value(outcomeKey{}).(*Outcome); o != nil {
		o.failed.Store(true)
	}
}

type observedBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	noteFailure(b.ctx, err)
	return n, err
}
