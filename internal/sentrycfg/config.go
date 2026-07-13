package sentrycfg

// Config is the plugin YAML/JSON config surface.
type Config struct {
	Enabled               bool           `yaml:"enabled" json:"enabled"`
	SentryEnabled         bool           `yaml:"sentry_enabled" json:"sentry_enabled"`
	AutoCooldown           bool           `yaml:"auto_cooldown" json:"auto_cooldown"`
	AutoCandidate         bool           `yaml:"auto_candidate" json:"auto_candidate"`
	AutoDelete            bool           `yaml:"auto_delete" json:"auto_delete"` // auto-trash
	CooldownSignals       []string       `yaml:"cooldown_signals" json:"cooldown_signals"`
	CandidateSignals      []string       `yaml:"candidate_signals" json:"candidate_signals"`
	DeleteSignals         []string       `yaml:"delete_signals" json:"delete_signals"`
	SignalThresholds      map[string]int `yaml:"signal_thresholds" json:"signal_thresholds"`
	TickSeconds           int            `yaml:"tick_seconds" json:"tick_seconds"`
	MaxResetSeconds       int            `yaml:"max_reset_seconds" json:"max_reset_seconds"`
	MinResetSeconds       int            `yaml:"min_reset_seconds" json:"min_reset_seconds"`
	PermissionCooldownSec int            `yaml:"permission_cooldown_seconds" json:"permission_cooldown_seconds"`
	Auth401CooldownSec      int            `yaml:"auth401_cooldown_seconds" json:"auth401_cooldown_seconds"`
	ManagementURL         string         `yaml:"management_url" json:"management_url"`
	ManagementKey         string         `yaml:"management_key" json:"management_key"`
	StatePath             string         `yaml:"state_path" json:"state_path"`
	AuthDir               string         `yaml:"auth_dir" json:"auth_dir"`
	TrashDir              string         `yaml:"trash_dir" json:"trash_dir"`
	TrashRetentionDays    int            `yaml:"trash_retention_days" json:"trash_retention_days"`
	TrashAutoPurge        bool           `yaml:"trash_auto_purge" json:"trash_auto_purge"`
	RestoreDefaultDis     bool           `yaml:"restore_default_disabled" json:"restore_default_disabled"`
	PatrolEnabled         bool           `yaml:"patrol_enabled" json:"patrol_enabled"`
	PatrolInterval        int            `yaml:"patrol_interval" json:"patrol_interval"`
	PatrolTimeout         int            `yaml:"patrol_timeout" json:"patrol_timeout"`
	PatrolConcurrency     int            `yaml:"patrol_concurrency" json:"patrol_concurrency"`
	PatrolBatchSize       int            `yaml:"patrol_batch_size" json:"patrol_batch_size"`
	PatrolModel           string         `yaml:"patrol_model" json:"patrol_model"`
	PatrolProxyURL        string         `yaml:"patrol_proxy_url" json:"patrol_proxy_url"`
	PatrolAutoModelSwitch bool           `yaml:"patrol_auto_model_switch" json:"patrol_auto_model_switch"`
	CPAMPURL              string         `yaml:"cpamp_url" json:"cpamp_url"`
	CPAMPAdminKey         string         `yaml:"cpamp_admin_key" json:"cpamp_admin_key"`
	CPAMPUsageFloor       bool           `yaml:"cpamp_usage_floor" json:"cpamp_usage_floor"`
	// ReopenForeignDisabled: when true (DEFAULT), tick re-enables CPA auth files
	// whose disable is NOT owned by this sentry (no plugin_auto cool-down / panel
	// permanent disable). Rationale: wait for next real usage error to re-stamp
	// ownership (self-heal). Set false to keep unknown disables closed and mark
	// them as CPA已禁用 instead.
	ReopenForeignDisabled bool `yaml:"reopen_foreign_disabled" json:"reopen_foreign_disabled"`
}

