package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/peering"
	"github.com/yetone/magpie/internal/provider"
)

func TestPeerRetrievalFallback(t *testing.T) {
	for _, api := range []struct{ name, body, reply string }{
		{"embeddings", `{"model":"group/retrieval","input":"billable work"}`, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"m1","usage":{"prompt_tokens":1,"total_tokens":1}}`},
		{"rerank", `{"model":"group/retrieval","query":"query","documents":["billable work"]}`, `{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":1}}`},
	} {
		for _, disconnect := range []bool{true, false} {
			mode := "complete-http-error"
			if disconnect {
				mode = "disconnect"
			}
			t.Run(api.name+"/"+mode, func(t *testing.T) {
				fresh(t)
				started := make(chan struct{})
				var once sync.Once
				var upstreamCalls, spareCalls atomic.Int32
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					upstreamCalls.Add(1)
					once.Do(func() { close(started) })
					if disconnect {
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error":{"message":"rate limited"}}`)
				}))
				t.Cleanup(up.Close)
				spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					spareCalls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					io.WriteString(w, api.reply)
				}))
				t.Cleanup(spare.Close)
				for _, p := range []provider.Provider{
					{ID: "fake", Key: "test-only", Chat: up.URL + "/v1", Models: []string{"m1"}},
					{ID: "spare", Key: "test-only", Chat: spare.URL + "/v1", Models: []string{"m1"}},
				} {
					if err := provider.Save(p); err != nil {
						t.Fatal(err)
					}
				}
				pair := newPeerGatewayPair(t, peering.Peer{Import: true}, peering.Peer{Exports: []peering.Selection{{Provider: "fake", Models: []string{"m1"}}}})
				if err := provider.SaveGroup(provider.Group{ID: "retrieval", Members: []string{"peer-office/fake/m1", "spare/m1"}, Routing: "order"}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				r, err := http.NewRequestWithContext(ctx, http.MethodPost, pair.aHTTP.URL+"/v1/"+api.name, strings.NewReader(api.body))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+Token)
				type result struct {
					status int
					err    error
				}
				finished := make(chan result, 1)
				go func() {
					res, err := http.DefaultClient.Do(r)
					status := 0
					if err == nil {
						status = res.StatusCode
						_, err = io.Copy(io.Discard, res.Body)
						res.Body.Close()
					}
					finished <- result{status, err}
				}()
				if disconnect {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("peer vendor never received the request")
					}
					pair.disconnect()
				}
				select {
				case got := <-finished:
					if got.err != nil {
						t.Fatal(got.err)
					}
					wantStatus, wantSpare := http.StatusOK, int32(1)
					if disconnect {
						wantStatus, wantSpare = http.StatusBadGateway, 0
					}
					if upstreamCalls.Load() != 1 || spareCalls.Load() != wantSpare || got.status != wantStatus {
						t.Fatalf("peer vendor calls=%d, fallback vendor calls=%d, status=%d; want 1, %d, %d", upstreamCalls.Load(), spareCalls.Load(), got.status, wantSpare, wantStatus)
					}
				case <-ctx.Done():
					t.Fatal("request did not finish")
				}
			})
		}
	}
}
