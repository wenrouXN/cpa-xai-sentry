package guard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

// HTTP 426 has no builtin Signal (SignalNone). A user-split fingerprint policy
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
	body := `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later via ` + "`grok update`" + ` or the installation documentation."}`
	fp := errorfp.Build(body, 426)
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fp.SuggestKey, Label: "终端版本过低", DisplayMsg: "终端版本过低",
		Enabled: true, Action: "disable", Threshold: 1, CountMode: "streak",
		SplitShape: fp.Shape, Source: "split",
		Escalations: []state.EscalationRule{{Streak: 1, Action: "disable"}},
	})
	_ = st.Save()
	g := guard.New(cfg, st, trash.New(filepath.Join(dir, "t"), 7, true, st), cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, dir))

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
		if o.Key == fp.SuggestKey && o.Count > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("want observed under fingerprint policy key")
	}
}

func TestMergedSplitPolicyRoutesMultipleShapes(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.SentryEnabled = true
	g, st, _, _ := setup(t, cfg)

	bodyA := `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later"}`
	bodyB := `{"error":"Grok CLI version 0.1.0 is no longer supported. Please update to version 0.2.0"}`
	fpA := errorfp.Build(bodyA, 426)
	fpB := errorfp.Build(bodyB, 426)
	keyA := fpA.SuggestKey
	keyB := fpB.SuggestKey
	if keyA == keyB || fpA.Shape == fpB.Shape {
		t.Fatalf("test bodies must produce different shapes: %s/%s", keyA, keyB)
	}

	st.ObserveError("unmatched", "未分类错误", "none", "", bodyA, "seed-a", "a.json", "usage", 426)
	st.ObserveError("unmatched", "未分类错误", "none", "", bodyB, "seed-b", "b.json", "usage", 426)
	if _, err := st.SplitObservedByShape("unmatched", keyA, "终端版本过低", fpA.Shape); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SplitObservedByShape("unmatched", keyB, "终端版本过低", fpB.Shape); err != nil {
		t.Fatal(err)
	}
	if err := st.ReclassifyErrorKey(keyB, keyA, "终端版本过低"); err != nil {
		t.Fatal(err)
	}
	p, ok := st.GetErrorPolicy(keyA)
	if !ok {
		t.Fatalf("missing merged policy %s", keyA)
	}
	gotShapes := map[string]bool{}
	for _, sh := range p.SplitShapeList() {
		gotShapes[sh] = true
	}
	if !gotShapes[fpA.Shape] || !gotShapes[fpB.Shape] {
		t.Fatalf("merged policy missing routes: %+v policy=%+v", gotShapes, p)
	}

	for _, tc := range []struct {
		auth string
		body string
	}{
		{"route-a", bodyA},
		{"route-b", bodyB},
	} {
		if err := g.HandleUsage(context.Background(), guard.UsageEvent{
			Provider: "xai", AuthIndex: tc.auth, FileName: tc.auth + ".json",
			StatusCode: 426, Success: false, Body: tc.body, Source: "usage",
		}); err != nil {
			t.Fatal(err)
		}
	}
	foundMerged := false
	for _, o := range st.ListObserved() {
		if o.Key == keyB {
			t.Fatalf("old merged key received traffic: %+v", o)
		}
		if o.Key == keyA {
			foundMerged = true
			if o.Count != 4 {
				t.Fatalf("want both new bodies routed to merged key count=4, got %+v", o)
			}
		}
	}
	if !foundMerged {
		t.Fatalf("merged key %s missing after routing", keyA)
	}
}
