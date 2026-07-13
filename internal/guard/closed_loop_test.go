package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

type fakeCPA struct {
	mu       sync.Mutex
	disabled map[string]bool
	files    []map[string]any
}

func newFakeCPA(initial []map[string]any) (*httptest.Server, *fakeCPA) {
	f := &fakeCPA{disabled: map[string]bool{}, files: initial}
	for _, x := range initial {
		if d, _ := x["disabled"].(bool); d {
			if n, _ := x["name"].(string); n != "" {
				f.disabled[n] = true
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			out := make([]map[string]any, 0, len(f.files))
			for _, x := range f.files {
				cp := map[string]any{}
				for k, v := range x {
					cp[k] = v
				}
				name, _ := cp["name"].(string)
				cp["disabled"] = f.disabled[name]
				out = append(out, cp)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"files": out})
		case r.URL.Path == "/v0/management/auth-files/status":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			name, _ := body["name"].(string)
			if d, ok := body["disabled"].(bool); ok && name != "" {
				f.disabled[name] = d
			}
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	return srv, f
}

func TestClosedLoopCooldownRecover(t *testing.T) {
	srv, fake := newFakeCPA([]map[string]any{{
		"name": "xai-a@x.com.json", "email": "a@x.com", "provider": "xai", "type": "xai", "disabled": false,
	}})
	defer srv.Close()
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.AutoCooldown = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	cfg.AuthDir = dir
	cfg.PermissionCooldownSec = 1
	st := state.New(filepath.Join(dir, "s.json"))
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)

	// force 403 cool-down
	err := g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "hash1", FileName: "xai-a@x.com.json", Email: "a@x.com",
		StatusCode: 403, Body: `{"code":"permission-denied","error":"permission denied"}`,
		Source: "usage",
	})
	// threshold 3 for 403 builtin - call thrice
	_ = err
	for i := 0; i < 3; i++ {
		_ = g.HandleUsage(context.Background(), guard.UsageEvent{
			Provider: "xai", AuthIndex: "hash1", FileName: "xai-a@x.com.json", Email: "a@x.com",
			StatusCode: 403, Body: `{"code":"permission-denied","error":"permission denied"}`,
			Source: "usage",
		})
	}
	acc := st.Get("hash1")
	if acc == nil || acc.State != state.CooldownPermission || acc.DisableSource != "plugin_auto" {
		t.Fatalf("after 403 ladder: %+v", acc)
	}
	if !fake.disabled["xai-a@x.com.json"] {
		t.Fatal("CPA file should be disabled")
	}
	// expire recover
	st.SetRecoverAt("hash1", time.Now().Add(-time.Second))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc = st.Get("hash1")
	if acc.State != state.Active || acc.DisableSource != "" || acc.LastSignal != "" {
		t.Fatalf("after recover want clean active, got state=%s src=%s sig=%s", acc.State, acc.DisableSource, acc.LastSignal)
	}
	if fake.disabled["xai-a@x.com.json"] {
		t.Fatal("CPA file should be re-enabled after recover")
	}
}

func TestClosedLoopManualDisableNeverAutoOpen(t *testing.T) {
	srv, fake := newFakeCPA([]map[string]any{{
		"name": "xai-m@x.com.json", "email": "m@x.com", "provider": "xai", "type": "xai", "disabled": false,
	}})
	defer srv.Close()
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	st.Touch("m1")
	st.UpdateMeta("m1", "xai-m@x.com.json", "m@x.com", "")
	if err := g.ManualDisable(context.Background(), "m1"); err != nil {
		t.Fatal(err)
	}
	if !fake.disabled["xai-m@x.com.json"] {
		t.Fatal("manual disable should set CPA disabled")
	}
	// even with recover_at in past, must not reenable
	st.SetRecoverAt("m1", time.Now().Add(-time.Hour))
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("m1")
	if acc.State != state.UserManual || acc.DisableSource != "user_manual" {
		t.Fatalf("manual disable opened: %+v", acc)
	}
	if !fake.disabled["xai-m@x.com.json"] {
		t.Fatal("CPA file reopened for manual disable")
	}
}

func TestPruneKeepsCooldownOverEmptyActive(t *testing.T) {
	dir := t.TempDir()
	srv, _ := newFakeCPA(nil)
	defer srv.Close()
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	// empty Active shell keyed by email-ish / file name
	st.Touch("xai-dup@x.com.json")
	st.UpdateMeta("xai-dup@x.com.json", "xai-dup@x.com.json", "dup@x.com", "")
	st.SetAccountState("xai-dup@x.com.json", state.Active, "")
	// real cool-down row with hash id
	st.Touch("hashdup")
	st.UpdateMeta("hashdup", "xai-dup@x.com.json", "dup@x.com", "")
	st.SetAccountState("hashdup", state.CooldownQuota, "plugin_auto")
	st.SetRecoverAt("hashdup", time.Now().Add(time.Hour))
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// cool-down row must survive
	if st.Get("hashdup") == nil || st.Get("hashdup").State != state.CooldownQuota {
		t.Fatalf("cool-down row lost: %+v", st.Get("hashdup"))
	}
	// empty shell should be pruned
	if st.Get("xai-dup@x.com.json") != nil {
		t.Fatalf("empty Active shell should be pruned, still: %+v", st.Get("xai-dup@x.com.json"))
	}
}

func TestCanAutoReenablePluginAutoWithoutOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	raw, _ := json.Marshal(map[string]any{
		"version": "1",
		"accounts": map[string]any{
			"a": map[string]any{
				"auth_index": "a", "state": "cooldown_quota", "disable_source": "plugin_auto",
				"owner": "", "recover_at": time.Now().Add(time.Hour).Format(time.RFC3339Nano),
			},
		},
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	st2, err := state.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !st2.CanAutoReenable("a") {
		t.Fatal("plugin_auto cool-down must CanAutoReenable even if Owner empty")
	}
	if st2.CanAutoReenable("missing") {
		t.Fatal("missing must false")
	}
}
