package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// Patrol alive (source=patrol, HTTP 200) must exit cool-down and SetDisabled(false).
func TestPatrolAliveReopensCooldown(t *testing.T) {
	dir := t.TempDir()
	var setFalse atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-alive@lovc.eu.cc.json", "auth_index": "alive1",
				"email": "alive@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": true,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setFalse.Add(1)
			}
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
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("alive1")
	st.UpdateMeta("alive1", "xai-alive@lovc.eu.cc.json", "alive@lovc.eu.cc", "")
	st.SetAccountState("alive1", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("alive1", time.Now().Add(24*time.Hour))
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))

	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "alive1", FileName: "xai-alive@lovc.eu.cc.json", Email: "alive@lovc.eu.cc",
		Provider: "xai", StatusCode: 200, Success: true, Body: `{"ok":true}`,
		Source: "patrol",
	}); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("alive1")
	if acc.State != state.Active {
		t.Fatalf("want Active after patrol alive, got %s", acc.State)
	}
	if setFalse.Load() < 1 {
		t.Fatal("want SetDisabled(false) on patrol alive reopen")
	}
	page, _ := st.SnapshotLogsPage(0, 50)
	found := false
	for _, e := range page {
		if e.Action == "patrol_alive_reopen" && e.Auth == "alive1" {
			found = true
		}
	}
	if !found {
		t.Fatal("want patrol_alive_reopen action log")
	}
}

// Permanent range patrol: alive → restore traffic (open file + leave user_manual).
func TestPatrolAliveReopensPermanent(t *testing.T) {
	dir := t.TempDir()
	var setFalse atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setFalse.Add(1)
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("perm1")
	st.UpdateMeta("perm1", "xai-perm@lovc.eu.cc.json", "perm@lovc.eu.cc", "")
	st.SetAccountState("perm1", state.UserManual, "user_manual")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "perm1", FileName: "xai-perm@lovc.eu.cc.json", Email: "perm@lovc.eu.cc",
		Provider: "xai", StatusCode: 200, Success: true, Source: "patrol",
	})
	acc := st.Get("perm1")
	if acc.State != state.Active {
		t.Fatalf("want Active after permanent patrol alive, got %s", acc.State)
	}
	if setFalse.Load() < 1 {
		t.Fatal("want SetDisabled(false) for permanent alive")
	}
	// subsequent policy error should still apply (not stuck permanent forever without re-policy)
}

func TestPatrolAliveReopensPermanentFromAnySelectedRange(t *testing.T) {
	dir := t.TempDir()
	var setFalse atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setFalse.Add(1)
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled, cfg.ManagementURL, cfg.ManagementKey = true, srv.URL, "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("perm-all")
	st.UpdateMeta("perm-all", "xai-perm-all.json", "perm-all@x.test", "")
	st.SetAccountState("perm-all", state.UserManual, "user_manual")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "perm-all", FileName: "xai-perm-all.json", Email: "perm-all@x.test",
		Provider: "xai", StatusCode: 200, Success: true, Source: "patrol",
	}); err != nil {
		t.Fatal(err)
	}
	if st.Get("perm-all").State != state.Active {
		t.Fatal("a selected permanent account with real 2xx must become active")
	}
	if setFalse.Load() == 0 {
		t.Fatal("a selected permanent account with real 2xx must enable auth file")
	}
}

func TestPatrolAliveReopensCandidateRegardlessOfPreviousError(t *testing.T) {
	dir := t.TempDir()
	var setFalse atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setFalse.Add(1)
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled, cfg.ManagementURL, cfg.ManagementKey = true, srv.URL, "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("cand403")
	st.UpdateMeta("cand403", "xai-cand403.json", "cand403@x.test", "")
	st.SetAccountState("cand403", state.CandidateDead, "plugin_auto")
	st.SetLastSignal("cand403", "permission_403")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "cand403", FileName: "xai-cand403.json", Email: "cand403@x.test",
		Provider: "xai", StatusCode: 200, Success: true, Source: "patrol",
	}); err != nil {
		t.Fatal(err)
	}
	if st.Get("cand403").State != state.Active {
		t.Fatal("a selected candidate with real 2xx must become active")
	}
	if setFalse.Load() == 0 {
		t.Fatal("a selected candidate with real 2xx must enable auth file")
	}
}

func TestErrorShapeChangeStartsNewLifecycle(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = false
	st := state.New(filepath.Join(dir, "s.json"))
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), nil)
	permission := `{"code":"permission-denied","error":"Access to the chat endpoint is denied."}`
	gateway := `{"error":"Access Denied"}`
	for i := 0; i < 2; i++ {
		if err := g.HandleUsage(context.Background(), guard.UsageEvent{
			AuthIndex: "shape-switch", FileName: "xai-shape-switch.json", Provider: "xai",
			StatusCode: 403, Body: permission, Source: "usage",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if st.Get("shape-switch").Streaks["permission_403"] != 2 {
		t.Fatalf("permission streak=%v", st.Get("shape-switch").Streaks)
	}
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "shape-switch", FileName: "xai-shape-switch.json", Provider: "xai",
		StatusCode: 403, Body: gateway, Source: "usage",
	}); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("shape-switch")
	if acc.LastSignal == "permission_403" {
		t.Fatalf("shape change must change current signal: %+v", acc)
	}
	if acc.Streaks[acc.LastSignal] != 1 {
		t.Fatalf("new shape must start streak at 1: %+v", acc.Streaks)
	}
	if acc.Streaks["permission_403"] != 0 {
		t.Fatalf("old consecutive streak must clear: %+v", acc.Streaks)
	}
}

// Trashed accounts must never be reopened by patrol alive.
func TestPatrolAliveDoesNotOpenTrashed(t *testing.T) {
	dir := t.TempDir()
	var setFalse atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setFalse.Add(1)
			}
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("t1")
	st.UpdateMeta("t1", "xai-t@lovc.eu.cc.json", "t@lovc.eu.cc", "")
	st.SetAccountState("t1", state.Trashed, "trash")
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "t1", FileName: "xai-t@lovc.eu.cc.json", Email: "t@lovc.eu.cc",
		Provider: "xai", StatusCode: 200, Success: true, Source: "patrol",
	})
	if st.Get("t1").State != state.Trashed {
		t.Fatal("trash must stay")
	}
	if setFalse.Load() != 0 {
		t.Fatal("must not open trashed file")
	}
}
