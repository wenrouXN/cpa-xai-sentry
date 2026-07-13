package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/guard"
	"github.com/openclaw-local/cpa-xai-sentry/internal/patrol"
	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
	"github.com/openclaw-local/cpa-xai-sentry/internal/trash"
)

type API struct {
	Cfg    *sentrycfg.Config
	State  *state.Store
	Trash  *trash.Store
	Guard  *guard.Guard
	Patrol *patrol.Runner
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/state", a.handleState)
	mux.HandleFunc("/config", a.handleConfig)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc("/candidates", a.handleCandidates)
	mux.HandleFunc("/trash", a.handleTrash)
	mux.HandleFunc("/trash/restore", a.handleTrashRestore)
	mux.HandleFunc("/trash/purge", a.handleTrashPurge)
	mux.HandleFunc("/run-tick", a.handleRunTick)
	mux.HandleFunc("/ui", a.handleUI)
	mux.HandleFunc("/", a.handleUI)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) handleState(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	accs := a.State.AccountsSnapshot()
	type row struct {
		AuthIndex string `json:"auth_index"`
		FileName  string `json:"file_name"`
		Email     string `json:"email"`
		Tier      string `json:"tier"`
		State     string `json:"state"`
		Signal    string `json:"last_signal"`
		RecoverAt any    `json:"recover_at,omitempty"`
	}
	rows := make([]row, 0, len(accs))
	for _, acc := range accs {
		if view == "focus" {
			if acc.State == state.Active && acc.LastSignal == "" {
				continue
			}
		}
		var ra any
		if !acc.RecoverAt.IsZero() {
			ra = acc.RecoverAt.UTC().Format(time.RFC3339)
		}
		rows = append(rows, row{
			AuthIndex: acc.AuthIndex, FileName: acc.FileName, Email: acc.Email,
			Tier: acc.Tier, State: string(acc.State), Signal: acc.LastSignal, RecoverAt: ra,
		})
	}
	writeJSON(w, 200, map[string]any{
		"accounts":    rows,
		"trash_count": len(a.State.ListTrash()),
		"mode":        modeOf(*a.Cfg),
	})
}

func modeOf(c sentrycfg.Config) string {
	if !c.SentryEnabled {
		return "off"
	}
	if !c.AutoCooldown && !c.AutoCandidate && !c.AutoDelete {
		return "observe"
	}
	if c.AutoDelete {
		return "auto_trash"
	}
	if c.AutoCandidate {
		return "safe-guard"
	}
	return "cooldown"
}

func (a *API) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, a.Cfg)
	case http.MethodPost:
		var in sentrycfg.Config
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		in = in.Validate()
		*a.Cfg = in
		if a.Guard != nil {
			a.Guard.Cfg = in
		}
		writeJSON(w, 200, a.Cfg)
	default:
		w.WriteHeader(405)
	}
}

func (a *API) handleLogs(w http.ResponseWriter, r *http.Request) {
	// return last logs without secrets
	type L struct {
		At     string `json:"at"`
		Auth   string `json:"auth"`
		Source string `json:"source"`
		Signal string `json:"signal"`
		Action string `json:"action"`
		Reason string `json:"reason"`
	}
	out := make([]L, 0)
	for _, e := range a.State.SnapshotLogs() {
		out = append(out, L{
			At: e.At.UTC().Format(time.RFC3339), Auth: e.Auth, Source: e.Source,
			Signal: e.Signal, Action: e.Action, Reason: e.Reason,
		})
	}
	writeJSON(w, 200, map[string]any{"logs": out})
}

func (a *API) handleCandidates(w http.ResponseWriter, r *http.Request) {
	var out []any
	for _, acc := range a.State.AccountsSnapshot() {
		if acc.State == state.CandidateDead {
			out = append(out, map[string]string{
				"auth_index": acc.AuthIndex, "file_name": acc.FileName,
				"email": acc.Email, "signal": acc.LastSignal,
			})
		}
	}
	writeJSON(w, 200, map[string]any{"candidates": out})
}

func (a *API) handleTrash(w http.ResponseWriter, r *http.Request) {
	// metadata only
	writeJSON(w, 200, map[string]any{"trash": a.Trash.ListMeta()})
}

func (a *API) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		ID     string `json:"id"`
		Enable bool   `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	err := a.Trash.Restore(in.ID, in.Enable, func(fileName string, raw []byte) error {
		// caller/host wires CPA write; for unit tests inject via Guard.CPA if present
		if a.Guard != nil && a.Guard.CPA != nil {
			return a.Guard.CPA.WriteAuthFileToDir(fileName, raw)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var in struct {
		ID      string `json:"id"`
		Confirm bool   `json:"confirm"`
		Expired bool   `json:"expired"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if !in.Confirm {
		writeJSON(w, 400, map[string]string{"error": "confirm=true required"})
		return
	}
	if in.Expired {
		n, err := a.Trash.PurgeExpired(time.Now())
		if err != nil {
			writeJSON(w, 500, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"purged": n})
		return
	}
	if strings.TrimSpace(in.ID) == "" {
		writeJSON(w, 400, map[string]string{"error": "id required"})
		return
	}
	if err := a.Trash.Purge(in.ID); err != nil {
		writeJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleRunTick(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	if err := a.Guard.Tick(r.Context()); err != nil {
		writeJSON(w, 500, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *API) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}
