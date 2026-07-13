package patrol_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func TestProbeClassifies402NoTrash(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"code":"personal-team-blocked:spending-limit","error":"need a Grok subscription"}`))
	}))
	defer upstream.Close()

	cfg := sentrycfg.Default()
	cfg.PatrolEnabled = true
	cfg.AutoCooldown = true
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"spending_limit_402", "auth_401"}
	cfg.PatrolBatchSize = 10
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	r := patrol.New(cfg, g, nil)

	results := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "a", FileName: "xai-a.json", Provider: "xai",
		BaseURL: upstream.URL, Token: "t", Note: "free",
	}})
	if len(results) != 1 || results[0].StatusCode != 402 {
		t.Fatalf("%+v", results)
	}
	if len(st.ListTrash()) != 0 {
		t.Fatal("network/probe 402 must not trash")
	}
	// cooldown may apply
	acc := st.Get("a")
	if acc == nil {
		t.Fatal("missing account state")
	}
}

func TestNetworkErrorNoTrash(t *testing.T) {
	cfg := sentrycfg.Default()
	cfg.PatrolEnabled = true
	cfg.AutoDelete = true
	cfg.DeleteSignals = []string{"auth_401"}
	cfg.PatrolTimeout = 1
	dir := t.TempDir()
	st := state.New(filepath.Join(dir, "s.json"))
	tr := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	g := guard.New(cfg, st, tr, nil)
	r := patrol.New(cfg, g, nil)
	// closed server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	res := r.Run(context.Background(), []patrol.Target{{
		AuthIndex: "n", FileName: "xai-n.json", Provider: "xai", BaseURL: url,
	}})
	if len(res) != 1 || res[0].Err == "" {
		t.Fatalf("%+v", res)
	}
	if len(st.ListTrash()) != 0 {
		t.Fatal("network must not trash")
	}
	if !strings.Contains(res[0].Err, "connect") && res[0].Err == "" {
		t.Log(res[0].Err)
	}
}
