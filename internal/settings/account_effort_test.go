package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestAccountEffortsSaveRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	const raw = `{"theme":"system","accountEfforts":{"oa/key-1":"xhigh","antigravity/account-2":"xhigh"}}`
	if err := os.WriteFile(Path(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"oa/key-1": "xhigh", "antigravity/account-2": "xhigh"}
	s := Load()
	if !reflect.DeepEqual(s.AccountEfforts, want) {
		t.Fatalf("read account efforts = %v, want %v", s.AccountEfforts, want)
	}
	s.Theme = "dark"
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		AccountEfforts map[string]string `json:"accountEfforts"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(saved.AccountEfforts, want) {
		t.Fatalf("saved account efforts = %v, want %v", saved.AccountEfforts, want)
	}
}
