package gateway

// Tests for a group's retryBeforeFallback policy: the same account is asked
// again on a fixed schedule before it rests or another member is asked, the
// wait window covers an upstream still pending, and an exhausted policy
// leaves the account in a fixed cooldown that probes cannot lift.

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

// Ignore local loopback scanners without hiding model requests from assertions.
func withoutRootProbes(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// retryUp counts each key's calls and fails the first failures[id] calls of
// that key with fail, or every one when failures[id] is negative.
type retryUp struct {
	mu                sync.Mutex
	calls             []string
	failures          map[string]int
	fail              int
	body              string
	stall             chan struct{} // when set, stallKey's first call parks here
	stallKey          string
	stalled           chan struct{}
	stalledN          map[string]bool
	stallAfterFailure bool
}

func (u *retryUp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Local port scanners ask GET / without making a model request.
	if r.Method == http.MethodGet && r.URL.Path == "/" {
		http.NotFound(w, r)
		return
	}
	io.Copy(io.Discard, r.Body)
	id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	u.mu.Lock()
	u.calls = append(u.calls, id)
	n := u.failures[id]
	if u.stall != nil && id == u.stallKey && !u.stalledN[id] && (!u.stallAfterFailure || n == 0) {
		u.stalledN[id] = true
		u.mu.Unlock()
		u.stalled <- struct{}{}
		select {
		case <-u.stall:
		case <-r.Context().Done():
		}
		return
	}
	if n != 0 {
		if n > 0 {
			u.failures[id] = n - 1
		}
		fail := u.fail
		body := u.body
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fail)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, body)
		return
	}
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, chatOK)
}

func TestMemberRetryUpIgnoresPortProbes(t *testing.T) {
	up := &retryUp{failures: map[string]int{"a": 1}, fail: 503, body: "busy"}
	ask := func(method, path, key string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		rec := httptest.NewRecorder()
		up.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, key := range []string{"", "a"} {
		if code := ask("GET", "/", key); code != 404 {
			t.Fatalf("port probe answered %d, want 404", code)
		}
	}
	for i, want := range []int{503, 200} {
		if code := ask("POST", "/chat/completions", "a"); code != want {
			t.Fatalf("attempt %d answered %d, want %d", i, code, want)
		}
	}
	ask("POST", "/chat/completions", "")
	// An actual model request missing its account still counts and fails
	// the integration tests' exact call assertions.
	if want := []string{"a", "a", ""}; !reflect.DeepEqual(up.calls, want) {
		t.Fatalf("calls %v, want %v", up.calls, want)
	}
}

func TestMemberRetryProbeFilterKeepsModelCalls(t *testing.T) {
	up := &scripted{replies: []reply{{code: 503, body: "busy"}, {code: 200, body: chatOK}}}
	handler := withoutRootProbes(up)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 404},
		{"POST", "/v1/chat/completions", 503},
		{"GET", "/", 404},
		{"POST", "/v1/chat/completions", 200},
		{"POST", "/v1/chat/completions", 200},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, rec.Code, tc.status)
		}
	}
	if up.n != 3 {
		t.Fatalf("model calls %d, want 3 (including unauthenticated POSTs)", up.n)
	}
}

// The schedule: five additional tries, Interval apart, none starting at or
// past MaxWait.
func TestMemberRetrySchedule(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := newMemberRetry(provider.RetryPolicy{Count: 5, Interval: 15, MaxWait: 300})
	if !m.deadline().IsZero() {
		t.Fatal("the first attempt has a retry deadline")
	}
	now := start
	for i := 0; i < 5; i++ {
		wait, ok := m.pause(now)
		if !ok || wait != 15*time.Second {
			t.Fatalf("retry %d: %s, %v", i+1, wait, ok)
		}
		now = now.Add(wait)
	}
	if _, ok := m.pause(now); ok {
		t.Fatal("a sixth additional retry was allowed")
	}
	// a pause that would end at or past the window fits no more
	m = newMemberRetry(provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: 3})
	if _, ok := m.pause(start); !ok {
		t.Fatal("the first retry must fit")
	}
	if _, ok := m.pause(start.Add(3 * time.Second)); ok {
		t.Fatal("a retry ending on the window's edge was allowed")
	}
}

