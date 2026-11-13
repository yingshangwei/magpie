package provider

// Antigravity's built-in code still serves its accounts, so the hourly
// MoveRetiring must not move it onto the plugin: the user keeps the
// configuration as it is until they move it by hand (Adopt/Move) or install
// the plugin themselves (HandOver). Retired built-ins keep their automatic
// move, and a failed move of one of those still retries.

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The maintenance gate must leave even the account reader of a live
// built-in untouched. Reading accounts is unnecessary before opting in.
func TestMoveRetiringSkipsActiveBuiltin(t *testing.T) {
	claudeHome(t)
	was, wasRetiring := movers["antigravity"], retiring
	calls := 0
	movers["antigravity"] = &mover{
		out: func() ([]Moving, error) {
			calls++
			return nil, errors.New("the active built-in must not be inspected")
		},
	}
	retiring = func() []string { return []string{"antigravity"} }
	t.Cleanup(func() {
		movers["antigravity"], retiring = was, wasRetiring
	})
	if BuiltinRetired("antigravity") {
		t.Fatal("this fixture requires the Antigravity built-in")
	}
	if errs := MoveRetiring(context.Background()); len(errs) != 0 || calls != 0 {
		t.Fatalf("active Antigravity inspected %d times, errors %v", calls, errs)
	}
}

func TestMoveRetiringLeavesBuiltInAntigravity(t *testing.T) {
	googleSandbox(t, &fakeGoogle{})
	ForgetAccounts()
	for _, user := range []string{"one@example.com", "two@example.com"} {
		if err := addGoogleLogin("antigravity", user, "", googleAuth{
			AccessToken: "at-" + user, RefreshToken: "rt-" + user, Project: "project-" + user,
			Expiry: time.Now().Add(time.Hour).UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if accts, err := movers["antigravity"].out(); err != nil || len(accts) != 2 {
		t.Fatalf("the mover sees %d accounts: %v", len(accts), err)
	}
	if errs := MoveRetiring(context.Background()); len(errs) != 0 {
		t.Fatalf("MoveRetiring moved something despite the withheld built-in: %v", errs)
	}
	if _, moved := MigrationOf("antigravity"); moved {
		t.Fatal("Antigravity was migrated without the user choosing the plugin")
	}
	if ls := googleLogins("antigravity"); len(ls) != 2 {
		t.Fatalf("the built-in's logins changed: %d left", len(ls))
	}
	// the manual move and the plugin handover still own the id: their
	// paths below don't read BuiltinRetired, so they are unchanged
	if !Movable("antigravity") {
		t.Fatal("Antigravity lost its mover")
	}
}

// A retired built-in with accounts is still moved automatically, so the
// withheld path is specific to BuiltinRetired being false.
func TestMoveRetiringStillMovesRetiredBuiltIn(t *testing.T) {
	ctx, _, installs := fakePlugin(t)
	retiredBuiltins["fakeco"] = true
	t.Cleanup(func() { delete(retiredBuiltins, "fakeco") })
	retiring = func() []string { return []string{"fakeco"} }
	t.Cleanup(func() { retiring = MovableIDs })
	setFakeLogins(t, fakeLogin("a@fake", "r-a", true, true))
	was := *installs
	// MoveRetiring records every attempted move, including nil on success.
	errs := MoveRetiring(ctx)
	err, attempted := errs["fakeco"]
	if len(errs) != 1 || !attempted || err != nil || *installs != was+1 || !Moved("fakeco") {
		t.Fatalf("a retired built-in was not moved: %v, %d installs", errs, *installs-was)
	}
}

// The shared mover also stands in for live built-ins exercising HandOver.
func TestMoveFakeMoverStartsLive(t *testing.T) {
	claudeHome(t)
	fakeMover(t)
	if BuiltinRetired("fakeco") {
		t.Fatal("the shared fake mover unexpectedly retires a live built-in")
	}
}
