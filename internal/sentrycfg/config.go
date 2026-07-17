package sentrycfg

import "strings"

// Config is the plugin YAML/JSON config surface.
type Config struct {
	Enabled               bool           `yaml:"enabled" json:"enabled"`
	SentryEnabled         bool           `yaml:"sentry_enabled" json:"sentry_enabled"`
	AutoCooldown          bool           `yaml:"auto_cooldown" json:"auto_cooldown"`
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
	Auth401CooldownSec    int            `yaml:"auth401_cooldown_seconds" json:"auth401_cooldown_seconds"`
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
	// PatrolMode for scheduled auto patrol: all | enabled | cooldown | permanent
	// (legacy "full" accepted as enabled). Manual start can override per request.
	PatrolMode string `yaml:"patrol_mode" json:"patrol_mode"`
	CPAMPURL              string         `yaml:"cpamp_url" json:"cpamp_url"`
	CPAMPAdminKey         string         `yaml:"cpamp_admin_key" json:"cpamp_admin_key"`
	CPAMPUsageFloor       bool           `yaml:"cpamp_usage_floor" json:"cpamp_usage_floor"`
	// ReopenForeignDisabled: when true (DEFAULT per ops preference), tick re-enables
	// CPA auth files that are disabled and NOT owned by this sentry. Next real
	// usage/patrol error re-stamps cool-down ownership. Owned cool-downs / panel
	// permanent disables are NEVER opened. Set false to keep unknown disables closed.
	ReopenForeignDisabled bool `yaml:"reopen_foreign_disabled" json:"reopen_foreign_disabled"`

	// --- Register tab (grok-register-lite / :8788) ---
	RegisterEnabled              bool    `yaml:"register_enabled" json:"register_enabled"`
	RegisterBaseURL              string  `yaml:"register_base_url" json:"register_base_url"`
	RegisterAdminBase            string  `yaml:"register_admin_base" json:"register_admin_base"`
	RegisterPassword             string  `yaml:"register_password" json:"register_password"`
	RegisterTimeoutSec           int     `yaml:"register_timeout_sec" json:"register_timeout_sec"`
	RegisterDryRun               bool    `yaml:"register_dry_run" json:"register_dry_run"`
	RegisterManualDefaultCount   int     `yaml:"register_manual_default_count" json:"register_manual_default_count"`
	RegisterManualMaxCount       int     `yaml:"register_manual_max_count" json:"register_manual_max_count"`
	RegisterAutoEnabled          bool    `yaml:"register_auto_enabled" json:"register_auto_enabled"`
	RegisterAutoIntervalSec      int     `yaml:"register_auto_interval_sec" json:"register_auto_interval_sec"`
	RegisterAutoCount            int     `yaml:"register_auto_count" json:"register_auto_count"`
	// Floor (保底): when (CPA enabled + sentry cooldown) < MinPool, register Count each IntervalSec until above.
	RegisterFloorEnabled     bool `yaml:"register_floor_enabled" json:"register_floor_enabled"`
	RegisterFloorMinPool     int  `yaml:"register_floor_min_pool" json:"register_floor_min_pool"`
	RegisterFloorCount       int  `yaml:"register_floor_count" json:"register_floor_count"`
	RegisterFloorIntervalSec int  `yaml:"register_floor_interval_sec" json:"register_floor_interval_sec"`
	RegisterAutoOnlyWhenIdle     bool    `yaml:"register_auto_only_when_idle" json:"register_auto_only_when_idle"`
	RegisterAutoRequireHealth    bool    `yaml:"register_auto_require_health_ok" json:"register_auto_require_health_ok"`
	RegisterAutoPauseOnLow       bool    `yaml:"register_auto_pause_on_low_success" json:"register_auto_pause_on_low_success"`
	RegisterHealthIntervalSec    int     `yaml:"register_health_interval_sec" json:"register_health_interval_sec"`
	RegisterHealthWindowJobs     int     `yaml:"register_health_window_jobs" json:"register_health_window_jobs"`
	RegisterHealthMinSamples     int     `yaml:"register_health_min_samples" json:"register_health_min_samples"`
	RegisterHealthOKRate         float64 `yaml:"register_health_ok_rate" json:"register_health_ok_rate"`
	RegisterHealthWarnRate       float64 `yaml:"register_health_warn_rate" json:"register_health_warn_rate"`
	RegisterRequireCPAok         bool    `yaml:"register_require_cpa_ok" json:"register_require_cpa_ok"`
	// Relogin (only 8788-local accounts with password)
	RegisterReloginOnAuth401    bool `yaml:"register_relogin_on_auth401" json:"register_relogin_on_auth401"`
	RegisterReloginMaxStreak    int  `yaml:"register_relogin_max_streak" json:"register_relogin_max_streak"`
	RegisterReloginConcurrency  int  `yaml:"register_relogin_concurrency" json:"register_relogin_concurrency"`
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
		Auth401CooldownSec:    3600,
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
		PatrolMode:            "enabled",
		CPAMPUsageFloor:       true,
		ReopenForeignDisabled: true, // ops: open unowned disables; wait next error to re-stamp
		// Register tab defaults (off until configured)
		RegisterEnabled:            false,
		RegisterBaseURL:            "http://192.168.1.68:8788",
		RegisterAdminBase:          "/admin",
		RegisterTimeoutSec:         30,
		RegisterManualDefaultCount: 10,
		RegisterManualMaxCount:     50,
		RegisterAutoEnabled:        false,
		RegisterAutoIntervalSec:    3600,
		RegisterAutoCount:          10,
		RegisterFloorEnabled:       false,
		RegisterFloorMinPool:       100,
		RegisterFloorCount:         10,
		RegisterFloorIntervalSec:   600,
		RegisterAutoOnlyWhenIdle:   true,
		RegisterAutoRequireHealth:  true,
		RegisterAutoPauseOnLow:     true,
		RegisterHealthIntervalSec:  300,
		RegisterHealthWindowJobs:   10,
		RegisterHealthMinSamples:   10,
		RegisterHealthOKRate:       0.85,
		RegisterHealthWarnRate:     0.60,
		RegisterRequireCPAok:       false,
		RegisterReloginOnAuth401:   true,
		RegisterReloginMaxStreak:   2,
		RegisterReloginConcurrency: 2,
	}
}

