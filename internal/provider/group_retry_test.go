package provider

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// A group's retry policy is read and written as given, with an owned copy.
func TestGroupRetryPolicy(t *testing.T) {
	want := RetryPolicy{Count: 5, Interval: 15, MaxWait: 300, Cooldown: 600}
	if err := (&want).check(); err != nil {
		t.Fatal(err)
	}
	g := Group{ID: "g", Name: "G", Members: []string{"a/m"}, Routing: Ordered, RetryBeforeFallback: &want}
	copy := g.own()
	copy.RetryBeforeFallback.Count = 1
	if g.RetryBeforeFallback.Count != 5 {
		t.Fatal("own() shared the retry policy with the caller")
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var read Group
	if err := json.Unmarshal(b, &read); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*read.RetryBeforeFallback, want) {
		t.Fatalf("round trip = %+v, want %+v", *read.RetryBeforeFallback, want)
	}
}

// Both configured Opus groups survive a normal read/edit/SaveGroup cycle.
func TestGroupRetryPolicySaveRoundTrip(t *testing.T) {
	effortHome(t)
	const raw = `{"providers":[{"id":"a","name":"A","key":"k","chat":"http://127.0.0.1:1/v1","models":["claude-opus-5-5"]}],
		"groups":[
			{"id":"opus","name":"Opus","members":["a/claude-opus-5-5"],"routing":"order","retryBeforeFallback":{"count":5,"interval":15,"maxWait":300,"cooldown":600}},
			{"id":"opus-priority","name":"Opus Priority","members":["a/claude-opus-5-5"],"routing":"order","retryBeforeFallback":{"count":5,"interval":15,"maxWait":300,"cooldown":600}}
		]}`
	if err := os.WriteFile(Path(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	want := RetryPolicy{Count: 5, Interval: 15, MaxWait: 300, Cooldown: 600}
	for _, id := range []string{"opus", "opus-priority"} {
		g, _, ok := FindGroup("group/" + id)
		if !ok || g.RetryBeforeFallback == nil || *g.RetryBeforeFallback != want {
			t.Fatalf("%s read policy = %+v", id, g.RetryBeforeFallback)
		}
		g.Name += " saved"
		if err := SaveGroup(g); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Groups []Group `json:"groups"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Groups) != 2 {
		t.Fatalf("saved %d groups, want both Opus groups", len(saved.Groups))
	}
	for _, g := range saved.Groups {
		if g.RetryBeforeFallback == nil || *g.RetryBeforeFallback != want {
			t.Fatalf("%s saved policy = %+v", g.ID, g.RetryBeforeFallback)
		}
	}
}

func TestGroupRetryPolicyCheck(t *testing.T) {
	for _, r := range []RetryPolicy{
		{Count: -1, Interval: 1, MaxWait: 1},
		{Count: 101, Interval: 1, MaxWait: 1},
		{Count: 1, Cooldown: -1},
		{Count: 1, Cooldown: 86401},
		{Count: 1, Interval: 0, MaxWait: 1},
		{Count: 1, Interval: 2, MaxWait: 1},
		{Count: 1, Interval: 1, MaxWait: 3601},
	} {
		if err := r.check(); err == nil {
			t.Fatalf("invalid policy accepted: %+v", r)
		}
	}
	// count 0 disables it, and so does no policy at all
	if err := (&RetryPolicy{Count: 0, Interval: 0, MaxWait: 0}).check(); err != nil {
		t.Fatalf("a disabled policy: %v", err)
	}
	if err := (*RetryPolicy)(nil).check(); err != nil {
		t.Fatalf("nil policy: %v", err)
	}
}
