package panel_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/panel"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestTrashListNoTokensAndPurgeConfirm(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: g}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	tok := "tok_panel"
	raw := []byte("{\"email\":\"a@b.com\",\"access_token\":\"" + tok + "\",\"disabled\":false}")
	meta := state.TrashMeta{ID: "p1", AuthIndex: "a", FileName: "xai-a.json", Email: "a@b.com", TrashedAt: time.Now()}
	if err := tr.MoveToTrash(meta, raw, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/trash")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	b, _ := json.Marshal(body)
	if strings.Contains(string(b), tok) || strings.Contains(string(b), "access_token") {
		t.Fatalf("token leak: %s", b)
	}

	// purge without confirm
	r, _ := http.Post(srv.URL+"/trash/purge", "application/json", bytes.NewReader([]byte(`{"id":"p1"}`)))
	if r.StatusCode == 200 {
		t.Fatal("should require confirm")
	}
	r.Body.Close()

	r, _ = http.Post(srv.URL+"/trash/purge", "application/json", bytes.NewReader([]byte(`{"id":"p1","confirm":true}`)))
	if r.StatusCode != 200 {
		t.Fatalf("status %d", r.StatusCode)
	}
	r.Body.Close()
}

func TestConfigValidateStrips402(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: g}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	payload := `{"auto_delete":true,"delete_signals":["spending_limit_402","auth_401"],"signal_thresholds":{"auth_401":2},"trash_retention_days":7}`
	r, err := http.Post(srv.URL+"/config", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	var out sentrycfg.Config
	_ = json.NewDecoder(r.Body).Decode(&out)
	for _, s := range out.DeleteSignals {
		if s == "spending_limit_402" {
			t.Fatal("402 must be stripped")
		}
	}
}

func TestUIHasCPAMPAccent(t *testing.T) {
	cfg := sentrycfg.Default()
	st := state.New("")
	tr := trash.New(t.TempDir(), 7, true, st)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: guard.New(cfg, st, tr, nil)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/ui")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if !strings.Contains(buf.String(), "#409eff") {
		t.Fatal("missing CPAMP accent")
	}
}

func TestPersistEndpointRedactsRegisterPassword(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	cfg.StatePath = filepath.Join(dir, "state.json")
	cfg.RegisterPassword = "super-secret-register-password"
	st := state.New(cfg.StatePath)
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: guard.New(cfg, st, tr, nil)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()
	p := filepath.Join(dir, "runtime-overrides.json")
	if err := os.WriteFile(p, []byte(`{"register_password":"super-secret-register-password","patrol_mode":"enabled"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(srv.URL + "/persist")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(bytes.Buffer)
	_, _ = b.ReadFrom(resp.Body)
	if strings.Contains(b.String(), "super-secret-register-password") {
		t.Fatalf("password leak: %s", b.String())
	}
}

func TestStateCooldownQuotaFingerprintDisplayUsesLastSignalHTTP(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: guard.New(cfg, st, tr, nil)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	fp := "reason:fp_426test"
	st.Touch("a426")
	st.SetLastSignal("a426", fp)
	st.SetAccountState("a426", state.CooldownQuota, "plugin_auto")
	st.UpsertErrorPolicy(state.ErrorPolicy{Key: fp, Label: "终端版本过低", DisplayMsg: "终端版本过低", Enabled: true})
	st.ObserveError(fp, "终端版本过低", "none", "", `{"error":"cli version outdated"}`, "a426", "xai-a.json", "usage", 426)

	resp, err := http.Get(srv.URL + "/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Accounts []struct {
			AuthIndex string `json:"auth_index"`
			Reason    string `json:"reason"`
			QuotaText string `json:"quota_text"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 {
		t.Fatalf("want one account, got %+v", out.Accounts)
	}
	if out.Accounts[0].Reason != "426·冷却" {
		t.Fatalf("want 426·冷却, got %+v", out.Accounts[0])
	}
	if strings.Contains(out.Accounts[0].Reason, "429") || strings.Contains(out.Accounts[0].QuotaText, "用尽") {
		t.Fatalf("fingerprint cooldown must not look like quota exhaustion: %+v", out.Accounts[0])
	}
}

func TestAccountRecentHumanizesActionReasons(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: guard.New(cfg, st, tr, nil)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	fp := "reason:fp_426recent"
	st.Touch("a426")
	st.UpsertErrorPolicy(state.ErrorPolicy{Key: fp, Label: "终端版本过低", DisplayMsg: "终端版本过低", Enabled: true})
	st.ObserveError(fp, "终端版本过低", "none", "", `{"error":"cli version outdated"}`, "a426", "xai-a.json", "usage", 426)
	st.Log(state.ActionLog{Auth: "a426", Signal: fp, Action: "cooldown", Reason: fp, Source: "usage"})
	st.Log(state.ActionLog{Auth: "a429", Signal: "free_usage_429", Action: "cooldown", Reason: "free_usage_429", Source: "usage"})

	resp, err := http.Get(srv.URL + "/accounts/recent?auth=a426")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Actions []struct {
			Reason string `json:"reason"`
			Signal string `json:"signal"`
		} `json:"actions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Actions) == 0 {
		t.Fatal("want recent actions")
	}
	if out.Actions[0].Reason != "终端版本过低" {
		t.Fatalf("want humanized reason, got %+v", out.Actions[0])
	}
	reasons := make([]string, 0, len(out.Actions))
	for _, act := range out.Actions {
		reasons = append(reasons, act.Reason)
	}
	raw, _ := json.Marshal(reasons)
	if strings.Contains(string(raw), "free_usage_429") || strings.Contains(string(raw), "reason:fp_") {
		t.Fatalf("display reason leaked internal key: %s", raw)
	}
	if out.Actions[0].Signal != fp {
		t.Fatalf("technical signal should remain available, got %+v", out.Actions[0])
	}
}

func TestErrorsListReturnsSplitShapes(t *testing.T) {
	dir := t.TempDir()
	cfg := sentrycfg.Default()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: guard.New(cfg, st, tr, nil)}
	srv := httptest.NewServer(api.Handler())
	defer srv.Close()

	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "merged_426", Label: "终端版本过低", Enabled: true,
		SplitShape: "fp_a", SplitShapes: []string{"fp_a", "fp_b"},
	})
	resp, err := http.Get(srv.URL + "/errors")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Errors []struct {
			Key         string   `json:"key"`
			SplitShape  string   `json:"split_shape"`
			SplitShapes []string `json:"split_shapes"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	for _, e := range out.Errors {
		if e.Key != "merged_426" {
			continue
		}
		if e.SplitShape != "fp_a" || len(e.SplitShapes) != 2 || e.SplitShapes[0] != "fp_a" || e.SplitShapes[1] != "fp_b" {
			t.Fatalf("split shapes not returned: %+v", e)
		}
		return
	}
	t.Fatalf("policy missing from /errors: %+v", out.Errors)
}
