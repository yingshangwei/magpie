package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

func TestMemberRetrySchedule(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := memberRetry{policy: provider.RetryPolicy{Count: 5, Interval: 15, MaxWait: 300}}
	for i := 0; i < 5; i++ {
		wait, ok := m.pause(now.Add(time.Duration(i) * 15 * time.Second))
		if !ok || wait != 15*time.Second {
			t.Fatalf("retry %d: %s, %v", i+1, wait, ok)
		}
	}
	if _, ok := m.pause(now.Add(75 * time.Second)); ok {
		t.Fatal("more than five additional attempts")
	}
	m = memberRetry{policy: provider.RetryPolicy{Count: 5, Interval: 15, MaxWait: 300}}
	if wait, ok := m.pause(now); !ok || wait != 15*time.Second {
		t.Fatalf("fixed interval: %s %v", wait, ok)
	}
	if _, ok := m.pause(now.Add(290 * time.Second)); ok {
		t.Fatal("a pause would exceed the five-minute window")
	}
}

// The upstream records which member was asked, through the real gateway
// handler, while all credentials and providers live in fresh(t)'s HOME.
func TestMemberRetryBeforeFailover(t *testing.T) {
	for _, tt := range []struct {
		name       string
		fail       int
		failures   int
		members    int
		wantStatus int
		want       []string
	}{
		{"rate-recovers", 429, 1, 2, 200, []string{"a", "a"}},
		{"quota-pool-recovers", 429, 1, 2, 200, []string{"a", "a"}},
		{"error-recovers", 503, 1, 2, 200, []string{"a", "a"}},
		{"five-retries", 503, 6, 2, 200, []string{"a", "a", "a", "a", "a", "a", "b"}},
		{"last-no-extra-retries", 429, 6, 1, 429, []string{"a", "a", "a", "a", "a", "a"}},
		{"quota-pool-exhausted", 429, 6, 1, 429, []string{"a", "a", "a", "a", "a", "a"}},
		{"invalid-request", 400, 6, 2, 400, []string{"a"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fresh(t)
			forgetSinks(t)
			var mu sync.Mutex
			var calls []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				calls = append(calls, id)
				n := len(calls)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if id == "a" && n <= tt.failures {
					w.WriteHeader(tt.fail)
					if strings.HasPrefix(tt.name, "quota-pool") {
						io.WriteString(w, `{"error":{"message":"upstream account quota usage limit has been reached"}}`)
					} else {
						io.WriteString(w, `{"error":{"message":"temporarily unavailable"}}`)
					}
					return
				}
				io.WriteString(w, chatOK)
			}))
			defer up.Close()
			var members []string
			for _, id := range []string{"a", "b"}[:tt.members] {
				if err := provider.Save(provider.Provider{ID: id, Name: id, Key: id, Chat: up.URL, Models: []string{"m"}}); err != nil {
					t.Fatal(err)
				}
				members = append(members, id+"/m")
			}
			g := provider.Group{ID: "g", Name: "g", Members: members, Routing: provider.Ordered,
				Affinity: provider.AffinityOff, Sink: true,
				RetryBeforeFallback: &provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: 30, Cooldown: 600}}
			if err := provider.SaveGroup(g); err != nil {
				t.Fatal(err)
			}
			s := New()
			status, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`)
			mu.Lock()
			got := append([]string(nil), calls...)
			mu.Unlock()
			if status != tt.wantStatus || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("status=%d, calls=%v, want=%v; %s", status, got, tt.want, body)
			}
			route := s.trace.routes[len(s.trace.routes)-1]
			for i, tr := range route.Tries {
				if i < min(tt.failures, 5) && tt.fail != 400 {
					if tr.Again != 1000 || tr.Rest != nil {
						t.Fatalf("attempt %d rested before retry exhaustion: %+v", i, tr)
					}
				}
			}
			if tt.failures >= 6 && tt.fail != 400 && route.Tries[5].Rest == nil {
				t.Fatal("exhausted account did not enter cooldown")
			}
			if tt.failures >= 6 && tt.fail != 400 {
				rest := route.Tries[5].Rest
				if rest.By != "retry-policy" || time.Until(rest.Until) < 590*time.Second || time.Until(rest.Until) > 600*time.Second {
					t.Fatalf("expected ten-minute cooldown: %+v", rest)
				}
				before := len(got)
				status, body = postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"next"}]}`)
				mu.Lock()
				next := append([]string(nil), calls[before:]...)
				mu.Unlock()
				if tt.members == 1 {
					if status != 503 || len(next) != 0 {
						t.Fatalf("cooling member was probed: status=%d calls=%v body=%s", status, next, body)
					}
				} else if status != 200 || !reflect.DeepEqual(next, []string{"b"}) {
					t.Fatalf("cooling member was not skipped: status=%d calls=%v", status, next)
				}
			}
		})
	}
}

