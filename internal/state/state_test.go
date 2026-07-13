package state_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
)

func TestStreakAndClear(t *testing.T) {
	s := state.New("")
	s.Touch("a1")
	if n := s.IncStreak("a1", "auth_401"); n != 1 {
		t.Fatal(n)
	}
	if n := s.IncStreak("a1", "auth_401"); n != 2 {
		t.Fatal(n)
	}
	s.ClearAuthStreaks("a1")
	acc := s.Get("a1")
	if acc.Streaks["auth_401"] != 0 {
		t.Fatalf("%v", acc.Streaks)
	}
}

func TestUserManualNeverAutoReenable(t *testing.T) {
	s := state.New("")
	s.SetAccountState("a1", state.UserManual, "user_manual")
	if s.CanAutoReenable("a1") {
		t.Fatal("user_manual must not auto reenable")
	}
	s.SetAccountState("a2", state.CooldownQuota, "plugin_auto")
	if !s.CanAutoReenable("a2") {
		t.Fatal("plugin_auto should reenable")
	}
}

func TestSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s := state.New(path)
	s.Touch("x")
	s.IncStreak("x", "permission_403")
	s.Log(state.ActionLog{Auth: "x", Action: "cooldown", Signal: "permission_403"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Get("x") == nil || s2.Get("x").Streaks["permission_403"] != 1 {
		t.Fatalf("%+v", s2.Get("x"))
	}
	if len(s2.Logs) != 1 {
		t.Fatal(len(s2.Logs))
	}
}

func TestTrashIndex(t *testing.T) {
	s := state.New("")
	s.AddTrash(state.TrashMeta{ID: "t1", AuthIndex: "a", ExpiresAt: time.Now().Add(time.Hour)})
	s.AddTrash(state.TrashMeta{ID: "t1", AuthIndex: "a", Email: "e@x"})
	if len(s.ListTrash()) != 1 {
		t.Fatal("replace same id")
	}
	if s.RemoveTrash("t1") == nil {
		t.Fatal("remove")
	}
	if len(s.ListTrash()) != 0 {
		t.Fatal("empty")
	}
}
