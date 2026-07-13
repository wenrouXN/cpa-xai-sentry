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

func TestTickDoesNotReopenOperatorDisabled(t *testing.T) {
	dir := t.TempDir()
	setCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v0/management/auth-files" && r.Method == http.MethodGet:
			// match cpaapi.ListAuthFiles unmarshalling
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{{
					"name": "xai-op@lovc.eu.cc.json", "email": "op@lovc.eu.cc",
					"provider": "xai", "type": "xai", "disabled": true,
				}},
			})
		case r.URL.Path == "/v0/management/auth-files/status":
			setCalls++
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			// ignore other probes
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
	cfg.ReopenForeignDisabled = false
	st := state.New(filepath.Join(dir, "s.json"))
	st.Touch("auth1")
	st.UpdateMeta("auth1", "xai-op@lovc.eu.cc.json", "op@lovc.eu.cc", "")
	st.SetAccountState("auth1", state.Active, "")
	_ = st.Save()
	ts := trash.New(filepath.Join(dir, "trash"), 7, true, st)
	cpa := cpaapi.New(cfg.ManagementURL, cfg.ManagementKey, cfg.AuthDir)
	g := guard.New(cfg, st, ts, cpa)
	if err := g.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	acc := st.Get("auth1")
	if acc == nil {
		t.Fatal("missing account")
	}
	if acc.State != state.UserManual || acc.DisableSource != "cpa_file_disabled" {
		t.Fatalf("want cpa_file_disabled lock, got state=%s src=%s", acc.State, acc.DisableSource)
	}
	if setCalls != 0 {
		t.Fatalf("must not call SetDisabled when reopen_foreign_disabled=false, calls=%d", setCalls)
	}
}
