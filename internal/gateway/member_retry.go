package gateway

// A group's retryBeforeFallback policy (provider.RetryPolicy): a transient
// 408/429/5xx is replayed on the same account, on a fixed schedule, before
// the account rests, sinks or another member is asked. This file keeps the
// per-request schedule, what may be retried, the fixed cooldown and the
// deadline that covers queueing and the upstream wait together.

import (
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// memberRetry belongs to one account/model in one request. It never
// rests or sinks the account before the retry budget is exhausted.
type memberRetry struct {
	policy provider.RetryPolicy
	used   int
	// start is the first failure that enters this member's retry schedule.
	start time.Time
}

func newMemberRetry(policy provider.RetryPolicy) *memberRetry {
	return &memberRetry{policy: policy}
}

// deadline is when the wait window ends.
func (m *memberRetry) deadline() time.Time {
	if m.start.IsZero() {
		return time.Time{}
	}
	return m.start.Add(time.Duration(m.policy.MaxWait) * time.Second)
}

// pause says how long to wait before another attempt, and whether another
// fits: Count additional tries, each Interval seconds apart, and none
// starting at or past the MaxWait window from the first failure.
func (m *memberRetry) pause(now time.Time) (time.Duration, bool) {
	if m.start.IsZero() {
		m.start = now
	}
	if m.used >= m.policy.Count {
		return 0, false
	}
	wait := time.Duration(m.policy.Interval) * time.Second
	if !now.Add(wait).Before(m.deadline()) {
		return 0, false
	}
	m.used++
	return wait, true
}

// memberRetryable leaves invalid requests, revoked credentials and
// refusals to their existing handling. Only a held transient error may be
// replayed; a reply already sent never is. A 429 naming quota can be a
// relay's pool that has another account shortly.
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

// restMember uses the group's fixed cooldown only after its retry budget
// ran out (or the wait window elapsed) on a failure the policy retries.
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

// retryCooling is the fixed cooldown a retry policy set, while it lasts.
func retryCooling(c candidate) (Rest, bool) {
	r, ok := restingOf(c)
	return r, ok && r.By == "retry-policy"
}

// limitRetry covers queueing and the upstream wait together. Once a reply
// begins, its body is allowed to finish; it cannot be replayed. The
// returned func stops the watch and waits for it to end.
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

// expireRetry marks the wait window elapsed and stops a still-pending try,
// unless its reply has begun: a passing reply, its end already seen, or a
// held stream's first content.
func (h *holdWriter) expireRetry() {
	h.mu.Lock()
	expired := !h.passing && !h.ended && !(h.stream && h.first.first != 0)
	h.retryExpired = expired
	stop := h.stop
	h.mu.Unlock()
	if expired && stop != nil {
		stop()
	}
}
