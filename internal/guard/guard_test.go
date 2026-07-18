package guard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func mustShape(t *testing.T, body string, status int) string {
	t.Helper()
	fp := errorfp.Build(body, status)
	if fp.Shape == "" {
		t.Fatalf("empty shape for %d %s", status, body)
	}
	return fp.Shape
}

func setup(t *testing.T, cfg sentrycfg.Config) (*guard.Guard, *state.Store, string, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	_ = os.MkdirAll(authDir, 0o700)
	_ = os.WriteFile(filepath.Join(authDir, "xai-a.json"), []byte(`{"email":"a@b.com","access_token":"t","disabled":false,"type":"xai"}`), 0o600)

	mux := http.NewServeMux()
	mux.HandleFunc("/v0/management/auth-files/status", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	mux.HandleFunc("/v0/management/auth-files", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	srv := httptest.NewServer(mux)
	st := state.New(filepath.Join(dir, "state.json"))
	tr := trash.New(filepath.Join(dir, "trash"), cfg.TrashRetentionDays, cfg.TrashAutoPurge, st)
	cpa := cpaapi.New(srv.URL, "k", authDir)
	g := guard.New(cfg, st, tr, cpa)
	t.Cleanup(srv.Close)
	return g, st, authDir, srv
}

func TestNonBuiltinFingerprintsDoNotUseLegacyGlobalSignalActions(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"spending", 402, `{"code":"personal-team-blocked:spending-limit","error":"You have run out of credits"}`},
		{"auth", 401, `{"error":"Invalid or expired credentials"}`},
		{"generic403", 403, `{"error":"Access Denied"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := sentrycfg.Default()
			cfg.AutoCooldown, cfg.AutoCandidate, cfg.AutoDelete = true, true, true
			cfg.SignalThresholds["auth_401"] = 1
			g, st, _, _ := setup(t, cfg)
			if err := g.HandleUsage(context.Background(), guard.UsageEvent{
				Provider: "xai", AuthIndex: "a", FileName: "xai-a.json",
				StatusCode: tc.status, Body: tc.body,
			}); err != nil {
				t.Fatal(err)
			}
			if acc := st.Get("a"); acc == nil || (acc.State != state.Active && acc.State != "") {
				t.Fatalf("non-builtin fingerprint without explicit policy must only observe: %+v", acc)
			}
			if len(st.ListTrash()) != 0 {
				t.Fatal("non-builtin fingerprint must not enter trash")
			}
		})
	}
}

func TestUnknownShapesStayUnmatchedUntilSplit(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown, cfg.AutoCandidate, cfg.AutoDelete = true, true, true
	cfg.SignalThresholds["auth_401"] = 1
	g, st, _, _ := setup(t, cfg)
	body := `Post "https://api.x.ai/v1/chat/completions": local error: tls: bad record MAC`
	fp := errorfp.Build(body, 0)

	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "tls-a", FileName: "xai-a.json",
		StatusCode: 0, Body: body, Source: "usage",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetErrorPolicy(fp.SuggestKey); ok {
		t.Fatalf("unknown shape must not seed fingerprint policy %s", fp.SuggestKey)
	}
	obs := st.ListObserved()
	var unmatched *state.ObservedError
	for i := range obs {
		if obs[i].Key == fp.SuggestKey {
			t.Fatalf("unknown shape observed under fingerprint key: %+v", obs[i])
		}
		if obs[i].Key == "unmatched" {
			unmatched = &obs[i]
		}
	}
	if unmatched == nil || unmatched.Count != 1 || unmatched.Shape != fp.Shape || !strings.Contains(unmatched.Sample, "bad record MAC") {
		t.Fatalf("want unmatched observation with shape/sample, got %+v", unmatched)
	}
	if len(unmatched.Hits) != 1 || unmatched.Hits[0].Shape != fp.Shape {
		t.Fatalf("want hit shape preserved, got %+v", unmatched.Hits)
	}
	if acc := st.Get("tls-a"); acc == nil || acc.LastSignal != "unmatched" || acc.Streaks["unmatched"] != 1 {
		t.Fatalf("want unmatched last_signal/streak, got %+v", acc)
	}

	if _, err := st.SplitObservedByShape("unmatched", fp.SuggestKey, "TLS 握手异常", fp.Shape); err != nil {
		t.Fatal(err)
	}
	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "tls-b", FileName: "xai-a.json",
		StatusCode: 0, Body: body, Source: "usage",
	}); err != nil {
		t.Fatal(err)
	}
	if acc := st.Get("tls-b"); acc == nil || acc.LastSignal != fp.SuggestKey || acc.Streaks[fp.SuggestKey] != 1 {
		t.Fatalf("want split key last_signal/streak, got %+v", acc)
	}
	foundSplit := false
	for _, o := range st.ListObserved() {
		if o.Key == fp.SuggestKey {
			foundSplit = true
			if o.Count != 2 || o.Shape != fp.Shape {
				t.Fatalf("want split observation count/shape, got %+v", o)
			}
		}
	}
	if !foundSplit {
		t.Fatalf("split key %s did not receive routed hit", fp.SuggestKey)
	}
}

func TestHTTP426StaysUnmatchedWithoutSplitPolicy(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown = true
	g, st, _, _ := setup(t, cfg)
	body := `{"error":"Your Grok CLI version (none) is outdated. Please update to version 0.1.202 or later"}`
	fp := errorfp.Build(body, 426)

	if err := g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "a426", FileName: "xai-a.json",
		StatusCode: 426, Body: body, Source: "usage",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetErrorPolicy(fp.SuggestKey); ok {
		t.Fatalf("426 must not seed fingerprint policy %s", fp.SuggestKey)
	}
	if acc := st.Get("a426"); acc == nil || acc.State != state.Active || acc.LastSignal != "unmatched" {
		t.Fatalf("426 without split must only observe unmatched, got %+v", acc)
	}
	found := false
	for _, o := range st.ListObserved() {
		if o.Key == "unmatched" {
			found = true
			if o.Shape != fp.Shape {
				t.Fatalf("want 426 shape %s, got %+v", fp.Shape, o)
			}
		}
		if o.Key == fp.SuggestKey {
			t.Fatalf("426 observed under fingerprint key without split: %+v", o)
		}
	}
	if !found {
		t.Fatal("missing unmatched observation")
	}
}

func TestStrictBuiltinsBypassUnmatched(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown = true
	g, st, _, _ := setup(t, cfg)
	cases := []struct {
		auth   string
		status int
		body   string
		key    string
	}{
		{
			auth: "builtin-429", status: 429, key: "free_usage_429",
			body: `{"code":"subscription:free-usage-exhausted","error":"You have reached your included free usage"}`,
		},
		{
			auth: "builtin-403", status: 403, key: "permission_403",
			body: `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`,
		},
	}
	for _, tc := range cases {
		if err := g.HandleUsage(context.Background(), guard.UsageEvent{
			Provider: "xai", AuthIndex: tc.auth, FileName: "xai-a.json",
			StatusCode: tc.status, Body: tc.body, Source: "usage",
		}); err != nil {
			t.Fatal(err)
		}
		if acc := st.Get(tc.auth); acc == nil || acc.LastSignal != tc.key || acc.Streaks[tc.key] != 1 {
			t.Fatalf("want builtin %s last_signal/streak, got %+v", tc.key, acc)
		}
		found := false
		for _, o := range st.ListObserved() {
			if o.Key == tc.key {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing builtin observation %s", tc.key)
		}
	}
}

func Test429CooldownNotTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown = true
	g, st, _, _ := setup(t, cfg)
	err := g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "a", FileName: "xai-a.json",
		StatusCode: 429,
		Body:       `{"code":"subscription:free-usage-exhausted","error":"included free usage"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	acc := st.Get("a")
	if acc.State != state.CooldownQuota {
		t.Fatalf("state=%s", acc.State)
	}
	if len(st.ListTrash()) != 0 {
		t.Fatal("must not trash")
	}
}

func Test402FollowsPolicyNoHardBan(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown = true
	cfg.AutoDelete = true
	g, st, _, _ := setup(t, cfg)
	// v2: 402 is a fingerprint class; explicit policy required for auto cool
	fpBody := `{"code":"personal-team-blocked:spending-limit","error":"need a Grok subscription"}`
	fpKey := "reason:" + mustShape(t, fpBody, 402)
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fpKey, Label: "402", Enabled: true, NeverTrash: false,
		SplitShape:  strings.TrimPrefix(fpKey, "reason:"),
		Escalations: []state.EscalationRule{{Streak: 1, Action: "cooldown", CooldownSec: 3600}},
	})
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "a", FileName: "xai-a.json",
		StatusCode: 402,
		Body:       fpBody,
	})
	acc := st.Get("a")
	if acc == nil || acc.State != state.CooldownSpending {
		got := ""
		if acc != nil {
			got = string(acc.State)
		}
		t.Fatalf("want cooldown_spending (explicit fingerprint policy), got %s", got)
	}
	if p, ok := st.GetErrorPolicy(fpKey); ok && p.NeverTrash {
		t.Fatal("never_trash must not be hard-forced on 402")
	}
}

