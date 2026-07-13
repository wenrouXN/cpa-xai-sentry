package persist

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
)

// Overrides are UI/runtime knobs that must survive CPA host reconfigure from YAML
// and plugin process restarts (docker restart / plugin upgrade).
// Host YAML is base; these fields win when present.
// Secrets (management_key, cpamp_admin_key) stay in YAML only.
type Overrides struct {
	SentryEnabled *bool `json:"sentry_enabled,omitempty"`
	AutoCooldown  *bool `json:"auto_cooldown,omitempty"`
	AutoCandidate *bool `json:"auto_candidate,omitempty"`
	AutoDelete    *bool `json:"auto_delete,omitempty"`

	// Ops timing / self-heal
	TickSeconds           *int  `json:"tick_seconds,omitempty"`
	MaxResetSeconds       *int  `json:"max_reset_seconds,omitempty"`
	MinResetSeconds       *int  `json:"min_reset_seconds,omitempty"`
	PermissionCooldownSec *int  `json:"permission_cooldown_seconds,omitempty"`
	Auth401CooldownSec    *int  `json:"auth401_cooldown_seconds,omitempty"`
	ReopenForeignDisabled *bool `json:"reopen_foreign_disabled,omitempty"`
	CPAMPUsageFloor       *bool `json:"cpamp_usage_floor,omitempty"`
	TrashRetentionDays    *int  `json:"trash_retention_days,omitempty"`
	TrashAutoPurge        *bool `json:"trash_auto_purge,omitempty"`
	RestoreDefaultDis     *bool `json:"restore_default_disabled,omitempty"`

	// Patrol
	PatrolEnabled         *bool   `json:"patrol_enabled,omitempty"`
	PatrolInterval        *int    `json:"patrol_interval,omitempty"`
	PatrolTimeout         *int    `json:"patrol_timeout,omitempty"`
	PatrolConcurrency     *int    `json:"patrol_concurrency,omitempty"`
	PatrolBatchSize       *int    `json:"patrol_batch_size,omitempty"`
	PatrolModel           *string `json:"patrol_model,omitempty"`
	PatrolProxyURL        *string `json:"patrol_proxy_url,omitempty"`
	PatrolAutoModelSwitch *bool   `json:"patrol_auto_model_switch,omitempty"`

	UpdatedAt string `json:"updated_at,omitempty"`
	Source    string `json:"source,omitempty"`
}

func PathFor(cfg sentrycfg.Config) string {
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
	if o.TickSeconds != nil && *o.TickSeconds > 0 {
		cfg.TickSeconds = *o.TickSeconds
	}
	if o.MaxResetSeconds != nil && *o.MaxResetSeconds >= 0 {
		cfg.MaxResetSeconds = *o.MaxResetSeconds
	}
	if o.MinResetSeconds != nil && *o.MinResetSeconds >= 0 {
		cfg.MinResetSeconds = *o.MinResetSeconds
	}
	if o.PermissionCooldownSec != nil && *o.PermissionCooldownSec > 0 {
		cfg.PermissionCooldownSec = *o.PermissionCooldownSec
	}
	if o.Auth401CooldownSec != nil && *o.Auth401CooldownSec > 0 {
		cfg.Auth401CooldownSec = *o.Auth401CooldownSec
	}
	if o.ReopenForeignDisabled != nil {
		cfg.ReopenForeignDisabled = *o.ReopenForeignDisabled
	}
	if o.CPAMPUsageFloor != nil {
		cfg.CPAMPUsageFloor = *o.CPAMPUsageFloor
	}
	if o.TrashRetentionDays != nil && *o.TrashRetentionDays > 0 {
		cfg.TrashRetentionDays = *o.TrashRetentionDays
	}
	if o.TrashAutoPurge != nil {
		cfg.TrashAutoPurge = *o.TrashAutoPurge
	}
	if o.RestoreDefaultDis != nil {
		cfg.RestoreDefaultDis = *o.RestoreDefaultDis
	}
	if o.PatrolEnabled != nil {
		cfg.PatrolEnabled = *o.PatrolEnabled
	}
	if o.PatrolInterval != nil && *o.PatrolInterval > 0 {
		cfg.PatrolInterval = *o.PatrolInterval
	}
	if o.PatrolTimeout != nil && *o.PatrolTimeout > 0 {
		cfg.PatrolTimeout = *o.PatrolTimeout
	}
	if o.PatrolConcurrency != nil && *o.PatrolConcurrency > 0 {
		cfg.PatrolConcurrency = *o.PatrolConcurrency
	}
	if o.PatrolBatchSize != nil && *o.PatrolBatchSize >= 0 {
		cfg.PatrolBatchSize = *o.PatrolBatchSize
	}
	if o.PatrolModel != nil && *o.PatrolModel != "" {
		cfg.PatrolModel = *o.PatrolModel
	}
	if o.PatrolProxyURL != nil {
		cfg.PatrolProxyURL = *o.PatrolProxyURL
	}
	if o.PatrolAutoModelSwitch != nil {
		cfg.PatrolAutoModelSwitch = *o.PatrolAutoModelSwitch
	}
	return cfg
}

func FromConfig(cfg sentrycfg.Config) Overrides {
	se, ac, cand, del := cfg.SentryEnabled, cfg.AutoCooldown, cfg.AutoCandidate, cfg.AutoDelete
	tick, maxR, minR := cfg.TickSeconds, cfg.MaxResetSeconds, cfg.MinResetSeconds
	pcool, a401 := cfg.PermissionCooldownSec, cfg.Auth401CooldownSec
	reopen, floor := cfg.ReopenForeignDisabled, cfg.CPAMPUsageFloor
	trd, tap, rdd := cfg.TrashRetentionDays, cfg.TrashAutoPurge, cfg.RestoreDefaultDis
	pe := cfg.PatrolEnabled
	pi, pt, pc, pb := cfg.PatrolInterval, cfg.PatrolTimeout, cfg.PatrolConcurrency, cfg.PatrolBatchSize
	pm, pp := cfg.PatrolModel, cfg.PatrolProxyURL
	pams := cfg.PatrolAutoModelSwitch
	return Overrides{
		SentryEnabled: &se, AutoCooldown: &ac, AutoCandidate: &cand, AutoDelete: &del,
		TickSeconds: &tick, MaxResetSeconds: &maxR, MinResetSeconds: &minR,
		PermissionCooldownSec: &pcool, Auth401CooldownSec: &a401,
		ReopenForeignDisabled: &reopen, CPAMPUsageFloor: &floor,
		TrashRetentionDays: &trd, TrashAutoPurge: &tap, RestoreDefaultDis: &rdd,
		PatrolEnabled: &pe, PatrolInterval: &pi, PatrolTimeout: &pt,
		PatrolConcurrency: &pc, PatrolBatchSize: &pb,
		PatrolModel: &pm, PatrolProxyURL: &pp, PatrolAutoModelSwitch: &pams,
		Source: "panel",
	}
}

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
	case "reopen_foreign_disabled":
		cur.ReopenForeignDisabled = &val
	case "cpamp_usage_floor":
		cur.CPAMPUsageFloor = &val
	case "trash_auto_purge":
		cur.TrashAutoPurge = &val
	case "restore_default_disabled":
		cur.RestoreDefaultDis = &val
	case "patrol_auto_model_switch":
		cur.PatrolAutoModelSwitch = &val
	}
	return cur
}
