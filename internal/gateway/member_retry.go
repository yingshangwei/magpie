package gateway

import (
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// memberRetry belongs to one account/model in one request. It never
// rests or sinks the account before the retry budget is exhausted.
type memberRetry struct {
	policy provider.RetryPolicy
	used   int
	until  time.Time
}

func (m *memberRetry) pause(now time.Time) (time.Duration, bool) {
	if m.until.IsZero() {
		m.until = now.Add(time.Duration(m.policy.MaxWait) * time.Second)
	}
	if m.used >= m.policy.Count {
		return 0, false
	}
	wait := time.Duration(m.policy.Interval) * time.Second
	if !now.Add(wait).Before(m.until) {
		return 0, false
	}
	m.used++
	return wait, true
}

// memberRetryable leaves invalid requests, revoked
// credentials and refusals to their existing handling. Only a held
// transient error may be replayed; a reply already sent never is. A 429
// naming quota can be a relay pool that has another account shortly.
func memberRetryable(h *holdWriter) bool {
	if !h.failed() || h.refused || h.turnedAway ||
		protectionRefused(h.code(), h.errBody()) ||
		shapeRefused(h.code(), h.errBody()) {
		return false
	}
	f := failure(h.code(), h.errBody())
	if f != failRate && f != failOther && f != failProxy && !(h.code() == 429 && f == failQuota) {
		return false
	}
	return h.code() == 408 || h.code() == 429 || h.code() >= 500
}

// restMember uses the group's fixed cooldown only after its retry budget.
// Other failures keep the provider's existing rest rules.
func (s *Server) restMember(c candidate, h *holdWriter, m *memberRetry) Rest {
	if m == nil || m.policy.Cooldown == 0 || !(h.retryExpired || memberRetryable(h)) {
		return s.restAfterMarked(c, h.code(), h.header, h.errBody(), h.sharedPool)
	}
	now := time.Now()
	r := Rest{Why: failureOf(c, h.code(), h.errBody()), Status: h.code(),
		By: "retry-policy", Key: c.restKey(),
		Until: now.Add(time.Duration(m.policy.Cooldown) * time.Second)}
	restingUntil.Lock()
	restingUntil.m[r.Key], restingUntil.note[r.Key] = r.Until, r
	restingUntil.Unlock()
	sink(c.restKey(), now)
	return r
}

func retryCooling(c candidate) (Rest, bool) {
	r, ok := restingOf(c)
	return r, ok && r.By == "retry-policy"
}

// limitRetry covers queueing and the upstream wait together. Once a
// reply begins, its body is allowed to finish; it cannot be replayed.
func (h *holdWriter) limitRetry(until time.Time) func() {
	if until.IsZero() {
		return func() {}
	}
	done, over := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(over)
		timer := time.NewTimer(time.Until(until))
		defer timer.Stop()
		select {
		case <-done:
			return
		case <-timer.C:
			h.expireRetry()
		}
	}()
	return func() {
		close(done)
		<-over
	}
}

func (h *holdWriter) expireRetry() {
	h.mu.Lock()
	expired := !h.passing && !h.ended && !(h.stream && h.first.first != 0)
	h.retryExpired = expired
	h.mu.Unlock()
	if expired {
		h.stop()
	}
}