func TestAuth401RequiresExplicitPolicy(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCandidate = true
	cfg.AutoCooldown = true
	g, st, _, _ := setup(t, cfg)
	body := `{"error":"Invalid or expired credentials (no auth context)"}`
	fpKey := "reason:" + mustShape(t, body, 401)
	// no policy → observe only
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "a", FileName: "xai-a.json", Email: "a@b.com",
		StatusCode: 401, Body: body,
	}
	_ = g.HandleUsage(context.Background(), ev)
	_ = g.HandleUsage(context.Background(), ev)
	if acc := st.Get("a"); acc == nil || (acc.State != state.Active && acc.State != "") {
		t.Fatalf("without policy 401 fingerprint must only observe: %+v", acc)
	}
	// with explicit fingerprint policy → candidate
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fpKey, Label: "凭证失效", Enabled: true, Action: "candidate", Threshold: 2,
		SplitShape:  strings.TrimPrefix(fpKey, "reason:"),
		Escalations: []state.EscalationRule{{Streak: 2, Action: "candidate"}},
	})
	_ = g.HandleUsage(context.Background(), ev)
	_ = g.HandleUsage(context.Background(), ev)
	if acc := st.Get("a"); acc == nil || acc.State != state.CandidateDead {
		got := ""
		if acc != nil {
			got = string(acc.State)
		}
		t.Fatalf("want candidate_dead after explicit policy, got %s", got)
	}
}

func TestSuperNoAutoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.AutoCandidate = true
	g, st, _, _ := setup(t, cfg)
	body := `{"error":"Invalid or expired credentials"}`
	fpKey := "reason:" + mustShape(t, body, 401)
	st.UpsertErrorPolicy(state.ErrorPolicy{
		Key: fpKey, Label: "401", Enabled: true, Action: "trash", Threshold: 1,
		SplitShape:  strings.TrimPrefix(fpKey, "reason:"),
		Escalations: []state.EscalationRule{{Streak: 1, Action: "trash"}},
	})
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "s", FileName: "xai-super-1.json",
		StatusCode: 401, Note: "super", Body: body,
	}
	_ = g.HandleUsage(context.Background(), ev)
	_ = g.HandleUsage(context.Background(), ev)
	if len(st.ListTrash()) != 0 {
		t.Fatal("super protected")
	}
}

func TestUserManualNotReenabled(t *testing.T) {
	cfg := sentrycfg.Default()
	g, st, _, _ := setup(t, cfg)
	st.SetAccountState("m", state.UserManual, "user_manual")
	st.SetRecoverAt("m", time.Now().Add(-time.Minute))
	acc := st.Touch("m")
	acc.FileName = "xai-m.json"
	_ = st.Save()
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st.Get("m").State != state.UserManual {
		t.Fatalf("state=%s", st.Get("m").State)
	}
}

func TestTickPurgeExpiredTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.TrashAutoPurge = true
	cfg.TrashRetentionDays = 7
	past := time.Now().Add(-time.Hour)
	// inject expired trash via MoveToTrash
	dir := t.TempDir()
	st2 := state.New(filepath.Join(dir, "s.json"))
	ts2 := trash.New(filepath.Join(dir, "trash"), 7, true, st2)
	raw := []byte(`{"email":"x"}`)
	meta := state.TrashMeta{ID: "e1", AuthIndex: "e", FileName: "f.json", ExpiresAt: past, TrashedAt: past}
	if err := ts2.MoveToTrash(meta, raw, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	g2 := guard.New(cfg, st2, ts2, nil)
	if err := g2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(st2.ListTrash()) != 0 {
		t.Fatal("expected auto purge")
	}
}
