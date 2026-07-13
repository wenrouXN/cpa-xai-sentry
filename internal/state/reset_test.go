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
	if acc.DisableSource != "" || acc.LastSignal != "" || !acc.RecoverAt.IsZero() {
		t.Fatalf("not clean: src=%q sig=%q recover=%v", acc.DisableSource, acc.LastSignal, acc.RecoverAt)
	}
	if len(acc.Streaks) != 0 {
		t.Fatalf("streaks=%v", acc.Streaks)
	}
}