// Validate applies defaults (no hard bans on delete_signals — panel/policy decides trash).
func (c Config) Validate() Config {
	out := c
	if out.TrashRetentionDays <= 0 {
		out.TrashRetentionDays = 7
	}
	if out.TickSeconds <= 0 {
		out.TickSeconds = 30
	}
	if out.SignalThresholds == nil {
		out.SignalThresholds = Default().SignalThresholds
	}
	// normalize patrol auto range (timer uses this; panel select values)
	switch strings.ToLower(strings.TrimSpace(out.PatrolMode)) {
	case "all", "全部", "any":
		out.PatrolMode = "all"
	case "cooldown", "cool", "spending", "冷却":
		out.PatrolMode = "cooldown"
	case "permanent", "manual", "disabled", "user_manual", "永久禁用", "永禁":
		out.PatrolMode = "permanent"
	case "candidate", "候删", "候选", "candidate_dead":
		out.PatrolMode = "candidate"
	case "trash", "trashed", "垃圾箱", "箱":
		out.PatrolMode = "trash"
	case "enabled", "full", "active", "启用", "可接流":
		out.PatrolMode = "enabled"
	default:
		out.PatrolMode = "enabled"
	}
	if out.RegisterAdminBase == "" {
		out.RegisterAdminBase = "/admin"
	}
	if out.RegisterTimeoutSec <= 0 {
		out.RegisterTimeoutSec = 30
	}
	if out.RegisterManualDefaultCount <= 0 {
		out.RegisterManualDefaultCount = 10
	}
	if out.RegisterManualMaxCount <= 0 {
		out.RegisterManualMaxCount = 50
	}
	if out.RegisterAutoIntervalSec <= 0 {
		out.RegisterAutoIntervalSec = 3600
	}
	if out.RegisterAutoCount <= 0 {
		out.RegisterAutoCount = 10
	}
	if out.RegisterFloorMinPool <= 0 {
		out.RegisterFloorMinPool = 100
	}
	if out.RegisterFloorCount <= 0 {
		out.RegisterFloorCount = 10
	}
	if out.RegisterFloorIntervalSec <= 0 {
		out.RegisterFloorIntervalSec = 600
	}
	// product defaults: always on for auto/floor safety (not exposed in panel)
	out.RegisterAutoOnlyWhenIdle = true
	out.RegisterAutoRequireHealth = true
	out.RegisterAutoPauseOnLow = true
	out.RegisterRequireCPAok = true
	if out.RegisterHealthIntervalSec <= 0 {
		out.RegisterHealthIntervalSec = 300
	}
	if out.RegisterHealthWindowJobs <= 0 {
		out.RegisterHealthWindowJobs = 10
	}
	if out.RegisterHealthMinSamples <= 0 {
		out.RegisterHealthMinSamples = 10
	}
	if out.RegisterHealthOKRate <= 0 {
		out.RegisterHealthOKRate = 0.85
	}
	if out.RegisterHealthWarnRate <= 0 {
		out.RegisterHealthWarnRate = 0.60
	}
	if out.RegisterReloginMaxStreak <= 0 {
		out.RegisterReloginMaxStreak = 2
	}
	if out.RegisterReloginConcurrency <= 0 {
		out.RegisterReloginConcurrency = 2
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
		"patrol_mode":                 c.PatrolMode,
		"cpamp_url":                   c.CPAMPURL,
		"cpamp_admin_key_set":         c.CPAMPAdminKey != "",
		"cpamp_usage_floor":           c.CPAMPUsageFloor,
		"reopen_foreign_disabled":     c.ReopenForeignDisabled,
		"register_enabled":                   c.RegisterEnabled,
		"register_base_url":                  c.RegisterBaseURL,
		"register_admin_base":                c.RegisterAdminBase,
		"register_password_set":              c.RegisterPassword != "",
		"register_timeout_sec":               c.RegisterTimeoutSec,
		"register_dry_run":                   c.RegisterDryRun,
		"register_manual_default_count":      c.RegisterManualDefaultCount,
		"register_manual_max_count":          c.RegisterManualMaxCount,
		"register_auto_enabled":              c.RegisterAutoEnabled,
		"register_auto_interval_sec":         c.RegisterAutoIntervalSec,
		"register_auto_count":                c.RegisterAutoCount,
		"register_floor_enabled":             c.RegisterFloorEnabled,
		"register_floor_min_pool":            c.RegisterFloorMinPool,
		"register_floor_count":               c.RegisterFloorCount,
		"register_floor_interval_sec":        c.RegisterFloorIntervalSec,
		"register_auto_only_when_idle":       c.RegisterAutoOnlyWhenIdle,
		"register_auto_require_health_ok":    c.RegisterAutoRequireHealth,
		"register_auto_pause_on_low_success": c.RegisterAutoPauseOnLow,
		"register_health_interval_sec":       c.RegisterHealthIntervalSec,
		"register_health_window_jobs":        c.RegisterHealthWindowJobs,
		"register_health_min_samples":        c.RegisterHealthMinSamples,
		"register_health_ok_rate":            c.RegisterHealthOKRate,
		"register_health_warn_rate":          c.RegisterHealthWarnRate,
		"register_require_cpa_ok":            c.RegisterRequireCPAok,
		"register_relogin_on_auth401":         c.RegisterReloginOnAuth401,
		"register_relogin_max_streak":         c.RegisterReloginMaxStreak,
		"register_relogin_concurrency":        c.RegisterReloginConcurrency,
	}
}