func TestMemberRetryWindowAndCancellation(t *testing.T) {
	t.Run("overlapping-result-keeps-cooldown", func(t *testing.T) {
		fresh(t)
		forgetSinks(t)
		c := candidate{p: provider.Provider{ID: "a", Key: "a"}, model: "m"}
		h := newHoldWriter(httptest.NewRecorder(), true)
		h.WriteHeader(503)
		h.Write([]byte(`{"error":{"message":"busy"}}`))
		s := New()
		want := s.restMember(c, h, &memberRetry{policy: provider.RetryPolicy{Cooldown: 600}})
		servedCandidate(c, 1)
		got := s.restAfterMarked(c, 503, nil, []byte(`{"error":{"message":"busy"}}`), false)
		if got.By != "retry-policy" || !got.Until.Equal(want.Until) {
			t.Fatalf("overlapping result changed the cooldown: got=%+v want=%+v", got, want)
		}
		if _, cooling := retryCooling(c); !cooling {
			t.Fatal("overlapping success lifted the cooldown")
		}
	})
	t.Run("pending-retry-reaches-deadline", func(t *testing.T) {
		fresh(t)
		forgetSinks(t)
		var mu sync.Mutex
		var calls []string
		stopped := make(chan struct{})
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			mu.Lock()
			calls = append(calls, id)
			n := len(calls)
			mu.Unlock()
			if id == "a" {
				if n == 1 {
					w.WriteHeader(503)
					io.WriteString(w, `{"error":{"message":"busy"}}`)
				} else {
					<-r.Context().Done()
					close(stopped)
				}
				return
			}
			io.WriteString(w, chatOK)
		}))
		defer up.Close()
		for _, id := range []string{"a", "b"} {
			if err := provider.Save(provider.Provider{ID: id, Key: id, Chat: up.URL, Models: []string{"m"}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := provider.SaveGroup(provider.Group{ID: "g", Members: []string{"a/m", "b/m"},
			Routing: provider.Ordered, Affinity: provider.AffinityOff,
			RetryBeforeFallback: &provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: 3, Cooldown: 600}}); err != nil {
			t.Fatal(err)
		}
		s := New()
		status, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`)
		<-stopped
		mu.Lock()
		defer mu.Unlock()
		if status != 200 || !reflect.DeepEqual(calls, []string{"a", "a", "b"}) {
			t.Fatalf("deadline did not move to next member: status=%d calls=%v body=%s", status, calls, body)
		}
		route := s.trace.routes[len(s.trace.routes)-1]
		if route.Tries[1].Status != 504 || route.Tries[1].Rest == nil || route.Tries[1].Rest.By != "retry-policy" {
			t.Fatalf("deadline missing from route: %+v", route.Tries[1])
		}
	})
	t.Run("deadline-cancels-wait", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		h := newHoldWriter(httptest.NewRecorder(), true)
		h.stop = cancel
		// An error header is not a completed answer: its stalled body
		// must still be covered by the retry deadline.
		h.status = 503
		finish := h.limitRetry(time.Now().Add(-time.Second))
		<-ctx.Done()
		finish()
		if !h.retryExpired {
			t.Fatal("deadline did not cancel the pending upstream")
		}
	})
	t.Run("started-reply-finishes", func(t *testing.T) {
		h := newHoldWriter(httptest.NewRecorder(), true)
		h.passing = true
		h.stop = func() { t.Error("a started reply was canceled") }
		h.expireRetry()
	})
	t.Run("caller-cancels", func(t *testing.T) {
		fresh(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var mu sync.Mutex
		n := 0
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			n++
			mu.Unlock()
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"busy"}}`)
			cancel()
		}))
		defer up.Close()
		if err := provider.Save(provider.Provider{ID: "a", Name: "a", Key: "a", Chat: up.URL, Models: []string{"m"}}); err != nil {
			t.Fatal(err)
		}
		if err := provider.SaveGroup(provider.Group{ID: "g", Members: []string{"a/m"},
			RetryBeforeFallback: &provider.RetryPolicy{Count: 5, Interval: 15, MaxWait: 300}}); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
		New().Handler().ServeHTTP(httptest.NewRecorder(), req)
		mu.Lock()
		defer mu.Unlock()
		if n != 1 {
			t.Fatalf("canceled request made %d attempts", n)
		}
	})
}
