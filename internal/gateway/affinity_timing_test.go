package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// A long reply ends now, but its prompt was cached when the answering
// attempt began. The next user turn must not get another five minutes
// (or hour) of affinity merely because generation took that long.
func TestAffinityLongReplyDoesNotRefreshCache(t *testing.T) {
	for _, tc := range []struct {
		name string
		age  time.Duration
		hour int
		why  string
	}{
		{"five minutes expired", 6 * time.Minute, 0, "cold"},
		{"hour still warm", 20 * time.Minute, 3000, "cache"},
		{"hour expired", 61 * time.Minute, 3000, "cold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh(t)
			c := candidate{rest: "cw", model: "m", p: provider.Provider{ID: "cw", Key: "k"}}
			in := http.Header{"X-Session-Id": {"long-reply"}}
			body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`)
			pick := func() (*Affinity, string) {
				_, _, a, key := affine("cw", "", false, in, provider.Chat, body, []candidate{c}, planned{order: []Weighed{{ID: c.rest}}})
				return a, key
			}
			_, key := pick()
			ended := time.Now()
			started := ended.Add(-tc.age)
			answered(key, c, 1, Usage{CacheWrite: 3000, CacheWrite1h: tc.hour}, started)
			for _, restart := range []bool{false, true} {
				if restart {
					sticks.Lock()
					sticks.m, sticks.from, sticks.mod = map[string]stick{}, "", time.Time{}
					sticks.Unlock()
				}
				a, _ := pick()
				if a.Why != tc.why || a.Kept != (tc.why == "cache") {
					t.Fatalf("restart %v: affinity %+v, want %s", restart, a, tc.why)
				}
				if a.At.Before(ended) || a.CacheWrite != 3000 {
					t.Fatalf("the last answer's time or usage was lost: %+v", a)
				}
			}
		})
	}
}

// Files from before cacheAt keep their old estimate until the next
// answer; a new write keeps the attempt's start separately from At.
func TestAffinityLegacyCacheTime(t *testing.T) {
	fresh(t)
	now := time.Now()
	b, err := json.Marshal(map[string]savedStick{"legacy": {Rest: "cw", Who: "cw", At: now, CacheWrite: 3000}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "cacheAt") {
		t.Fatal("legacy fixture unexpectedly has a cache time")
	}
	if err := os.MkdirAll(filepath.Dir(sticksPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sticksPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	sticks.Lock()
	st, ok := stickOf("legacy")
	sticks.Unlock()
	if !ok || !st.cachedAt().Equal(now) || !cacheWarm(st.cachedAt(), st.hour, now.Add(time.Minute)) || cacheWarm(st.cachedAt(), st.hour, now.Add(6*time.Minute)) {
		t.Fatalf("old file did not retain its estimate: %+v, present %v", st, ok)
	}
}

func TestCacheRenewalUsesRequestStart(t *testing.T) {
	for _, minutes := range []int{59, 61} {
		t.Run(itoa(minutes), func(t *testing.T) {
			fresh(t)
			now := time.Now()
			first := now.Add(-65 * time.Minute)
			started := first.Add(time.Duration(minutes) * time.Minute)
			c := candidate{rest: "cw", model: "m"}
			answered("renewal", c, 1, Usage{CacheWrite: 3000, CacheWrite1h: 3000}, first)
			turnRules.m["renewal"] = turnRule{effort: "high", cacheAt: first, cacheWho: c.who(), cacheModel: c.model, hour: hourCache{3000, first}}
			answered("renewal", c, 2, Usage{CacheRead: 3000}, started)
			ruleAnswered("renewal", c, Usage{CacheRead: 3000}, started)
			sticks.Lock()
			st := sticks.m["renewal"]
			sticks.Unlock()
			turnRules.Lock()
			tr := turnRules.m["renewal"]
			turnRules.Unlock()
			wantWarm := minutes == 59
			if st.hour.warm(now.Add(10*time.Minute)) != wantWarm || keepsEffort(tr, "low", 3000, now.Add(10*time.Minute)) != wantWarm {
				t.Fatalf("started after %d minutes, finished after 65: affinity hour %+v, rule %+v", minutes, st.hour, tr)
			}
		})
	}
}

// The latest cache observation wins within an account and model, even
// when an earlier request finishes last. Another account or model has
// no claim to that observation or its one-hour cache.
func TestCacheObservationsFinishingOutOfOrder(t *testing.T) {
	for _, scope := range []string{"same", "account", "model"} {
		t.Run(scope, func(t *testing.T) {
			fresh(t)
			now := time.Now()
			newer, older := now.Add(-time.Minute), now.Add(-2*time.Minute)
			c := candidate{rest: "cw", model: "m"}
			turnRules.m["overlap"] = turnRule{effort: "high"}
			u := Usage{CacheWrite: 3000, CacheWrite1h: 3000}
			answered("overlap", c, 2, u, newer)
			ruleAnswered("overlap", c, u, newer)
			if scope == "account" {
				c.rest = "other"
			} else if scope == "model" {
				c.model = "other"
			}
			answered("overlap", c, 1, Usage{Input: 10}, older)
			ruleAnswered("overlap", c, Usage{Input: 10}, older)
			st, tr := sticks.m["overlap"], turnRules.m["overlap"]
			want := older
			if scope == "same" {
				want = newer
			}
			if !st.cachedAt().Equal(want) || !tr.cacheAt.Equal(want) || st.hour.warm(now.Add(10*time.Minute)) != (scope == "same") || keepsEffort(tr, "low", 3000, now.Add(10*time.Minute)) != (scope == "same") {
				t.Fatalf("%s: affinity %+v, rule %+v", scope, st, tr)
			}
			if scope == "same" && st.cacheWrite != 3000 {
				t.Fatalf("older completion replaced newer usage: %+v", st)
			}
		})
	}
}

func TestEffortLongReplyDoesNotRefreshCache(t *testing.T) {
	fresh(t)
	now := time.Now()
	for _, tc := range []struct {
		name string
		age  time.Duration
		hour int
		warm bool
	}{
		{"short cache expired", 6 * time.Minute, 0, false},
		{"hour cache warm", 20 * time.Minute, 3000, true},
		{"hour cache expired", 61 * time.Minute, 3000, false},
	} {
		turnRules.m[tc.name] = turnRule{at: now, effort: "high"}
		ruleAnswered(tc.name, candidate{rest: "cw", model: "m"}, Usage{CacheWrite: 3000, CacheWrite1h: tc.hour}, now.Add(-tc.age))
		if tr := turnRules.m[tc.name]; keepsEffort(tr, "low", 3000, now) != tc.warm {
			t.Errorf("%s: recent completion kept stale effort: %+v", tc.name, tr)
		}
	}
	if cacheWarm(now.Add(time.Minute), hourCache{3000, now.Add(time.Minute)}, now) {
		t.Error("a future cache observation was treated as warm")
	}
}

// Use barriers, not elapsed-time thresholds: the answering attempt must
// start after its queue (or failed predecessor) and before its reply ends.
func TestCacheClockFollowsAnsweringAttempt(t *testing.T) {
	for _, kind := range []string{"queue", "retry"} {
		t.Run(kind, func(t *testing.T) {
			fresh(t)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			arrived := make(chan time.Time, 2)
			continueReply := make(chan struct{}, 2)
			v := &cacheWriter{wrote: 3000}
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				arrived <- time.Now()
				select {
				case <-continueReply:
				case <-ctx.Done():
					return
				}
				if kind == "retry" && r.Header.Get("x-api-key") == "k1" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":{"type":"api_error","message":"unavailable"}}`))
					return
				}
				v.ServeHTTP(w, r)
			}))
			t.Cleanup(up.Close)
			one := 1
			p := provider.Provider{ID: "cw", Name: "CW", Key: "k1", Models: []string{"m"}, Anthropic: up.URL + "/v1", MaxConcurrency: &one}
			if kind == "retry" {
				p.Keys = []provider.KeyAccount{{Key: "k2"}}
			}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			s := New()
			who := (candidate{p: p}).who()
			release := func() {}
			if kind == "queue" {
				var ok bool
				release, ok = s.lanes.acquire(ctx, who, 1)
				if !ok {
					t.Fatal("could not occupy the fixture lane")
				}
			}
			defer release()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"cw/m","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
				req.Header.Set("x-session-id", "timing")
				s.Handler().ServeHTTP(rec, req)
				done <- rec
			}()
			t.Cleanup(func() { cancel(); <-done })
			if kind == "queue" {
				tick := time.NewTicker(time.Millisecond)
				defer tick.Stop()
				for {
					s.lanes.mu.Lock()
					queued := len(s.lanes.m[who].queue) > 0
					s.lanes.mu.Unlock()
					if queued {
						break
					}
					select {
					case <-tick.C:
					case <-ctx.Done():
						t.Fatal("request never queued")
					}
				}
			} else {
				select {
				case <-arrived:
				case <-ctx.Done():
					t.Fatal("first attempt never arrived")
				}
			}
			ready := time.Now()
			if kind == "queue" {
				release()
			} else {
				continueReply <- struct{}{}
			}
			var sent time.Time
			select {
			case sent = <-arrived:
			case <-ctx.Done():
				t.Fatal("answering attempt never arrived")
			}
			ending := time.Now()
			continueReply <- struct{}{}
			rec := <-done
			done <- rec // cleanup always waits for the request, including failures above
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			sticks.Lock()
			defer sticks.Unlock()
			if len(sticks.m) != 1 {
				t.Fatalf("%d conversations, want 1", len(sticks.m))
			}
			for _, st := range sticks.m {
				if st.cachedAt().Before(ready) || st.cachedAt().After(sent) || st.at.Before(ending) {
					t.Fatalf("%s: cache at %v, must be in [%v, %v]; answered %v, earliest %v", kind, st.cachedAt(), ready, sent, st.at, ending)
				}
			}
		})
	}
}