// Before fallback: rate, a 5xx and a relay quota-pool 429 all reask the
// same account; an invalid request is returned without replay.
func TestMemberRetryBeforeFailover(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail       int
		body       string
		wantStatus int
		wantCalls  []string
	}{
		{"rate-recovers", 429, "slow down", 200, []string{"a", "a"}},
		{"quota-pool-recovers", 429, "upstream account quota usage limit has been reached", 200, []string{"a", "a"}},
		{"error-recovers", 503, "temporarily unavailable", 200, []string{"a", "a"}},
		{"five-retries", 503, "temporarily unavailable", 200, []string{"a", "a", "a", "a", "a", "a", "b"}},
		{"last-no-extra-retries", 429, "slow down", 429, []string{"a", "a", "a", "a", "a", "a"}},
		{"invalid-request", 400, "bad request", 400, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			forgetSinks(t)
			fails := map[string]int{}
			switch tc.name {
			case "five-retries":
				fails["a"], fails["b"] = 6, 0
			case "last-no-extra-retries":
				// every attempt fails: the policy gives six, no seventh
				fails["a"] = 6
			case "invalid-request":
				fails["a"] = -1
			default:
				// one failure, then the policy's retry recovers
				fails["a"] = 1
			}
			up := &retryUp{failures: fails, fail: tc.fail, body: tc.body}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			members := 1
			if tc.name == "five-retries" {
				members = 2
			}
			// invalid requests are never replayed, so they must not exhaust
			// the wait window either
			maxWait := 30
			if tc.name == "invalid-request" {
				maxWait = 1
			}
			retryGroupWithURL(t, srv.URL, members, provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: maxWait, Cooldown: 600})
			s := New()
			status, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`)
			if status != tc.wantStatus {
				t.Fatalf("status %d, want %d: %s", status, tc.wantStatus, body)
			}
			up.mu.Lock()
			got := append([]string(nil), up.calls...)
			up.mu.Unlock()
			if !reflect.DeepEqual(got, tc.wantCalls) {
				t.Fatalf("calls %v, want %v", got, tc.wantCalls)
			}
			route := lastRoute(s)
			if tc.wantStatus == 200 {
				for i, tr := range route.Tries[:min(len(route.Tries), 5)] {
					if tr.Rest != nil {
						t.Fatalf("attempt %d rested before the budget ran out: %+v", i, tr.Rest)
					}
					if i < len(route.Tries)-1 && tr.Again != int64(time.Second/time.Millisecond) {
						t.Fatalf("attempt %d waited %dms, want the fixed interval", i, tr.Again)
					}
				}
			}
		})
	}
}

// retryGroupWithURL is retryGroup against a test server's own URL.
func retryGroupWithURL(t *testing.T, url string, members int, policy provider.RetryPolicy) {
	t.Helper()
	for _, id := range []string{"a", "b"}[:min(members, 2)] {
		if err := provider.Save(provider.Provider{ID: id, Name: strings.ToUpper(id), Key: id, Chat: url, Models: []string{"m"}}); err != nil {
			t.Fatal(err)
		}
	}
	var ms []string
	for _, id := range []string{"a", "b"}[:min(members, 2)] {
		ms = append(ms, id+"/m")
	}
	if err := provider.SaveGroup(provider.Group{ID: "g", Name: "G", Members: ms,
		Routing: provider.Ordered, Affinity: provider.AffinityOff, Sink: true, RetryBeforeFallback: &policy}); err != nil {
		t.Fatal(err)
	}
}

// Once the budget is spent on the last member, the account rests for the
// fixed cooldown and is sunk; the next request skips it for another member,
// and a lone account is told 503 rather than probed.
func TestMemberRetryCooldown(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	up := &retryUp{failures: map[string]int{"a": -1, "b": 0}, fail: 503, body: "busy"}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 2, provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: 30, Cooldown: 600})
	s := New()
	if status, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`); status != 200 {
		t.Fatalf("the second member didn't answer: %d %s", status, body)
	}
	// a fails all six of its attempts and takes its cooldown on the last
	var rest *Rest
	for _, tr := range lastRoute(s).Tries {
		if tr.Rest != nil && tr.Rest.By == "retry-policy" {
			rest = tr.Rest
		}
	}
	if rest == nil || time.Until(rest.Until) < 590*time.Second {
		t.Fatalf("the exhausted account did not take its fixed cooldown: %+v", rest)
	}
	// a second request: a is cooling and must be skipped, b answers at once
	up.mu.Lock()
	before := len(up.calls)
	up.mu.Unlock()
	if status, _ := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"again"}]}`); status != 200 {
		t.Fatal("a cooling member stopped the other from answering")
	}
	up.mu.Lock()
	got := append([]string(nil), up.calls[before:]...)
	up.mu.Unlock()
	if !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("the cooling account was probed: %v", got)
	}
	// an overlapping success cannot lift the fixed rest
	if r, ok := restingOf(candidate{p: provider.Provider{ID: "a"}, rest: "a", model: "m"}); !ok || r.By != "retry-policy" {
		t.Fatalf("the cooldown was lost: %+v %v", r, ok)
	}
}

// A lone account that exhausted its policy with a cooldown is not probed:
// the next request is answered 503 with Retry-After and makes no call.
func TestMemberRetryLoneCooldownNotProbed(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	up := &retryUp{failures: map[string]int{"a": -1}, fail: 503, body: "busy"}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 1, provider.RetryPolicy{Count: 1, Interval: 1, MaxWait: 30, Cooldown: 600})
	s := New()
	if status, _ := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`); status != 503 {
		t.Fatal("a lone account was not held after its policy ran out")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"again"}]}`)))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("the cooling lone account answered %d with no Retry-After: %s", rec.Code, rec.Body)
	}
	up.mu.Lock()
	got := len(up.calls)
	up.mu.Unlock()
	if got != 2 { // the initial attempt and its one retry, no probe
		t.Fatalf("the cooling lone account was probed: %d calls", got)
	}
}

// A fixed cooldown is not shortened by an overlapping request's success.
func TestMemberRetryCooldownStandsOverlapping(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	h := newHoldWriter(httptest.NewRecorder(), true)
	h.WriteHeader(503)
	h.held.WriteString(`{"error":{"message":"busy"}}`)
	s := New()
	c := candidate{p: provider.Provider{ID: "a", Key: "a"}, rest: "a", model: "m"}
	want := s.restMember(c, h, &memberRetry{policy: provider.RetryPolicy{Count: 1, Cooldown: 600}})
	servedCandidate(c, 1) // a concurrent success would clear an ordinary rest
	if got, ok := restingOf(c); !ok || got.By != "retry-policy" || !got.Until.Equal(want.Until) {
		t.Fatalf("an overlapping success changed the fixed cooldown: %+v, ok %v, want %+v", got, ok, want)
	}
}

func TestMemberRetryCooldownCoversAccountModels(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	h := newHoldWriter(httptest.NewRecorder(), true)
	h.WriteHeader(429)
	h.held.WriteString(`{"error":{"message":"slow down"}}`)
	c := candidate{p: provider.Provider{ID: "sub", Account: &provider.Account{User: "one@example.com"}}, model: "opus"}
	want := New().restMember(c, h, &memberRetry{policy: provider.RetryPolicy{Count: 1, Cooldown: 600}})
	c.model = "sonnet"
	if got, ok := retryCooling(c); !ok || !got.Until.Equal(want.Until) {
		t.Fatalf("another model escaped the account cooldown: %+v, %v", got, ok)
	}
	c.p.Account = &provider.Account{User: "two@example.com"}
	if got, ok := retryCooling(c); ok {
		t.Fatalf("another account inherited the cooldown: %+v", got)
	}
}

func TestMemberRetryReplacesAffinityAndPatience(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	up := &retryUp{failures: map[string]int{}, fail: 503, body: "busy"}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 2, provider.RetryPolicy{Count: 1, Interval: 1, MaxWait: 30, Cooldown: 600})
	g, _, _ := provider.FindGroup("group/g")
	g.Affinity, g.Patience = provider.AffinitySession, 120
	if err := provider.SaveGroup(g); err != nil {
		t.Fatal(err)
	}
	s := New()
	const body = `{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`
	if status, reply := postAs(t, s, "kept", body); status != 200 {
		t.Fatalf("initial turn: %d %s", status, reply)
	}
	up.mu.Lock()
	up.failures["a"] = -1
	up.calls = nil
	up.mu.Unlock()
	if status, reply := postAs(t, s, "kept", body); status != 200 {
		t.Fatalf("fallback turn: %d %s", status, reply)
	}
	up.mu.Lock()
	got := append([]string(nil), up.calls...)
	up.failures["b"] = -1
	up.calls = nil
	up.mu.Unlock()
	if !reflect.DeepEqual(got, []string{"a", "a", "b"}) {
		t.Fatalf("affinity stacked extra retries: %v", got)
	}
	if status, _ := postAs(t, s, "kept", body); status != 503 {
		t.Fatalf("exhausted group: %d, want 503", status)
	}
	up.mu.Lock()
	got = append([]string(nil), up.calls...)
	up.mu.Unlock()
	if !reflect.DeepEqual(got, []string{"b", "b"}) {
		t.Fatalf("patience stacked extra rounds: %v", got)
	}
}

// A fallback left out for its smaller context window must not bypass the
// exhausted account's configured cooldown when it is asked as a last resort.
func TestMemberRetrySmallFallbackKeepsCooldown(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	up := &retryUp{failures: map[string]int{"a": -1}, fail: 429, body: "You exceeded your current quota, please check your plan and billing details."}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 2, provider.RetryPolicy{Count: 1, Interval: 1, MaxWait: 30, Cooldown: 600})
	for id, window := range map[string]int{"a": 10000, "b": 2000} {
		p, err := provider.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		p.Contexts = map[string]int{"m": window}
		if err := provider.Save(*p); err != nil {
			t.Fatal(err)
		}
	}
	s := New()
	code, body := postAs(t, s, "", `{"model":"group/g","messages":[{"role":"user","content":"`+strings.Repeat("word ", 1760)+`"}]}`)
	up.mu.Lock()
	got := append([]string(nil), up.calls...)
	up.mu.Unlock()
	if code != 200 || !reflect.DeepEqual(got, []string{"a", "a", "b"}) {
		t.Fatalf("last resort: %d %s, calls %v", code, body, got)
	}
	for _, tr := range lastRoute(s).Tries {
		if tr.ID == "a" && tr.Rest != nil && tr.Rest.By == "retry-policy" {
			return
		}
	}
	t.Fatalf("the smaller fallback bypassed the account cooldown: %+v", lastRoute(s).Tries)
}

// A pending window cancels a queue too; a reply already begun may finish.
func TestMemberRetryWindowStopsQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h := newHoldWriter(httptest.NewRecorder(), true)
	h.stop = cancel
	var lane lanes
	release, _, err := lane.take(ctx, "a", 1, 10, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	finish := h.limitRetry(time.Now().Add(50 * time.Millisecond))
	_, _, err = lane.take(ctx, "a", 1, 10, time.Minute, nil)
	finish()
	if err == nil || !h.retryExpired {
		t.Fatalf("queue was not stopped by the retry window: error %v, expired %v", err, h.retryExpired)
	}
	for _, state := range []string{"passing", "ended", "content"} {
		t.Run(state, func(t *testing.T) {
			h := newHoldWriter(httptest.NewRecorder(), true)
			stopped := false
			h.stop = func() { stopped = true }
			switch state {
			case "passing":
				h.passing = true
			case "ended":
				h.ended = true
			case "content":
				h.stream, h.first.first = true, time.Millisecond
			}
			h.expireRetry()
			if stopped || h.retryExpired {
				t.Fatal("the window stopped a reply already begun")
			}
		})
	}
}

// A request whose upstream stays pending past MaxWait is cut off: the try is
// recorded as a 504 and the next member answers.
func TestMemberRetryWindowAndCancellation(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	stall, stalled := make(chan struct{}), make(chan struct{})
	up := &retryUp{failures: map[string]int{"a": 1}, fail: 503, body: "busy", stallAfterFailure: true, stall: stall, stallKey: "a", stalled: stalled, stalledN: map[string]bool{}}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 2, provider.RetryPolicy{Count: 5, Interval: 1, MaxWait: 3, Cooldown: 600})
	s := New()
	done := make(chan struct{})
	var code int
	go func() {
		defer close(done)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hello"}]}`))
		s.Handler().ServeHTTP(rec, req)
		code = rec.Code
	}()
	<-stalled // a failed once, and its retry is parked
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the request never finished")
	}
	close(stall)
	if code != 200 {
		t.Fatalf("the second member did not answer: %d", code)
	}
	r := lastRoute(s)
	var saw504 bool
	for _, tr := range r.Tries {
		if tr.Status == http.StatusGatewayTimeout {
			saw504 = true
		}
	}
	if !saw504 {
		t.Fatalf("no try recorded the wait limit: %+v", r.Tries)
	}
}

// A client that goes away during a policy's pause makes no further attempt.
func TestMemberRetryCallerCancels(t *testing.T) {
	fresh(t)
	forgetSinks(t)
	up := &retryUp{failures: map[string]int{"a": -1}, fail: 503, body: "busy"}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	retryGroupWithURL(t, srv.URL, 1, provider.RetryPolicy{Count: 5, Interval: 60, MaxWait: 300})
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"group/g","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	up.mu.Lock()
	got := len(up.calls)
	up.mu.Unlock()
	if got != 1 {
		t.Fatalf("the canceled request made %d attempts, want 1", got)
	}
}
