package state

import (
	"testing"
	"time"
)

func TestResetToActiveClosedLoop(t *testing.T) {
	s := New("")
	s.SetAccountState("a1", CooldownQuota, "plugin_auto")
	s.SetRecoverAt("a1", time.Now().Add(time.Hour))
	s.SetLastSignal("a1", "free_usage_429")
	s.IncStreak("a1", "free_usage_429")
	s.ResetToActive("a1")
	acc := s.Get("a1")
	if acc.State != Active {
		t.Fatalf("state=%s", acc.State)
	}
	// cool-down locks cleared
	if acc.DisableSource != "" || !acc.RecoverAt.IsZero() {
		t.Fatalf("cool-down lock not cleared: src=%q recover=%v", acc.DisableSource, acc.RecoverAt)
	}
	// streaks/signal retained for policy ladders across cool-down cycles
	if acc.Streaks["free_usage_429"] != 1 {
		t.Fatalf("streak should persist for ladder, got %v", acc.Streaks)
	}
	if acc.LastSignal != "free_usage_429" {
		t.Fatalf("last_signal should persist, got %q", acc.LastSignal)
	}
}
