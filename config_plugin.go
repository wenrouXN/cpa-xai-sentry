//go:build cshared

package main

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/openclaw-local/cpa-xai-sentry/internal/sentrycfg"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

func configDefaults() sentrycfg.Config {
	return sentrycfg.Default()
}

func configFields() []pluginapi.ConfigField {
	return []pluginapi.ConfigField{
		{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "CPA 加载插件开关"},
		{Name: "sentry_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Sentry 功能总开关（UI 可切换）"},
		{Name: "auto_cooldown", Type: pluginapi.ConfigFieldTypeBoolean, Description: "自动冷却"},
		{Name: "auto_candidate", Type: pluginapi.ConfigFieldTypeBoolean, Description: "自动进入候选"},
		{Name: "auto_delete", Type: pluginapi.ConfigFieldTypeBoolean, Description: "自动移入垃圾箱（非硬删）"},
		{Name: "trash_retention_days", Type: pluginapi.ConfigFieldTypeNumber, Description: "垃圾箱保留天数（默认7）"},
		{Name: "trash_auto_purge", Type: pluginapi.ConfigFieldTypeBoolean, Description: "到期自动彻底清除"},
		{Name: "trash_dir", Type: pluginapi.ConfigFieldTypeString, Description: "垃圾箱快照目录"},
		{Name: "restore_default_disabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "恢复默认禁用"},
		{Name: "tick_seconds", Type: pluginapi.ConfigFieldTypeNumber, Description: "tick 周期(秒)"},
		{Name: "management_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPA 管理 API 基址"},
		{Name: "management_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPA management key（敏感）"},
		{Name: "state_path", Type: pluginapi.ConfigFieldTypeString, Description: "状态 JSON 路径"},
		{Name: "auth_dir", Type: pluginapi.ConfigFieldTypeString, Description: "auth 文件目录"},
		{Name: "patrol_enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "定时巡查"},
		{Name: "patrol_interval", Type: pluginapi.ConfigFieldTypeNumber, Description: "巡查周期(秒)"},
		{Name: "patrol_timeout", Type: pluginapi.ConfigFieldTypeNumber, Description: "探测超时(秒)"},
		{Name: "patrol_batch_size", Type: pluginapi.ConfigFieldTypeNumber, Description: "每轮上限"},
		{Name: "patrol_concurrency", Type: pluginapi.ConfigFieldTypeNumber, Description: "并发"},
		{Name: "patrol_model", Type: pluginapi.ConfigFieldTypeString, Description: "探测模型"},
		{Name: "patrol_proxy_url", Type: pluginapi.ConfigFieldTypeString, Description: "巡查代理"},
		{Name: "cpamp_url", Type: pluginapi.ConfigFieldTypeString, Description: "CPAMP 基址(可选)"},
		{Name: "cpamp_admin_key", Type: pluginapi.ConfigFieldTypeString, Description: "CPAMP key(敏感)"},
	}
}

func parseConfigFromReconfigure(request []byte) sentrycfg.Config {
	cfg := configDefaults()
	if len(request) == 0 {
		return cfg
	}
	var raw map[string]any
	if err := json.Unmarshal(request, &raw); err != nil {
		return cfg
	}
	if yb, ok := extractYAMLBytes(raw); ok {
		var m map[string]any
		if yaml.Unmarshal(yb, &m) == nil {
			applyConfigMap(&cfg, m)
		}
		return cfg.Validate()
	}
	configMap := raw
	if nested, ok := raw["config"].(map[string]any); ok {
		configMap = nested
	}
	applyConfigMap(&cfg, configMap)
	return cfg.Validate()
}

func extractYAMLBytes(raw map[string]any) ([]byte, bool) {
	v, ok := raw["config_yaml"]
	if !ok || v == nil {
		return nil, false
	}
	switch t := v.(type) {
	case string:
		if decoded, err := base64.StdEncoding.DecodeString(t); err == nil {
			return decoded, true
		}
		return []byte(t), true
	case []byte:
		return t, true
	default:
		return nil, false
	}
}

func applyConfigMap(cfg *sentrycfg.Config, m map[string]any) {
	if m == nil {
		return
	}
	if v, ok := asBool(m["enabled"]); ok {
		cfg.Enabled = v
	}
	if v, ok := asBool(m["sentry_enabled"]); ok {
		cfg.SentryEnabled = v
	}
	// if only enabled present, keep sentry_enabled default true when enabled
	if _, has := m["sentry_enabled"]; !has {
		if v, ok := asBool(m["enabled"]); ok {
			cfg.SentryEnabled = v
		}
	}
	if v, ok := asBool(m["auto_cooldown"]); ok {
		cfg.AutoCooldown = v
	}
	if v, ok := asBool(m["auto_candidate"]); ok {
		cfg.AutoCandidate = v
	}
	if v, ok := asBool(m["auto_delete"]); ok {
		cfg.AutoDelete = v
	}
	if v, ok := asStringSlice(m["cooldown_signals"]); ok {
		cfg.CooldownSignals = v
	}
	if v, ok := asStringSlice(m["candidate_signals"]); ok {
		cfg.CandidateSignals = v
	}
	if v, ok := asStringSlice(m["delete_signals"]); ok {
		cfg.DeleteSignals = v
	}
	if v, ok := asIntMap(m["signal_thresholds"]); ok {
		cfg.SignalThresholds = v
	}
	if v, ok := asFloat(m["tick_seconds"]); ok && v > 0 {
		cfg.TickSeconds = int(v)
	}
	if v, ok := asFloat(m["max_reset_seconds"]); ok && v > 0 {
		cfg.MaxResetSeconds = int(v)
	}
	if v, ok := asFloat(m["min_reset_seconds"]); ok && v >= 0 {
		cfg.MinResetSeconds = int(v)
	}
	if v, ok := asFloat(m["permission_cooldown_seconds"]); ok && v > 0 {
		cfg.PermissionCooldownSec = int(v)
	}
	if v, ok := asFloat(m["auth401_cooldown_seconds"]); ok && v > 0 {
		cfg.Auth401CooldownSec = int(v)
	}
	if v, ok := asString(m["management_url"]); ok {
		cfg.ManagementURL = strings.TrimSpace(v)
	}
	if v, ok := asString(m["management_key"]); ok {
		cfg.ManagementKey = strings.TrimSpace(v)
	}
	if v, ok := asString(m["state_path"]); ok && strings.TrimSpace(v) != "" {
		cfg.StatePath = strings.TrimSpace(v)
	}
	if v, ok := asString(m["auth_dir"]); ok && strings.TrimSpace(v) != "" {
		cfg.AuthDir = strings.TrimSpace(v)
	}
	// patrol_auth_dir alias
	if v, ok := asString(m["patrol_auth_dir"]); ok && strings.TrimSpace(v) != "" {
		cfg.AuthDir = strings.TrimSpace(v)
	}
	if v, ok := asString(m["trash_dir"]); ok && strings.TrimSpace(v) != "" {
		cfg.TrashDir = strings.TrimSpace(v)
	}
	if v, ok := asFloat(m["trash_retention_days"]); ok && v > 0 {
		cfg.TrashRetentionDays = int(v)
	}
	if v, ok := asBool(m["trash_auto_purge"]); ok {
		cfg.TrashAutoPurge = v
	}
	if v, ok := asBool(m["restore_default_disabled"]); ok {
		cfg.RestoreDefaultDis = v
	}
	if v, ok := asBool(m["patrol_enabled"]); ok {
		cfg.PatrolEnabled = v
	}
	if v, ok := asFloat(m["patrol_interval"]); ok && v > 0 {
		cfg.PatrolInterval = int(v)
	}
	if v, ok := asFloat(m["patrol_timeout"]); ok && v > 0 {
		cfg.PatrolTimeout = int(v)
	}
	if v, ok := asFloat(m["patrol_concurrency"]); ok && v > 0 {
		cfg.PatrolConcurrency = int(v)
	}
	if v, ok := asFloat(m["patrol_batch_size"]); ok && v >= 0 {
		cfg.PatrolBatchSize = int(v)
	}
	if v, ok := asString(m["patrol_model"]); ok {
		cfg.PatrolModel = strings.TrimSpace(v)
	}
	if v, ok := asString(m["patrol_proxy_url"]); ok {
		cfg.PatrolProxyURL = strings.TrimSpace(v)
	}
	if v, ok := asBool(m["patrol_auto_model_switch"]); ok {
		cfg.PatrolAutoModelSwitch = v
	}
	if v, ok := asString(m["cpamp_url"]); ok {
		cfg.CPAMPURL = strings.TrimSpace(v)
	}
	if v, ok := asString(m["cpamp_admin_key"]); ok {
		cfg.CPAMPAdminKey = strings.TrimSpace(v)
	}
	if v, ok := asBool(m["cpamp_usage_floor"]); ok {
		cfg.CPAMPUsageFloor = v
	}
}

func asBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		if s == "true" || s == "1" || s == "yes" {
			return true, true
		}
		if s == "false" || s == "0" || s == "no" {
			return false, true
		}
	case float64:
		return t != 0, true
	case int:
		return t != 0, true
	}
	return false, false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	}
	return 0, false
}

