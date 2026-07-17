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
// Secrets (management_key, cpamp_admin_key, register_password) stay in YAML/host when set;
// register_password is also mirrored here so panel save survives without host write.
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
	PatrolMode            *string `json:"patrol_mode,omitempty"`

	// Register tab (8788)
	RegisterEnabled             *bool    `json:"register_enabled,omitempty"`
	RegisterBaseURL             *string  `json:"register_base_url,omitempty"`
	RegisterAdminBase           *string  `json:"register_admin_base,omitempty"`
	RegisterPassword            *string  `json:"register_password,omitempty"`
	RegisterTimeoutSec          *int     `json:"register_timeout_sec,omitempty"`
	RegisterDryRun              *bool    `json:"register_dry_run,omitempty"`
	RegisterManualDefaultCount  *int     `json:"register_manual_default_count,omitempty"`
	RegisterManualMaxCount      *int     `json:"register_manual_max_count,omitempty"`
	RegisterAutoEnabled         *bool    `json:"register_auto_enabled,omitempty"`
	RegisterAutoIntervalSec     *int     `json:"register_auto_interval_sec,omitempty"`
	RegisterAutoCount           *int     `json:"register_auto_count,omitempty"`
	RegisterFloorEnabled        *bool    `json:"register_floor_enabled,omitempty"`
	RegisterFloorMinPool        *int     `json:"register_floor_min_pool,omitempty"`
	RegisterFloorCount          *int     `json:"register_floor_count,omitempty"`
	RegisterFloorIntervalSec    *int     `json:"register_floor_interval_sec,omitempty"`
	RegisterAutoOnlyWhenIdle    *bool    `json:"register_auto_only_when_idle,omitempty"`
	RegisterAutoRequireHealth   *bool    `json:"register_auto_require_health_ok,omitempty"`
	RegisterAutoPauseOnLow      *bool    `json:"register_auto_pause_on_low_success,omitempty"`
	RegisterHealthIntervalSec   *int     `json:"register_health_interval_sec,omitempty"`
	RegisterHealthWindowJobs    *int     `json:"register_health_window_jobs,omitempty"`
	RegisterHealthMinSamples    *int     `json:"register_health_min_samples,omitempty"`
	RegisterHealthOKRate        *float64 `json:"register_health_ok_rate,omitempty"`
	RegisterHealthWarnRate      *float64 `json:"register_health_warn_rate,omitempty"`
	RegisterRequireCPAok        *bool    `json:"register_require_cpa_ok,omitempty"`
	RegisterReloginOnAuth401   *bool `json:"register_relogin_on_auth401,omitempty"`
	RegisterReloginMaxStreak   *int  `json:"register_relogin_max_streak,omitempty"`
	RegisterReloginConcurrency *int  `json:"register_relogin_concurrency,omitempty"`

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
	if o.PatrolMode != nil && *o.PatrolMode != "" {
		cfg.PatrolMode = *o.PatrolMode
	}
	// register
	if o.RegisterEnabled != nil {
		cfg.RegisterEnabled = *o.RegisterEnabled
	}
	if o.RegisterBaseURL != nil {
		cfg.RegisterBaseURL = *o.RegisterBaseURL
	}
	if o.RegisterAdminBase != nil && *o.RegisterAdminBase != "" {
		cfg.RegisterAdminBase = *o.RegisterAdminBase
	}
	if o.RegisterPassword != nil && *o.RegisterPassword != "" {
		cfg.RegisterPassword = *o.RegisterPassword
	}
	if o.RegisterTimeoutSec != nil && *o.RegisterTimeoutSec > 0 {
		cfg.RegisterTimeoutSec = *o.RegisterTimeoutSec
	}
	if o.RegisterDryRun != nil {
		cfg.RegisterDryRun = *o.RegisterDryRun
	}
	if o.RegisterManualDefaultCount != nil && *o.RegisterManualDefaultCount > 0 {
		cfg.RegisterManualDefaultCount = *o.RegisterManualDefaultCount
	}
	if o.RegisterManualMaxCount != nil && *o.RegisterManualMaxCount > 0 {
		cfg.RegisterManualMaxCount = *o.RegisterManualMaxCount
	}
	if o.RegisterAutoEnabled != nil {
		cfg.RegisterAutoEnabled = *o.RegisterAutoEnabled
	}
	if o.RegisterAutoIntervalSec != nil && *o.RegisterAutoIntervalSec > 0 {
		cfg.RegisterAutoIntervalSec = *o.RegisterAutoIntervalSec
	}
	if o.RegisterAutoCount != nil && *o.RegisterAutoCount > 0 {
		cfg.RegisterAutoCount = *o.RegisterAutoCount
	}
	if o.RegisterFloorEnabled != nil {
		cfg.RegisterFloorEnabled = *o.RegisterFloorEnabled
	}
	if o.RegisterFloorMinPool != nil && *o.RegisterFloorMinPool > 0 {
		cfg.RegisterFloorMinPool = *o.RegisterFloorMinPool
	}
	if o.RegisterFloorCount != nil && *o.RegisterFloorCount > 0 {
		cfg.RegisterFloorCount = *o.RegisterFloorCount
	}
	if o.RegisterFloorIntervalSec != nil && *o.RegisterFloorIntervalSec > 0 {
		cfg.RegisterFloorIntervalSec = *o.RegisterFloorIntervalSec
	}
	if o.RegisterAutoOnlyWhenIdle != nil {
		cfg.RegisterAutoOnlyWhenIdle = *o.RegisterAutoOnlyWhenIdle
	}
	if o.RegisterAutoRequireHealth != nil {
		cfg.RegisterAutoRequireHealth = *o.RegisterAutoRequireHealth
	}
	if o.RegisterAutoPauseOnLow != nil {
		cfg.RegisterAutoPauseOnLow = *o.RegisterAutoPauseOnLow
	}
	if o.RegisterHealthIntervalSec != nil && *o.RegisterHealthIntervalSec > 0 {
		cfg.RegisterHealthIntervalSec = *o.RegisterHealthIntervalSec
	}
	if o.RegisterHealthWindowJobs != nil && *o.RegisterHealthWindowJobs > 0 {
		cfg.RegisterHealthWindowJobs = *o.RegisterHealthWindowJobs
	}
	if o.RegisterHealthMinSamples != nil && *o.RegisterHealthMinSamples > 0 {
		cfg.RegisterHealthMinSamples = *o.RegisterHealthMinSamples
	}
	if o.RegisterHealthOKRate != nil && *o.RegisterHealthOKRate > 0 {
		cfg.RegisterHealthOKRate = *o.RegisterHealthOKRate
	}
	if o.RegisterHealthWarnRate != nil && *o.RegisterHealthWarnRate > 0 {
		cfg.RegisterHealthWarnRate = *o.RegisterHealthWarnRate
	}
	if o.RegisterRequireCPAok != nil {
		cfg.RegisterRequireCPAok = *o.RegisterRequireCPAok
	}
	if o.RegisterReloginOnAuth401 != nil {
		cfg.RegisterReloginOnAuth401 = *o.RegisterReloginOnAuth401
	}
	if o.RegisterReloginMaxStreak != nil && *o.RegisterReloginMaxStreak > 0 {
		cfg.RegisterReloginMaxStreak = *o.RegisterReloginMaxStreak
	}
	if o.RegisterReloginConcurrency != nil && *o.RegisterReloginConcurrency > 0 {
		cfg.RegisterReloginConcurrency = *o.RegisterReloginConcurrency
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
	pmode := cfg.PatrolMode
	re, rurl, radmin := cfg.RegisterEnabled, cfg.RegisterBaseURL, cfg.RegisterAdminBase
	rpw := cfg.RegisterPassword
	rto, rdry := cfg.RegisterTimeoutSec, cfg.RegisterDryRun
	rmd, rmm := cfg.RegisterManualDefaultCount, cfg.RegisterManualMaxCount
	rae, rais, rac := cfg.RegisterAutoEnabled, cfg.RegisterAutoIntervalSec, cfg.RegisterAutoCount
	rfe, rfmin, rfc, rfi := cfg.RegisterFloorEnabled, cfg.RegisterFloorMinPool, cfg.RegisterFloorCount, cfg.RegisterFloorIntervalSec
	raoi, rarh, rapl := cfg.RegisterAutoOnlyWhenIdle, cfg.RegisterAutoRequireHealth, cfg.RegisterAutoPauseOnLow
	rhis, rhwj, rhms := cfg.RegisterHealthIntervalSec, cfg.RegisterHealthWindowJobs, cfg.RegisterHealthMinSamples
	rhor, rhwr := cfg.RegisterHealthOKRate, cfg.RegisterHealthWarnRate
	rrc := cfg.RegisterRequireCPAok
	roa, rms, rrcy := cfg.RegisterReloginOnAuth401, cfg.RegisterReloginMaxStreak, cfg.RegisterReloginConcurrency
	return Overrides{
		SentryEnabled: &se, AutoCooldown: &ac, AutoCandidate: &cand, AutoDelete: &del,
		TickSeconds: &tick, MaxResetSeconds: &maxR, MinResetSeconds: &minR,
		PermissionCooldownSec: &pcool, Auth401CooldownSec: &a401,
		ReopenForeignDisabled: &reopen, CPAMPUsageFloor: &floor,
		TrashRetentionDays: &trd, TrashAutoPurge: &tap, RestoreDefaultDis: &rdd,
		PatrolEnabled: &pe, PatrolInterval: &pi, PatrolTimeout: &pt,
		PatrolConcurrency: &pc, PatrolBatchSize: &pb,
		PatrolModel: &pm, PatrolProxyURL: &pp, PatrolAutoModelSwitch: &pams,
		PatrolMode: &pmode,
		RegisterEnabled: &re, RegisterBaseURL: &rurl, RegisterAdminBase: &radmin,
		RegisterPassword: &rpw, RegisterTimeoutSec: &rto, RegisterDryRun: &rdry,
		RegisterManualDefaultCount: &rmd, RegisterManualMaxCount: &rmm,
		RegisterAutoEnabled: &rae, RegisterAutoIntervalSec: &rais, RegisterAutoCount: &rac,
		RegisterFloorEnabled: &rfe, RegisterFloorMinPool: &rfmin, RegisterFloorCount: &rfc, RegisterFloorIntervalSec: &rfi,
		RegisterAutoOnlyWhenIdle: &raoi, RegisterAutoRequireHealth: &rarh, RegisterAutoPauseOnLow: &rapl,
		RegisterHealthIntervalSec: &rhis, RegisterHealthWindowJobs: &rhwj, RegisterHealthMinSamples: &rhms,
		RegisterHealthOKRate: &rhor, RegisterHealthWarnRate: &rhwr, RegisterRequireCPAok: &rrc,
		RegisterReloginOnAuth401: &roa, RegisterReloginMaxStreak: &rms, RegisterReloginConcurrency: &rrcy,
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
	case "register_enabled":
		cur.RegisterEnabled = &val
	case "register_auto_enabled":
		cur.RegisterAutoEnabled = &val
	case "register_dry_run":
		cur.RegisterDryRun = &val
	}
	return cur
}
