package persist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
)

// Overrides are UI/runtime knobs that must survive CPA host reconfigure from YAML.
// Host YAML is treated as base; these fields win when present.
type Overrides struct {
	SentryEnabled *bool  `json:"sentry_enabled,omitempty"`
	AutoCooldown  *bool  `json:"auto_cooldown,omitempty"`
	AutoCandidate *bool  `json:"auto_candidate,omitempty"`
	AutoDelete    *bool  `json:"auto_delete,omitempty"`
	PatrolEnabled *bool  `json:"patrol_enabled,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
	Source        string `json:"source,omitempty"`
}

func PathFor(cfg sentrycfg.Config) string {
	// Prefer next to state file.
	if cfg.StatePath != "" {
		return filepath.Join(filepath.Dir(cfg.StatePath), "runtime-overrides.json")
	}
	if cfg.AuthDir != "" {
		return filepath.Join(cfg.AuthDir, "cpa-xai-sentry", "runtime-overrides.json")
	}
	return "data/cpa-xai-sentry-runtime-overrides.json"
}

func Load(path string) (Overrides, error) {
	var o Overrides
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return o, nil
		}
		return o, err
	}
	if len(b) == 0 {
		return o, nil
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return o, err
	}
	return o, nil
}

func Save(path string, o Overrides) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	o.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if o.Source == "" {
		o.Source = "panel"
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Apply merges overrides onto cfg (bool pointers only when non-nil).
func Apply(cfg sentrycfg.Config, o Overrides) sentrycfg.Config {
	if o.SentryEnabled != nil {
		cfg.SentryEnabled = *o.SentryEnabled
	}
	if o.AutoCooldown != nil {
		cfg.AutoCooldown = *o.AutoCooldown
	}
	if o.AutoCandidate != nil {
		cfg.AutoCandidate = *o.AutoCandidate
	}
	if o.AutoDelete != nil {
		cfg.AutoDelete = *o.AutoDelete
	}
	if o.PatrolEnabled != nil {
		cfg.PatrolEnabled = *o.PatrolEnabled
	}
	return cfg
}

func FromConfig(cfg sentrycfg.Config) Overrides {
	se, ac, cand, del, pat := cfg.SentryEnabled, cfg.AutoCooldown, cfg.AutoCandidate, cfg.AutoDelete, cfg.PatrolEnabled
	return Overrides{
		SentryEnabled: &se,
		AutoCooldown:  &ac,
		AutoCandidate: &cand,
		AutoDelete:    &del,
		PatrolEnabled: &pat,
		Source:        "panel",
	}
}

// PatchBool updates one switch and returns new overrides.
func PatchBool(cur Overrides, field string, val bool) Overrides {
	switch field {
	case "sentry_enabled":
		cur.SentryEnabled = &val
	case "auto_cooldown":
		cur.AutoCooldown = &val
	case "auto_candidate":
		cur.AutoCandidate = &val
	case "auto_delete":
		cur.AutoDelete = &val
	case "patrol_enabled":
		cur.PatrolEnabled = &val
	}
	return cur
}