// HostPluginPatch builds the map written to CPA plugins.configs.cpa-xai-sentry.
// Includes operational fields; host keeps enabled/priority. Secrets included so
// GET+merge+PUT does not wipe management_key already present in host YAML.
func (c Config) HostPluginPatch() map[string]any {
	return map[string]any{
		"sentry_enabled":              c.SentryEnabled,
		"auto_cooldown":               c.AutoCooldown,
		"auto_candidate":              c.AutoCandidate,
		"auto_delete":                 c.AutoDelete,
		"tick_seconds":                c.TickSeconds,
		"max_reset_seconds":           c.MaxResetSeconds,
		"min_reset_seconds":           c.MinResetSeconds,
		"permission_cooldown_seconds": c.PermissionCooldownSec,
		"auth401_cooldown_seconds":    c.Auth401CooldownSec,
		"management_url":              c.ManagementURL,
		"management_key":              c.ManagementKey,
		"state_path":                  c.StatePath,
		"auth_dir":                    c.AuthDir,
		"trash_dir":                   c.TrashDir,
		"trash_retention_days":        c.TrashRetentionDays,
		"trash_auto_purge":            c.TrashAutoPurge,
		"restore_default_disabled":    c.RestoreDefaultDis,
		"patrol_enabled":              c.PatrolEnabled,
		"patrol_interval":             c.PatrolInterval,
		"patrol_timeout":              c.PatrolTimeout,
		"patrol_concurrency":          c.PatrolConcurrency,
		"patrol_batch_size":           c.PatrolBatchSize,
		"patrol_model":                c.PatrolModel,
		"patrol_proxy_url":            c.PatrolProxyURL,
		"patrol_auto_model_switch":    c.PatrolAutoModelSwitch,
		"patrol_mode":                 c.PatrolMode,
		"cpamp_url":                   c.CPAMPURL,
		"cpamp_admin_key":             c.CPAMPAdminKey,
		"cpamp_usage_floor":           c.CPAMPUsageFloor,
		"reopen_foreign_disabled":     c.ReopenForeignDisabled,
		"register_enabled":                   c.RegisterEnabled,
		"register_base_url":                  c.RegisterBaseURL,
		"register_admin_base":                c.RegisterAdminBase,
		"register_password":                  c.RegisterPassword,
		"register_timeout_sec":               c.RegisterTimeoutSec,
		"register_dry_run":                   c.RegisterDryRun,
		"register_manual_default_count":      c.RegisterManualDefaultCount,
		"register_manual_max_count":          c.RegisterManualMaxCount,
		"register_auto_enabled":              c.RegisterAutoEnabled,
		"register_auto_interval_sec":         c.RegisterAutoIntervalSec,
		"register_auto_count":                c.RegisterAutoCount,
		"register_floor_enabled":             c.RegisterFloorEnabled,
		"register_floor_min_pool":            c.RegisterFloorMinPool,
		"register_floor_count":               c.RegisterFloorCount,
		"register_floor_interval_sec":        c.RegisterFloorIntervalSec,
		"register_auto_only_when_idle":       c.RegisterAutoOnlyWhenIdle,
		"register_auto_require_health_ok":    c.RegisterAutoRequireHealth,
		"register_auto_pause_on_low_success": c.RegisterAutoPauseOnLow,
		"register_health_interval_sec":       c.RegisterHealthIntervalSec,
		"register_health_window_jobs":        c.RegisterHealthWindowJobs,
		"register_health_min_samples":        c.RegisterHealthMinSamples,
		"register_health_ok_rate":            c.RegisterHealthOKRate,
		"register_health_warn_rate":          c.RegisterHealthWarnRate,
		"register_require_cpa_ok":            c.RegisterRequireCPAok,
		"register_relogin_on_auth401":         c.RegisterReloginOnAuth401,
		"register_relogin_max_streak":         c.RegisterReloginMaxStreak,
		"register_relogin_concurrency":        c.RegisterReloginConcurrency,
	}
}