func Default() Config {
	return Config{
		Enabled:       true,
		SentryEnabled: true,
		AutoCooldown:  false,
		AutoCandidate: false,
		AutoDelete:    false,
		CooldownSignals: []string{
			"free_usage_429", "spending_limit_402", "auth_401", "permission_403",
		},
		CandidateSignals: []string{"auth_401"},
		DeleteSignals:    []string{},
		SignalThresholds: map[string]int{
			"free_usage_429": 1, "spending_limit_402": 1, "auth_401": 2, "permission_403": 3,
		},
		TickSeconds:           30,
		MaxResetSeconds:       86400,
		PermissionCooldownSec: 1800,
		Auth401CooldownSec:      3600,
		ManagementURL:         "http://127.0.0.1:8317",
		// Persist under auth_dir (host-mounted) so panel toggles & state survive container restarts.
		StatePath:             "/root/.cli-proxy-api/cpa-xai-sentry/state.json",
		AuthDir:               "/root/.cli-proxy-api",
		TrashDir:              "/root/.cli-proxy-api/cpa-xai-sentry/trash",
		TrashRetentionDays:    7,
		TrashAutoPurge:        true,
		RestoreDefaultDis:     true,
		PatrolInterval:        3600,
		PatrolTimeout:         15,
		PatrolConcurrency:     8,
		PatrolBatchSize:       50,
		PatrolModel:           "grok-4.5",
		CPAMPUsageFloor:       true,
		ReopenForeignDisabled: true, // self-heal: open unowned disables, wait next error
	}
}

// Validate applies hard bans and defaults.
func (c Config) Validate() Config {
	out := c
	filtered := make([]string, 0, len(out.DeleteSignals))
	for _, s := range out.DeleteSignals {
		if s == "spending_limit_402" {
			continue
		}
		filtered = append(filtered, s)
	}
	out.DeleteSignals = filtered
	if out.TrashRetentionDays <= 0 {
		out.TrashRetentionDays = 7
	}
	if out.TickSeconds <= 0 {
		out.TickSeconds = 30
	}
	if out.SignalThresholds == nil {
		out.SignalThresholds = Default().SignalThresholds
	}
	return out
}

// Redact returns a public view without secrets.
func (c Config) Redact() map[string]any {
	return map[string]any{
		"enabled":                     c.Enabled,
		"sentry_enabled":              c.SentryEnabled,
		"auto_cooldown":               c.AutoCooldown,
		"auto_candidate":              c.AutoCandidate,
		"auto_delete":                 c.AutoDelete,
		"cooldown_signals":            c.CooldownSignals,
		"candidate_signals":           c.CandidateSignals,
		"delete_signals":              c.DeleteSignals,
		"signal_thresholds":           c.SignalThresholds,
		"tick_seconds":                c.TickSeconds,
		"max_reset_seconds":           c.MaxResetSeconds,
		"min_reset_seconds":           c.MinResetSeconds,
		"permission_cooldown_seconds": c.PermissionCooldownSec,
		"auth401_cooldown_seconds":    c.Auth401CooldownSec,
		"trash_retention_days":        c.TrashRetentionDays,
		"trash_auto_purge":            c.TrashAutoPurge,
		"restore_default_disabled":    c.RestoreDefaultDis,
		"management_url":              c.ManagementURL,
		"management_key_set":          c.ManagementKey != "",
		"state_path":                  c.StatePath,
		"auth_dir":                    c.AuthDir,
		"trash_dir":                   c.TrashDir,
		"patrol_enabled":              c.PatrolEnabled,
		"patrol_interval":             c.PatrolInterval,
		"patrol_timeout":              c.PatrolTimeout,
		"patrol_concurrency":          c.PatrolConcurrency,
		"patrol_batch_size":           c.PatrolBatchSize,
		"patrol_model":                c.PatrolModel,
		"patrol_proxy_url":            c.PatrolProxyURL,
		"patrol_auto_model_switch":    c.PatrolAutoModelSwitch,
		"cpamp_url":                   c.CPAMPURL,
		"cpamp_admin_key_set":         c.CPAMPAdminKey != "",
		"cpamp_usage_floor":           c.CPAMPUsageFloor,
		"reopen_foreign_disabled":     c.ReopenForeignDisabled,
	}
}
