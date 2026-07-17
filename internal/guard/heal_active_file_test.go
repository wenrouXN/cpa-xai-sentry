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

// Active + CPA file disabled residual (KPI 未归类): periodic Tick must force-open.
func TestTickHealsActiveFileDisabled(t *testing.T) {
	dir := t.TempDir()
	disabled := true
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-gap@lovc.eu.cc.json", "email": "gap@lovc.eu.cc",
					"auth_index": "hashgap", "provider": "xai", "type": "xai",
					"disabled": disabled,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
				disabled = false
			}
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
	cfg.AuthDir = dir
	cfg.ReopenForeignDisabled = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("hashgap")
	st.UpdateMeta("hashgap", "xai-gap@lovc.eu.cc.json", "gap@lovc.eu.cc", "")
	// residual: sentry Active (after reenable) but file still disabled
	st.SetAccountState("hashgap", state.Active, "")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)

	// periodic Tick (not only manual) must heal
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse < 1 {
		t.Fatalf("want force-open SetDisabled(false), got %d", setToFalse)
	}
	if disabled {
		t.Fatal("file still disabled after heal")
	}
	acc := st.Get("hashgap")
	if acc.State != state.Active {
		t.Fatalf("state=%s", acc.State)
	}
	// v1.1.35: clean Active heal must not invent pending_observe
	if acc.PendingObserve {
		t.Fatal("pending_observe must NOT be set on clean Active heal")
	}
}

// Cool-down must NOT be force-opened by heal.
func TestTickHealSkipsOwnedCooldown(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-cool@lovc.eu.cc.json", "email": "cool@lovc.eu.cc",
					"provider": "xai", "type": "xai", "disabled": true,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
			}
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
	cfg.AuthDir = dir
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("cool1")
	st.UpdateMeta("cool1", "xai-cool@lovc.eu.cc.json", "cool@lovc.eu.cc", "")
	st.SetAccountState("cool1", state.CooldownQuota, "plugin_auto")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("must not open owned cool-down, opens=%d", setToFalse)
	}
	acc := st.Get("cool1")
	if acc.State != state.CooldownQuota {
		t.Fatalf("cool state changed: %s", acc.State)
	}
}

// Fresh manual_enable: skip 强制打开 during 2m settle even if list still shows disabled.
func TestTickHealSkippedDuringSettleAfterManualEnable(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-settleopen@lovc.eu.cc.json", "email": "settleopen@lovc.eu.cc",
					"auth_index": "settleopen1", "provider": "xai", "type": "xai",
					"disabled": true,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
			}
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
	cfg.AuthDir = dir
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("settleopen1")
	st.UpdateMeta("settleopen1", "xai-settleopen@lovc.eu.cc.json", "settleopen@lovc.eu.cc", "")
	st.SetAccountState("settleopen1", state.Active, "")
	st.Log(state.ActionLog{Auth: "settleopen1", Action: "manual_enable", Reason: "panel", At: time.Now().Add(-45 * time.Second)})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "trash"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("heal must skip during open settle, opens=%d", setToFalse)
	}
	// must not burn LastHealAt during settle
	if !st.Get("settleopen1").LastHealAt.IsZero() {
		t.Fatal("settle skip must not TouchLastHealAt")
	}
	page, _ := st.SnapshotLogsPage(0, 100)
	for _, e := range page {
		if e.Auth == "settleopen1" && (e.Action == "heal_active_file" || e.Action == "heal_active_file_failed") {
			t.Fatalf("unexpected heal log during settle: %+v", e)
		}
	}
}

// After open settle window, heal still force-opens sticky closed file.
func TestTickHealAfterOpenSettleWindow(t *testing.T) {
	dir := t.TempDir()
	disabled := true
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-lateopen@lovc.eu.cc.json", "email": "lateopen@lovc.eu.cc",
					"auth_index": "lateopen1", "provider": "xai", "type": "xai",
					"disabled": disabled,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
				disabled = false
			}
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
	cfg.AuthDir = dir
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("lateopen1")
	st.UpdateMeta("lateopen1", "xai-lateopen@lovc.eu.cc.json", "lateopen@lovc.eu.cc", "")
	st.SetAccountState("lateopen1", state.Active, "")
	st.Log(state.ActionLog{Auth: "lateopen1", Action: "manual_enable", Reason: "panel", At: time.Now().Add(-3 * time.Minute)})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "trash"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse < 1 {
		t.Fatalf("want heal after settle, opens=%d", setToFalse)
	}
}
