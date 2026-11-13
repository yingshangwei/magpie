package provider

import (
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

func TestAccountEffortSelectedAccount(t *testing.T) {
	effortHome(t)
	first := Provider{ID: "oa", Key: "first-key"}
	second := Provider{ID: "oa", Key: "second-key"}
	login := Provider{ID: "antigravity", Account: &Account{User: "one@example.com"}}
	cfg := settings.Load()
	cfg.AccountEfforts = map[string]string{
		first.ID + "/" + first.AccountID():   "xhigh",
		second.ID + "/" + second.AccountID(): "max",
		login.ID + "/" + login.AccountID():   "xhigh",
		"invalid/" + first.AccountID():       "nonsense",
	}
	if err := settings.Save(cfg); err != nil {
		t.Fatal(err)
	}
	Changed()
	for _, tc := range []struct {
		p    Provider
		want string
	}{
		{first, "xhigh"},
		{second, "max"},
		{login, "xhigh"},
		{Provider{ID: "missing", Key: first.Key}, ""},
		{Provider{ID: "invalid", Key: first.Key}, ""},
		{Provider{ID: "oa"}, ""},
	} {
		if got := AccountEffort(tc.p); got != tc.want {
			t.Fatalf("%s/%s effort = %q, want %q", tc.p.ID, tc.p.AccountID(), got, tc.want)
		}
	}
}
