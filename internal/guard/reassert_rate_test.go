package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// Reassert on sticky-open cool file: rate-limit narrative logs (≤1 full log / 15m).
func TestReassertLogRateLimited(t *testing.T) {
	dir := t.TempDir()
	// Always report enabled so reassert keeps firing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-rate@lovc.eu.cc.json", "auth_index": "rateauth1",
				"email": "rate@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": false,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("rateauth1")
	st.UpdateMeta("rateauth1", "xai-rate@lovc.eu.cc.json", "rate@lovc.eu.cc", "")
	st.SetAccountState("rateauth1", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("rateauth1", time.Now().Add(24*time.Hour))
	// Outside settle window so reassert can fire (LastAction=cooldown would skip 2m)
	st.StampLastAction("rateauth1", "cooldown")
	// force LastActionAt old by logging a cooldown in the past via ActionLog At
	// StampLastAction uses Now; re-stamp with older via Log
	st.Log(state.ActionLog{Auth: "rateauth1", Action: "cooldown", Reason: "setup", At: time.Now().Add(-3 * time.Minute)})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))

	// two ticks close together → at most one narrative reassert/file_still_open log
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, _ := st.SnapshotLogsPage(0, 500)
	nLog := 0
	for _, e := range page {
		if e.Auth != "rateauth1" {
			continue
		}
		switch e.Action {
		case "cooldown_reassert", "cooldown_file_still_open", "cooldown_reassert_failed":
			nLog++
		}
	}
	if nLog > 1 {
		t.Fatalf("want ≤1 reassert narrative log under rate limit, got %d", nLog)
	}
	if nLog < 1 {
		t.Fatalf("want at least 1 reassert outside settle window, got %d", nLog)
	}
}

// Cool-down settle: within 2m of primary cooldown, skip 冷却补关 even if list still open.
func TestReassertSkippedDuringSettleAfterCool(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-settle@lovc.eu.cc.json", "auth_index": "settle1",
				"email": "settle@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": false,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("settle1")
	st.UpdateMeta("settle1", "xai-settle@lovc.eu.cc.json", "settle@lovc.eu.cc", "")
	st.SetAccountState("settle1", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("settle1", time.Now().Add(24*time.Hour))
	// Fresh cool — inside 2m settle
	st.Log(state.ActionLog{Auth: "settle1", Action: "cooldown", Reason: "free_usage", At: time.Now().Add(-30 * time.Second)})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, _ := st.SnapshotLogsPage(0, 200)
	for _, e := range page {
		if e.Auth == "settle1" && (e.Action == "cooldown_reassert" || e.Action == "cooldown_file_still_open") {
			t.Fatalf("unexpected reassert during settle: %+v", e)
		}
	}
}

// After settle window, reassert still fires on open cool file.
func TestReassertAfterSettleWindow(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-late@lovc.eu.cc.json", "auth_index": "late1",
				"email": "late@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": false,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("late1")
	st.UpdateMeta("late1", "xai-late@lovc.eu.cc.json", "late@lovc.eu.cc", "")
	st.SetAccountState("late1", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("late1", time.Now().Add(24*time.Hour))
	st.Log(state.ActionLog{Auth: "late1", Action: "cooldown", Reason: "free_usage", At: time.Now().Add(-3 * time.Minute)})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, _ := st.SnapshotLogsPage(0, 200)
	found := false
	for _, e := range page {
		if e.Auth == "late1" && (e.Action == "cooldown_reassert" || e.Action == "cooldown_file_still_open") {
			found = true
		}
	}
	if !found {
		t.Fatal("want reassert after settle window when file still open")
	}
}

// applyCooldown verifies file closed; mock that stays open records cooldown_file_still_open.
func TestApplyCooldownRecordsFileStillOpen(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			// always open → verify fails
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-open@lovc.eu.cc.json", "auth_index": "open1",
				"email": "open@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": false,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	cfg.AutoCooldown = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("open1")
	st.UpdateMeta("open1", "xai-open@lovc.eu.cc.json", "open@lovc.eu.cc", "")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "open1", FileName: "xai-open@lovc.eu.cc.json", Email: "open@lovc.eu.cc",
		Provider: "xai", StatusCode: 429,
		Body:   `{"code":"subscription:free-usage-exhausted","error":"included free usage"}`,
		Source: "usage",
	})
	// cool state must still be stamped
	if st.Get("open1").State != state.CooldownQuota {
		t.Fatalf("state=%s", st.Get("open1").State)
	}
	page, _ := st.SnapshotLogsPage(0, 200)
	foundCool, foundStill := false, false
	for _, e := range page {
		if e.Auth != "open1" {
			continue
		}
		if e.Action == "cooldown" {
			foundCool = true
		}
		if e.Action == "cooldown_file_still_open" {
			foundStill = true
		}
	}
	if !foundCool {
		t.Fatal("expected cooldown log")
	}
	if !foundStill {
		t.Fatal("expected cooldown_file_still_open after verify list still open")
	}
}

// heal-inflated pending is cleared on tick without waiting 6h.
func TestTickClearsHealInflatedPending(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("h1")
	st.SetAccountState("h1", state.Active, "")
	st.ResetToActive("h1") // sets pending
	// stamp as heal-originated
	st.StampLastAction("h1", "heal_active_file")
	// clear residual signal so hasActiveLadderSignal is false
	st.SetLastSignal("h1", "")
	_ = st.Save()
	if !st.Get("h1").PendingObserve {
		t.Fatal("setup pending")
	}
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), nil)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Get("h1").PendingObserve {
		t.Fatal("heal-inflated pending should be cleared on tick")
	}
}
