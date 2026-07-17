package state

import (
	"testing"
	"time"
)

func TestPendingSinceSetOnceAndExpireIdle(t *testing.T) {
	s := New("")
	s.SetAccountState("a1", CooldownQuota, "plugin_auto")
	s.SetRecoverAt("a1", time.Now().Add(time.Hour))
	s.SetLastSignal("a1", "free_usage_429")
	s.ResetToActive("a1")
	acc := s.Get("a1")
	if !acc.PendingObserve || acc.PendingSince.IsZero() {
		t.Fatalf("want pending+since after ResetToActive, got pending=%v since=%v", acc.PendingObserve, acc.PendingSince)
	}
	first := acc.PendingSince
	time.Sleep(5 * time.Millisecond)
	// heal re-touch must NOT refresh PendingSince
	s.ResetToActive("a1")
	acc = s.Get("a1")
	if !acc.PendingSince.Equal(first) {
		t.Fatalf("PendingSince refreshed on re-touch: %v vs %v", acc.PendingSince, first)
	}
	// expire idle: clear pending + free_usage residual
	s.ExpireIdlePending("a1")
	acc = s.Get("a1")
	if acc.PendingObserve || !acc.PendingSince.IsZero() {
		t.Fatal("pending should clear")
	}
	if acc.LastSignal != "" {
		t.Fatalf("free_usage residual should clear, got %q", acc.LastSignal)
	}
}

func TestExpireIdleKeeps403Streak(t *testing.T) {
	s := New("")
	s.Touch("a2")
	s.ResetToActive("a2")
	s.SetLastSignal("a2", "permission_403")
	s.IncStreak("a2", "permission_403")
	s.IncStreak("a2", "permission_403")
	s.IncStreak("a2", "permission_403")
	s.ExpireIdlePending("a2")
	acc := s.Get("a2")
	if acc.PendingObserve {
		t.Fatal("pending cleared")
	}
	if acc.Streaks["permission_403"] != 3 {
		t.Fatalf("streak kept, got %v", acc.Streaks)
	}
	if acc.LastSignal != "permission_403" {
		t.Fatalf("403 signal kept, got %q", acc.LastSignal)
	}
}

func TestHealFailStreakEscalatesMark(t *testing.T) {
	s := New("")
	s.Touch("a3")
	s.SetAccountState("a3", Active, "")
	if n := s.IncHealFailStreak("a3"); n != 1 {
		t.Fatalf("n=%d", n)
	}
	if n := s.IncHealFailStreak("a3"); n != 2 {
		t.Fatalf("n=%d", n)
	}
	s.MarkCPAFileDisabled("a3")
	acc := s.Get("a3")
	if acc.State != UserManual || acc.DisableSource != "cpa_file_disabled" {
		t.Fatalf("want CPA已禁用, got state=%s src=%s", acc.State, acc.DisableSource)
	}
	if acc.PendingObserve {
		t.Fatal("pending should clear on mark")
	}
	s.ClearHealFailStreak("a3")
	if s.Get("a3").HealFailStreak != 0 {
		t.Fatal("heal fail streak not cleared")
	}
}
