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

func TestTickReopensUnownedDisabledSelfHeal(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-op@lovc.eu.cc.json", "email": "op@lovc.eu.cc",
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
	cfg.ReopenForeignDisabled = true // explicit opt-in for this test
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("auth1")
	st.UpdateMeta("auth1", "xai-op@lovc.eu.cc.json", "op@lovc.eu.cc", "")
	// previously tagged CPA已禁用 or plain active with file disabled
	st.SetAccountState("auth1", state.UserManual, "cpa_file_disabled")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 1 {
		t.Fatalf("want reopen once, got %d", setToFalse)
	}
	acc := st.Get("auth1")
	if acc.State != state.Active || acc.DisableSource != "" {
		t.Fatalf("want clean active after self-heal, got state=%s src=%s", acc.State, acc.DisableSource)
	}
}

func TestTickProtectsPluginAutoCooldown(t *testing.T) {
	dir := t.TempDir()
	setDisabledTo := map[bool]int{}
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
			if d, ok := body["disabled"].(bool); ok {
				setDisabledTo[d]++
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
	st.Touch("auth-cool")
	st.UpdateMeta("auth-cool", "xai-cool@lovc.eu.cc.json", "cool@lovc.eu.cc", "")
	st.SetAccountState("auth-cool", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("auth-cool", time.Now().Add(2*time.Hour))
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("auth-cool")
	if acc.State != state.CooldownQuota || acc.DisableSource != "plugin_auto" {
		t.Fatalf("plugin_auto cool-down mutated: state=%s src=%s", acc.State, acc.DisableSource)
	}
	if setDisabledTo[false] != 0 {
		t.Fatalf("must not re-enable plugin_auto cool-down file, opens=%d", setDisabledTo[false])
	}
}

func TestTickProtectsPanelManualDisable(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-m@lovc.eu.cc.json", "email": "m@lovc.eu.cc",
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
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("m1")
	st.UpdateMeta("m1", "xai-m@lovc.eu.cc.json", "m@lovc.eu.cc", "")
	st.SetAccountState("m1", state.UserManual, "user_manual")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("panel permanent disable must not reopen, opens=%d", setToFalse)
	}
	acc := st.Get("m1")
	if acc.State != state.UserManual || acc.DisableSource != "user_manual" {
		t.Fatalf("manual lock lost: %+v", acc)
	}
}

func TestTickRepairsActiveWithPluginAutoFutureRecover(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		if r.URL.Path == "/v0/management/auth-files" {
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{}})
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("a")
	st.UpdateMeta("a", "xai-a.json", "a@b.com", "")
	st.SetAccountState("a", state.Active, "plugin_auto")
	st.SetLastSignal("a", "permission_403")
	st.SetRecoverAt("a", time.Now().Add(time.Hour))
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("a")
	if acc.DisableSource != "plugin_auto" {
		t.Fatalf("ownership wiped: src=%s", acc.DisableSource)
	}
	if acc.State != state.CooldownPermission {
		t.Fatalf("want restored cooldown_permission, got %s", acc.State)
	}
}

func TestConservativeModeMarksCPADisabled(t *testing.T) {
	dir := t.TempDir()
	setCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-c@lovc.eu.cc.json", "email": "c@lovc.eu.cc",
					"provider": "xai", "type": "xai", "disabled": true,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			setCalls++
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
	cfg.ReopenForeignDisabled = false // conservative
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("c1")
	st.UpdateMeta("c1", "xai-c@lovc.eu.cc.json", "c@lovc.eu.cc", "")
	st.SetAccountState("c1", state.Active, "")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setCalls != 0 {
		t.Fatalf("conservative must not SetDisabled, calls=%d", setCalls)
	}
	acc := st.Get("c1")
	if acc.State != state.UserManual || acc.DisableSource != "cpa_file_disabled" {
		t.Fatalf("want cpa_file_disabled, got %s/%s", acc.State, acc.DisableSource)
	}
}

func TestTickDoesNotReopenMatchedCooldownByFileName(t *testing.T) {
	// CPA list may omit email; must still match state by xai-<email>.json basename
	// and protect plugin_auto cool-down (the 5w4ggr8txx bug).
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			// no email field — only name (as some CPA list responses)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-5w4ggr8txx@lovc.eu.cc.json",
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
	cfg.ReopenForeignDisabled = true
	st := state.New(filepath.Join(dir, "s.json"))
	// hash auth_index like production; email may be empty briefly but file_name set
	st.Touch("e7a69b89ef4084e7")
	st.UpdateMeta("e7a69b89ef4084e7", "xai-5w4ggr8txx@lovc.eu.cc.json", "5w4ggr8txx@lovc.eu.cc", "")
	st.SetAccountState("e7a69b89ef4084e7", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("e7a69b89ef4084e7", time.Now().Add(24*time.Hour))
	st.SetLastSignal("e7a69b89ef4084e7", "free_usage_429")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("must not reopen owned 429 cool-down (matched by filename), opens=%d", setToFalse)
	}
	acc := st.Get("e7a69b89ef4084e7")
	if acc.State != state.CooldownQuota || acc.DisableSource != "plugin_auto" {
		t.Fatalf("cool-down lost: state=%s src=%s", acc.State, acc.DisableSource)
	}
}

func TestTickMatchesEmailFromFilenameWhenListEmailEmpty(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"name": "/root/.cli-proxy-api/xai-pathuser@lovc.eu.cc.json",
				"type": "xai", "disabled": true,
			}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
			}
			w.WriteHeader(200)
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
	st.Touch("hashpath")
	st.UpdateMeta("hashpath", "xai-pathuser@lovc.eu.cc.json", "pathuser@lovc.eu.cc", "")
	st.SetAccountState("hashpath", state.CooldownPermission, "plugin_auto")
	st.SetRecoverAt("hashpath", time.Now().Add(time.Hour))
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, ts, cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("path+empty-email list must still protect cool-down, opens=%d", setToFalse)
	}
}

