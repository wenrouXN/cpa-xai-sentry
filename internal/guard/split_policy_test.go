package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// HTTP 426 has no builtin Signal (SignalNone). User-split policy reason:http_426
// with ≥1 permanent disable must still fire.
func TestSplitHTTP426PolicyTriggersDisable(t *testing.T) {
	dir := t.TempDir()
	var setTrue int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/management/auth-files/status" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if d, ok := body["disabled"].(bool); ok && d {
				setTrue++
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
	cfg.ManagementURL = srv.URL
	cfg.ManagementKey = "k"
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("a426")
	st.UpdateMeta("a426", "xai-01d5ie1esu@lovc.eu.cc.json", "01d5ie1esu@lovc.eu.cc", "")
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: "reason:http_426", Label: "终端版本过低", DisplayMsg: "终端版本过低",
		Enabled: true, Action: "disable", Threshold: 1, CountMode: "streak",
		SplitShape: "http_426", Source: "split",
		Escalations: []state.EscalationRule{{Streak: 1, Action: "disable"}},
	})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))

	body := `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later via ` + "`grok update`" + ` or the installation documentation."}`
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		AuthIndex: "a426", FileName: "xai-01d5ie1esu@lovc.eu.cc.json", Email: "01d5ie1esu@lovc.eu.cc",
		Provider: "xai", StatusCode: 426, Success: false, Body: body, Source: "usage",
	}); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("a426")
	if acc == nil || acc.State != state.UserManual {
		t.Fatalf("want user_manual permanent disable after 426 policy, got %+v", acc)
	}
	if setTrue < 1 {
		t.Fatal("want CPA SetDisabled(true) for permanent disable")
	}
	// observed under split key
	found := false
	for _, o := range st.ListObserved() {
		if o.Key == "reason:http_426" && o.Count > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("want observed under reason:http_426")
	}
}
