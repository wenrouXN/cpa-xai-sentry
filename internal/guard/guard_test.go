package guard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/cpaapi"
	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

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

func Test402NeverTrashEvenAutoDelete(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoCooldown = true
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	g, st, _, _ := setup(t, cfg)
	_ = g.HandleUsage(context.Background(), guard.UsageEvent{
		Provider: "xai", AuthIndex: "a", FileName: "xai-a.json",
		StatusCode: 402,
		Body:       `{"code":"personal-team-blocked:spending-limit","error":"need a Grok subscription"}`,
	})
	if len(st.ListTrash()) != 0 {
		t.Fatal("402 never trash")
	}
}

func TestAuth401AutoTrashFree(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	g, st, authDir, _ := setup(t, cfg)
	// force free tier via note
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "a", FileName: "xai-a.json", Email: "a@b.com",
		StatusCode: 401, Note: "free pool",
		Body: `{"error":"Invalid or expired credentials (no auth context)"}`,
	}
	_ = g.HandleUsage(context.Background(), ev)
	_ = g.HandleUsage(context.Background(), ev) // streak 2
	if len(st.ListTrash()) != 1 {
		t.Fatalf("trash=%d", len(st.ListTrash()))
	}
	// live file deleted via API only — auth dir file still exists unless we also remove; ok
	_ = authDir
}

func TestSuperNoAutoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	g, st, _, _ := setup(t, cfg)
	ev := guard.UsageEvent{
		Provider: "xai", AuthIndex: "s", FileName: "xai-super-1.json",
		StatusCode: 401, Note: "super",
		Body: `{"error":"Invalid or expired credentials"}`,
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
