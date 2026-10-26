package netproxy

import (
	"context"
	"net/http"
)

type transportKey struct{}
type transportChoice struct{ transport http.RoundTripper }

// WithTransport carries a provider's local transport through the common
// HTTP clients. A nil transport clears the previous provider's choice.
func WithTransport(ctx context.Context, t http.RoundTripper) context.Context {
	if t == nil && transportOf(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, transportKey{}, transportChoice{t})
}

func transportOf(ctx context.Context) http.RoundTripper {
	v, _ := ctx.Value(transportKey{}).(transportChoice)
	return v.transport
}