func TestTickProtectsCooldownMatchedByAuthIndex(t *testing.T) {
	// CPA list includes auth_index (real API); must protect even if name slightly differs
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-63p73ccpdz@lovc.eu.cc.json",
					"id": "xai-63p73ccpdz@lovc.eu.cc.json",
					"path": "/root/.cli-proxy-api/xai-63p73ccpdz@lovc.eu.cc.json",
					"auth_index": "25a0202c2ec35d69",
					"email": "63p73ccpdz@lovc.eu.cc",
					"account": "63p73ccpdz@lovc.eu.cc",
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
	cfg.ReopenForeignDisabled = true
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("25a0202c2ec35d69")
	st.UpdateMeta("25a0202c2ec35d69", "xai-63p73ccpdz@lovc.eu.cc.json", "63p73ccpdz@lovc.eu.cc", "")
	st.SetAccountState("25a0202c2ec35d69", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("25a0202c2ec35d69", time.Now().Add(24*time.Hour))
	st.SetLastSignal("25a0202c2ec35d69", "free_usage_429")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, ts, cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("auth_index-matched 429 cool-down must not reopen, opens=%d", setToFalse)
	}
	acc := st.Get("25a0202c2ec35d69")
	if acc.State != state.CooldownQuota || acc.DisableSource != "plugin_auto" {
		t.Fatalf("state corrupted: %+v", acc)
	}
}

func TestTickDoesNotReopenWhenSiblingActiveShellExists(t *testing.T) {
	// cool-down on hash id + empty Active shell keyed by filename — must protect file
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-i1bh92sdjf@lovc.eu.cc.json", "auth_index": "de847af412d8414c",
				"email": "i1bh92sdjf@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": true,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
			}
			w.WriteHeader(200)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	cfg.ReopenForeignDisabled = true
	st := state.New(filepath.Join(dir, "s.json"))
	// weak shell
	st.Touch("xai-i1bh92sdjf@lovc.eu.cc.json")
	st.UpdateMeta("xai-i1bh92sdjf@lovc.eu.cc.json", "xai-i1bh92sdjf@lovc.eu.cc.json", "i1bh92sdjf@lovc.eu.cc", "")
	st.SetAccountState("xai-i1bh92sdjf@lovc.eu.cc.json", state.Active, "")
	// real cool-down
	st.Touch("de847af412d8414c")
	st.UpdateMeta("de847af412d8414c", "xai-i1bh92sdjf@lovc.eu.cc.json", "i1bh92sdjf@lovc.eu.cc", "")
	st.SetAccountState("de847af412d8414c", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("de847af412d8414c", time.Now().Add(24*time.Hour))
	st.SetLastSignal("de847af412d8414c", "free_usage_429")
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("sibling Active shell must not cause reopen, opens=%d", setToFalse)
	}
	if st.Get("de847af412d8414c").State != state.CooldownQuota {
		t.Fatalf("cool-down wiped: %+v", st.Get("de847af412d8414c"))
	}
}

func TestTickGraceProtectsRecentCooldownAction(t *testing.T) {
	dir := t.TempDir()
	setToFalse := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-grace@lovc.eu.cc.json", "auth_index": "grace1",
				"email": "grace@lovc.eu.cc", "provider": "xai", "disabled": true, "type": "xai",
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && !d {
				setToFalse++
			}
			w.WriteHeader(200)
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
	st.Touch("grace1")
	st.UpdateMeta("grace1", "xai-grace@lovc.eu.cc.json", "grace@lovc.eu.cc", "")
	// race residue: Active but just logged cooldown
	st.SetAccountState("grace1", state.Active, "")
	st.Log(state.ActionLog{Auth: "grace1", Action: "cooldown", Reason: "free_usage", At: time.Now()})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setToFalse != 0 {
		t.Fatalf("recent cooldown grace must protect, opens=%d", setToFalse)
	}
}

func TestTickReassertsOwnedCooldownIfFileEnabled(t *testing.T) {
	dir := t.TempDir()
	setTo := map[bool]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			// file shows ENABLED but account is cool-down owned
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{
				"name": "xai-i1bh92sdjf@lovc.eu.cc.json", "auth_index": "de847af412d8414c",
				"email": "i1bh92sdjf@lovc.eu.cc", "provider": "xai", "type": "xai", "disabled": false,
			}}})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok {
				setTo[d]++
			}
			w.WriteHeader(200)
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
	st.Touch("de847af412d8414c")
	st.UpdateMeta("de847af412d8414c", "xai-i1bh92sdjf@lovc.eu.cc.json", "i1bh92sdjf@lovc.eu.cc", "")
	st.SetAccountState("de847af412d8414c", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("de847af412d8414c", time.Now().Add(24*time.Hour))
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if setTo[true] == 0 {
		t.Fatalf("must re-disable owned cool-down file, setTo=%v", setTo)
	}
	if setTo[false] != 0 {
		t.Fatalf("must not open, setTo=%v", setTo)
	}
	if st.Get("de847af412d8414c").State != state.CooldownQuota {
		t.Fatalf("state=%s", st.Get("de847af412d8414c").State)
	}
}


func TestDefaultReopenForeignDisabledFalse(t *testing.T) {
	cfg := sentrycfg.Default()
	if cfg.ReopenForeignDisabled {
		t.Fatal("safe default must be reopen_foreign_disabled=false")
	}
}
