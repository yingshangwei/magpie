package provider

import "fmt"

// RetryPolicy bounds retries of one account after its first transient
// failure. Count excludes the initial attempt; Interval and MaxWait are
// seconds. Cooldown is the fixed rest after these retries are exhausted,
// or zero to keep the ordinary rest rules. MaxWait includes the pauses and
// upstream waits, not a reply that has already begun. A zero Count disables
// the policy.
type RetryPolicy struct {
	Count    int `json:"count"`
	Interval int `json:"interval"`
	MaxWait  int `json:"maxWait"`
	Cooldown int `json:"cooldown,omitempty"`
}

func (r *RetryPolicy) check() error {
	if r == nil {
		return nil
	}
	if r.Count < 0 || r.Count > 100 {
		return fmt.Errorf("retry count must be between 0 and 100")
	}
	if r.Cooldown < 0 || r.Cooldown > 86400 {
		return fmt.Errorf("retry cooldown must be between 0 and 86400 seconds")
	}
	if r.Count == 0 {
		return nil
	}
	if r.Interval < 1 || r.MaxWait < r.Interval || r.MaxWait > 3600 {
		return fmt.Errorf("retry interval must be at least 1 second and no longer than maxWait, which must be at most 3600 seconds")
	}
	return nil
}