func asString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	}
	return "", false
}

func asStringSlice(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	}
	return nil, false
}

func asIntMap(v any) (map[string]int, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	out := map[string]int{}
	for k, val := range m {
		if f, ok := asFloat(val); ok {
			out[k] = int(f)
		}
	}
	return out, true
}

func redactConfig(cfg sentrycfg.Config) map[string]any {
	return map[string]any{
		"enabled":                 cfg.Enabled,
		"sentry_enabled":          cfg.SentryEnabled,
		"auto_cooldown":           cfg.AutoCooldown,
		"auto_candidate":          cfg.AutoCandidate,
		"auto_delete":             cfg.AutoDelete,
		"cooldown_signals":        cfg.CooldownSignals,
		"candidate_signals":       cfg.CandidateSignals,
		"delete_signals":          cfg.DeleteSignals,
		"signal_thresholds":       cfg.SignalThresholds,
		"tick_seconds":            cfg.TickSeconds,
		"trash_retention_days":    cfg.TrashRetentionDays,
		"trash_auto_purge":        cfg.TrashAutoPurge,
		"restore_default_disabled": cfg.RestoreDefaultDis,
		"management_url":          cfg.ManagementURL,
		"management_key_set":      cfg.ManagementKey != "",
		"state_path":              cfg.StatePath,
		"auth_dir":                cfg.AuthDir,
		"trash_dir":               cfg.TrashDir,
		"patrol_enabled":          cfg.PatrolEnabled,
		"patrol_interval":         cfg.PatrolInterval,
		"patrol_model":            cfg.PatrolModel,
		"cpamp_url":               cfg.CPAMPURL,
		"cpamp_admin_key_set":     cfg.CPAMPAdminKey != "",
	}
}
