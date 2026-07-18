package state_test

import (
	"os"
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

func TestLoadV3ReclassifiesCooldownStates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"version":"1","accounts":{"a":{"auth_index":"a","state":"cooldown_quota","last_signal":"http_426","streaks":{"http_426":3},"day_calls":9}},"logs":[{"auth":"a","action":"cooldown"}],"trash":[{"id":"t"}],"error_policies":{"http_426":{"key":"http_426"}},"hidden_policy_keys":["http_426"],"observed_errors":{"http_426":{"key":"http_426","count":2}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := s.Get("a")
	if s.Version != "3" || len(s.ErrorPolicies) != 1 || len(s.Observed) != 1 || len(s.HiddenPolicyKeys) != 1 {
		t.Fatalf("migration mismatch: version=%s policies=%d observed=%d hidden=%d", s.Version, len(s.ErrorPolicies), len(s.Observed), len(s.HiddenPolicyKeys))
	}
	if a == nil || a.State != state.CooldownPolicy || a.DayCalls != 9 || a.LastSignal != "http_426" || a.Streaks["http_426"] != 3 {
		t.Fatalf("account migration mismatch: %+v", a)
	}
	if len(s.Logs) != 1 || len(s.Trash) != 1 {
		t.Fatalf("operational history lost: logs=%d trash=%d", len(s.Logs), len(s.Trash))
	}
	// re-load from disk: must already be v3 without replaying migration
	s2, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Version != "3" || s2.Get("a").State != state.CooldownPolicy {
		t.Fatalf("migration must be persisted: version=%s policies=%d", s2.Version, len(s2.ErrorPolicies))
	}
}

func TestLoadV3CooldownClassMatrix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"version":"2","accounts":{
		"free":{"auth_index":"free","state":"cooldown_policy","last_signal":"free_usage_429"},
		"spend":{"auth_index":"spend","state":"cooldown_quota","last_signal":"spending_limit_402"},
		"perm":{"auth_index":"perm","state":"cooldown_quota","last_signal":"permission_403"},
		"auth":{"auth_index":"auth","state":"cooldown_quota","last_signal":"auth_401"},
		"fp":{"auth_index":"fp","state":"cooldown_quota","last_signal":"reason:fp_abc"},
		"empty":{"auth_index":"empty","state":"cooldown_quota"},
		"cand":{"auth_index":"cand","state":"candidate_dead","last_signal":"permission_403"}
	}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]state.AccountState{
		"free":  state.CooldownQuota,
		"spend": state.CooldownSpending,
		"perm":  state.CooldownPermission,
		"auth":  state.CooldownAuth,
		"fp":    state.CooldownPolicy,
		"empty": state.CooldownPolicy,
		"cand":  state.CandidateDead,
	}
	for id, st := range want {
		if got := s.Get(id); got == nil || got.State != st {
			t.Fatalf("%s want %s got %+v", id, st, got)
		}
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
