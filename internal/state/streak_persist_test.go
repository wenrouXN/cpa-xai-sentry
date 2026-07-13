package state

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResetToActiveKeepsStreaks(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "s.json"))
	s.Touch("a")
	s.IncStreak("a", "permission_403")
	s.IncStreak("a", "permission_403")
	s.IncStreak("a", "permission_403")
	s.SetAccountState("a", CooldownPermission, "plugin_auto")
	s.SetRecoverAt("a", time.Now().Add(-time.Second))
	s.ResetToActive("a")
	acc := s.Get("a")
	if acc.State != Active {
		t.Fatalf("state=%s", acc.State)
	}
	if acc.DisableSource != "" {
		t.Fatalf("src=%s", acc.DisableSource)
	}
	if acc.Streaks["permission_403"] != 3 {
		t.Fatalf("streak wiped: %v", acc.Streaks)
	}
	// next failure should reach 4
	n := s.IncStreak("a", "permission_403")
	if n != 4 {
		t.Fatalf("want 4 got %d", n)
	}
}

func TestClearManualLockClearsStreaks(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "s.json"))
	s.Touch("a")
	s.IncStreak("a", "permission_403")
	s.IncStreak("a", "permission_403")
	s.ClearManualLock("a")
	acc := s.Get("a")
	if len(acc.Streaks) != 0 && acc.Streaks["permission_403"] != 0 {
		t.Fatalf("manual enable should clear streaks: %v", acc.Streaks)
	}
}
