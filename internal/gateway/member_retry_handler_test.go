package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// Exercise the final client handoff, including a stream whose keepalive
// already sent its headers. The first failure queues a competing turn
// before the retry pause, so queue timeouts need no scheduler race.
func TestMemberRetryFinalTimeout(t *testing.T) {
	for _, mode := range []string{"queue", "upstream"} {
		for _, members := range []int{1, 2} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/members=%d/stream=%v", mode, members, stream), func(t *testing.T) {
					fresh(t)
					forgetSinks(t)
					oldHeld, oldWatch := keepHeldAfter, watchEvery
					keepHeldAfter, watchEvery = 10*time.Millisecond, 10*time.Millisecond
					t.Cleanup(func() { keepHeldAfter, watchEvery = oldHeld, oldWatch })
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					first, answer := make(chan struct{}), make(chan struct{})
					var calls atomic.Int32
					target := "a"
					if members == 2 {
						target = "b"
					}
					up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						io.Copy(io.Discard, r.Body)
						if r.Header.Get("Authorization") != "Bearer "+target {
							w.WriteHeader(401)
							io.WriteString(w, `{"error":{"message":"invalid API key"}}`)
							return
						}
						if calls.Add(1) == 1 {
							close(first)
							select {
							case <-answer:
							case <-r.Context().Done():
								return
							}
							w.WriteHeader(503)
							io.WriteString(w, `{"error":{"message":"busy"}}`)
							return
						}
						<-r.Context().Done()
					})))
					defer func() { cancel(); up.Close() }()
					retryGroupWithURL(t, up.URL, members, provider.RetryPolicy{Count: 3, Interval: 1, MaxWait: 2, Cooldown: 600})
					if mode == "queue" {
						n := 1
						if err := provider.Save(provider.Provider{ID: target, Name: target, Key: target, Chat: up.URL, Models: []string{"m"}, MaxConcurrency: &n}); err != nil {
							t.Fatal(err)
						}
					}
					s := New()
					rec := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						defer close(done)
						req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"group/g","stream":%v,"messages":[{"role":"user","content":"hi"}]}`, stream))).WithContext(ctx)
						s.Handler().ServeHTTP(rec, req)
					}()
					select {
					case <-first:
					case <-time.After(5 * time.Second):
						t.Fatal("first attempt never arrived")
					}
					if mode == "queue" {
						occupied := make(chan func(), 1)
						go func() {
							rel, _, err := s.lanes.take(ctx, target+"#"+provider.KeyID(target), 1, 10, time.Minute, nil)
							if err == nil {
								occupied <- rel
							}
						}()
						waitFor(t, "competing turn queued", func() bool { return s.Lanes()[target+"#"+provider.KeyID(target)].Waiting == 1 })
						close(answer)
						select {
						case rel := <-occupied:
							defer rel()
						case <-time.After(5 * time.Second):
							t.Fatal("competing turn never got slot")
						}
						waitFor(t, "retry queued", func() bool { return s.Lanes()[target+"#"+provider.KeyID(target)].Waiting == 1 })
					} else {
						close(answer)
					}
					select {
					case <-done:
					case <-time.After(4 * time.Second):
						cancel()
						<-done
						t.Fatal("MaxWait did not end the retry")
					}
					body := rec.Body.String()
					if !strings.Contains(body, "same-account retry wait limit reached") {
						t.Fatalf("missing timeout error: %d %s", rec.Code, body)
					}
					if stream {
						if rec.Code != 200 || !strings.Contains(body, ": keepalive") {
							t.Fatalf("stream headers never began: %d %s", rec.Code, body)
						}
						var terminal map[string]json.RawMessage
						for _, line := range strings.Split(body, "\n") {
							if strings.HasPrefix(line, "data: ") {
								terminal = nil
								if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &terminal); err != nil {
									t.Fatal(err)
								}
							}
						}
						if terminal["error"] == nil || !strings.Contains(string(terminal["error"]), "same-account retry wait limit reached") {
							t.Fatalf("missing terminal stream error: %s", body)
						}
					} else if rec.Code != 504 {
						t.Fatalf("client status %d, want 504: %s", rec.Code, body)
					}
					route := lastRoute(s)
					if route.Status != 504 || len(route.Tries) == 0 || route.Tries[len(route.Tries)-1].Status != 504 {
						t.Fatalf("trace differs from client timeout: %+v", route)
					}
					want := int32(2)
					if mode == "queue" {
						want = 1
					}
					if calls.Load() != want {
						t.Fatalf("upstream calls %d, want %d", calls.Load(), want)
					}
					if mode == "queue" && s.Lanes()[target+"#"+provider.KeyID(target)].Waiting != 0 {
						t.Fatal("retry left a lane waiter")
					}
				})
			}
		}
	}
}

func TestMemberRetryFirstAttemptHasNoWindow(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	first, answer := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		close(first)
		select {
		case <-answer:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, chatOK)
	})))
	defer up.Close()
	retryGroupWithURL(t, up.URL, 1, provider.RetryPolicy{Count: 1, Interval: 1, MaxWait: 2})
	s := New()
	out := make(chan nearReply, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		s.Handler().ServeHTTP(rec, req)
		out <- nearReply{rec.Code, rec.Body.String()}
	}()
	<-first
	// Deliberately hold the first attempt beyond MaxWait, then let it answer.
	select {
	case got := <-out:
		t.Fatalf("first attempt ended on retry window: %+v", got)
	case <-time.After(2100 * time.Millisecond):
	}
	close(answer)
	select {
	case got := <-out:
		if got.code != 200 || calls.Load() != 1 || lastRoute(s).Status != 200 {
			t.Fatalf("first attempt: %+v, calls %d", got, calls.Load())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first attempt never finished")
	}
}

func TestMemberRetryNearCapWindow(t *testing.T) {
	codexSignedIn(t)
	forgetSinks(t)
	newNearCapBackend(t, map[string]float64{"acct-1": 89}, nil, nil)
	nearCapAccounts(t, 90, 89)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, answer := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			close(first)
			select {
			case <-answer:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"busy"}}`)
	})))
	defer func() { cancel(); up.Close() }()
	oldBase := provider.CodexBase
	provider.CodexBase = up.URL
	defer func() { provider.CodexBase = oldBase }()
	if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: []string{"codex/gpt-5.5"}, Routing: provider.Ordered, Affinity: provider.AffinityOff,
		RetryBeforeFallback: &provider.RetryPolicy{Count: 3, Interval: 1, MaxWait: 2, Cooldown: 600}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	out := make(chan nearReply, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		s.Handler().ServeHTTP(rec, req)
		out <- nearReply{rec.Code, rec.Body.String()}
	}()
	select {
	case <-first:
	case <-time.After(5 * time.Second):
		t.Fatal("first attempt never arrived")
	}
	s.nearCap.mu.Lock()
	var who string
	for id := range s.nearCap.slots {
		who = id
	}
	s.nearCap.mu.Unlock()
	if who == "" {
		t.Fatal("first attempt never held near-cap slot")
	}
	occupied := make(chan func(), 1)
	go func() {
		if rel, err := s.nearCap.take(ctx, who); err == nil {
			occupied <- rel
		}
	}()
	waitFor(t, "competing near-cap turn queued", func() bool { return s.nearCap.waiting() == 1 })
	close(answer)
	select {
	case rel := <-occupied:
		defer rel()
	case <-time.After(5 * time.Second):
		t.Fatal("competing turn never held near-cap slot")
	}
	waitFor(t, "retry queued near cap", func() bool { return s.nearCap.waiting() == 1 })
	select {
	case got := <-out:
		if got.code != 504 || !strings.Contains(got.body, "same-account retry wait limit reached") || lastRoute(s).Status != 504 {
			t.Fatalf("near-cap timeout: %+v", got)
		}
	case <-time.After(4 * time.Second):
		cancel()
		<-out
		t.Fatal("near-cap retry ignored MaxWait")
	}
	if calls.Load() != 1 {
		t.Fatalf("extra upstream calls: %d", calls.Load())
	}
	if s.nearCap.waiting() != 0 {
		t.Fatal("retry left near-cap waiters")
	}
}

