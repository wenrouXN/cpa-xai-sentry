//go:build !cshared

package main

// Non-plugin binary entry for local smoke (panel demo / tests helpers).
// Real CPA plugin uses main_plugin.go with -tags cshared / c-shared build.

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/panel"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18999", "panel listen address")
	data := flag.String("data", "data", "data dir")
	flag.Parse()
	_ = os.MkdirAll(*data, 0o755)
	cfg := sentrycfg.Default()
	cfg.StatePath = filepath.Join(*data, "cpa-xai-sentry-state.json")
	cfg.TrashDir = filepath.Join(*data, "cpa-xai-sentry-trash")
	st, err := state.Load(cfg.StatePath)
	if err != nil {
		fmt.Println("load state:", err)
		os.Exit(1)
	}
	tr := trash.New(cfg.TrashDir, cfg.TrashRetentionDays, cfg.TrashAutoPurge, st)
	g := guard.New(cfg, st, tr, nil)
	api := &panel.API{Cfg: &cfg, State: st, Trash: tr, Guard: g}
	fmt.Println("XAI Sentry panel (stub) on http://" + *addr + "/")
	if err := http.ListenAndServe(*addr, api.Handler()); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
