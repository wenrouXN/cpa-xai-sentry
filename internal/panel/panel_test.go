package panel_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	raw := []byte("{\"email\":\"a@b.com\",\"access_token\":\""+tok+"\",\"disabled\":false}")
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