// Before any failure, a busy near-cap member still gives way to a ready one.
func TestMemberRetryNearCapFirstAttemptGivesWay(t *testing.T) {
	codexSignedIn(t)
	forgetSinks(t)
	newNearCapBackend(t, map[string]float64{"acct-1": 89}, nil, nil)
	nearCapAccounts(t, 90, 89)
	var calls, fallbackCalls atomic.Int32
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer b" {
			fallbackCalls.Add(1)
			io.WriteString(w, chatOK)
			return
		}
		calls.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"busy"}}`)
	})))
	defer up.Close()
	oldBase := provider.CodexBase
	provider.CodexBase = up.URL
	defer func() { provider.CodexBase = oldBase }()
	if err := provider.Save(provider.Provider{ID: "b", Name: "B", Key: "b", Chat: up.URL, Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: []string{"codex/gpt-5.5", "b/m"},
		Routing: provider.Ordered, Affinity: provider.AffinityOff,
		RetryBeforeFallback: &provider.RetryPolicy{Count: 3, Interval: 1, MaxWait: 2}}); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Find("codex")
	if err != nil {
		t.Fatal(err)
	}
	c := candidate{p: *p, model: "gpt-5.5"}
	if _, near := nearCapOf(c, time.Now()); !near {
		t.Fatal("A is not near its cap")
	}
	s := New()
	release, ok := s.nearCap.tryTake(c.who())
	if !ok {
		t.Fatal("competitor could not occupy A's near-cap slot")
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, done := make(chan nearReply, 1), make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		s.Handler().ServeHTTP(rec, req)
		out <- nearReply{rec.Code, rec.Body.String()}
	}()
	defer func() { cancel(); <-done }()
	// Hold A's slot until cleanup, including after B's complete reply.
	// The timer only bounds a broken implementation; success needs no delay.
	select {
	case got := <-out:
		if got.code != 200 || calls.Load() != 0 || fallbackCalls.Load() != 1 {
			t.Fatalf("first attempt: %+v, A calls %d, B calls %d", got, calls.Load(), fallbackCalls.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("B did not answer while A's near-cap slot stayed occupied; A calls %d, B calls %d, waiters %d",
			calls.Load(), fallbackCalls.Load(), s.nearCap.waiting())
	}
	route := lastRoute(s)
	if len(route.Tries) != 1 || route.Tries[0].ID != "b" || route.Status != 200 {
		t.Fatalf("expected only B's successful first attempt: %+v", route)
	}
	if s.nearCap.waiting() != 0 {
		t.Fatal("first attempt left near-cap waiters")
	}
}

// Losing a near-cap slot to a competing turn must not bypass the retry
// window just because another member could answer.
func TestMemberRetryNearCapBeforeFallback(t *testing.T) {
	codexSignedIn(t)
	forgetSinks(t)
	newNearCapBackend(t, map[string]float64{"acct-1": 89}, nil, nil)
	nearCapAccounts(t, 90, 89)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, answer := make(chan struct{}), make(chan struct{})
	failedAt, fallbackAt := make(chan time.Time, 1), make(chan time.Time, 1)
	var calls, fallbackCalls atomic.Int32
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.Header.Get("Authorization") == "Bearer b" {
			fallbackCalls.Add(1)
			fallbackAt <- time.Now()
			io.WriteString(w, chatOK)
			return
		}
		if calls.Add(1) == 1 {
			close(first)
			select {
			case <-answer:
			case <-r.Context().Done():
				return
			}
			failedAt <- time.Now()
		}
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"message":"busy"}}`)
	})))
	defer func() { cancel(); up.Close() }()
	oldBase := provider.CodexBase
	provider.CodexBase = up.URL
	defer func() { provider.CodexBase = oldBase }()
	if err := provider.Save(provider.Provider{ID: "b", Name: "B", Key: "b", Chat: up.URL, Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	const maxWait = 3
	if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: []string{"codex/gpt-5.5", "b/m"},
		Routing: provider.Ordered, Affinity: provider.AffinityOff,
		RetryBeforeFallback: &provider.RetryPolicy{Count: 3, Interval: 1, MaxWait: maxWait}}); err != nil {
		t.Fatal(err)
	}
	s := New()
	out := make(chan nearReply, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		s.Handler().ServeHTTP(rec, req)
		out <- nearReply{rec.Code, rec.Body.String()}
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("first attempt never arrived")
	}
	s.nearCap.mu.Lock()
	var who string
	for id := range s.nearCap.slots {
		who = id
	}
	s.nearCap.mu.Unlock()
	if who == "" {
		t.Fatal("first attempt never held near-cap slot")
	}
	occupied := make(chan struct{})
	competitorDone := make(chan struct{})
	go func() {
		defer close(competitorDone)
		if rel, err := s.nearCap.take(ctx, who); err == nil {
			defer rel()
			close(occupied)
			<-ctx.Done()
		}
	}()
	defer func() { cancel(); <-competitorDone }()
	waitFor(t, "competing near-cap turn queued", func() bool { return s.nearCap.waiting() == 1 })
	close(answer)
	select {
	case <-occupied:
	case <-time.After(10 * time.Second):
		t.Fatal("competing turn never held near-cap slot")
	}
	// The competitor keeps A's slot until cleanup. Measure at the handlers,
	// not when this goroutine wakes: a loaded runner may observe B late.
	select {
	case at := <-fallbackAt:
		if elapsed := at.Sub(<-failedAt); elapsed < maxWait*time.Second {
			t.Fatalf("B called before A's retry window expired: %s < %s", elapsed, maxWait*time.Second)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("B never took over after A's retry deadline")
	}
	select {
	case got := <-out:
		if got.code != 200 || calls.Load() != 1 || fallbackCalls.Load() != 1 {
			t.Fatalf("fallback: %+v, A calls %d, B calls %d", got, calls.Load(), fallbackCalls.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("fallback never finished")
	}
	route := lastRoute(s)
	if len(route.Tries) != 3 || route.Tries[1].Status != 504 || route.Status != 200 {
		t.Fatalf("expected A's queued retry timeout then B's answer: %+v", route)
	}
	if s.nearCap.waiting() != 0 {
		t.Fatal("retry left near-cap waiters")
	}
}

// Once a retry starts answering, the same deadline must let its body finish.
func TestMemberRetryReplyFinishesPastWindow(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	var calls atomic.Int32
	up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"message":"busy"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-time.After(2100 * time.Millisecond):
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\" finished\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})))
	defer up.Close()
	retryGroupWithURL(t, up.URL, 1, provider.RetryPolicy{Count: 3, Interval: 1, MaxWait: 2})
	s := New()
	code, body := sendTo(s, "/v1/chat/completions", `{"model":"group/g","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || !strings.Contains(body, "finished") || calls.Load() != 2 || lastRoute(s).Status != 200 {
		t.Fatalf("reply cut at retry deadline: %d %s, calls %d", code, body, calls.Load())
	}
}

func TestMemberRetryRemoteRoutedFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		routed bool
		status int
		want   int32
	}{
		{"routed-503", true, 503, 1},
		{"before-routing-503", false, 503, 2},
		{"before-routing-429", false, 429, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			forgetSinks(t)
			var remoteCalls, fallbackCalls atomic.Int32
			up := httptest.NewServer(withoutRootProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				if r.Header.Get("Authorization") == "Bearer b" {
					fallbackCalls.Add(1)
					io.WriteString(w, chatOK)
					return
				}
				remoteCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if tc.routed {
					w.Header().Set(providerHeader, "fake")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, `{"error":{"message":"temporarily unavailable","type":"server_error"}}`)
			})))
			defer up.Close()
			if err := provider.Save(provider.Provider{ID: "office", Name: "Office", Preset: provider.RemoteMagpiePreset,
				Key: "k", Chat: up.URL + "/v1", Models: []string{"fake/m1"}}); err != nil {
				t.Fatal(err)
			}
			if err := provider.Save(provider.Provider{ID: "b", Name: "B", Key: "b", Chat: up.URL, Models: []string{"m"}}); err != nil {
				t.Fatal(err)
			}
			if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: []string{"office/fake/m1", "b/m"},
				Routing: provider.Ordered, Affinity: provider.AffinityOff,
				RetryBeforeFallback: &provider.RetryPolicy{Count: 1, Interval: 1, MaxWait: 30, Cooldown: 600}}); err != nil {
				t.Fatal(err)
			}
			s := New()
			code, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)
			if code != 200 || remoteCalls.Load() != tc.want || fallbackCalls.Load() != 1 {
				t.Fatalf("remote calls %d, want %d; fallback calls %d, want 1; reply %d %s",
					remoteCalls.Load(), tc.want, fallbackCalls.Load(), code, body)
			}
			route := lastRoute(s)
			if len(route.Tries) != int(tc.want)+1 || route.Status != 200 {
				t.Fatalf("remote failure did not fall back as expected: %+v", route)
			}
		})
	}
}
