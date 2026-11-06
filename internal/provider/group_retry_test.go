package provider

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestGroupRetryPolicy(t *testing.T) {
	want := RetryPolicy{Count: 5, Interval: 15, MaxWait: 300, Cooldown: 600}
	if err := want.check(); err != nil {
		t.Fatal(err)
	}
	g := Group{RetryBeforeFallback: &want}
	copy := g.own()
	copy.RetryBeforeFallback.Count = 1
	if want.Count != 5 {
		t.Fatal("a copied group changed the original retry policy")
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	var read Group
	if err := json.Unmarshal(b, &read); err != nil || !reflect.DeepEqual(read.RetryBeforeFallback, &want) {
		t.Fatalf("round trip: %+v %v", read, err)
	}
	for _, bad := range []RetryPolicy{
		{Count: -1}, {Count: 101}, {Count: 5, Interval: 0, MaxWait: 300},
		{Count: 5, Interval: 15, MaxWait: 14}, {Count: 5, Interval: 15, MaxWait: 3601},
		{Count: 5, Interval: 15, MaxWait: 300, Cooldown: -1},
	} {
		if bad.check() == nil {
			t.Fatalf("accepted invalid retry policy: %+v", bad)
		}
	}
}
