package errorsig

import (
	"fmt"
	"strings"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
)

// Action for a single error key.
type Action string

const (
	ActionObserve   Action = "observe"   // 仅记录
	ActionCooldown  Action = "cooldown"  // 冷却禁用
	ActionCandidate Action = "candidate" // 候选（人工确认）
	ActionTrash     Action = "trash"     // 进垃圾箱
)

// Policy is per-error control knobs.
type Policy struct {
	Key           string `json:"key"`
	Label         string `json:"label"`
	Enabled       bool   `json:"enabled"`
	Action        Action `json:"action"`
	Threshold     int    `json:"threshold"`
	CooldownSec   int    `json:"cooldown_seconds"`
	NeverTrash    bool   `json:"never_trash"`
	Note          string `json:"note"`
	Source        string `json:"source"` // builtin|learned
	UpdatedAt     string `json:"updated_at,omitempty"`
}

// Observed is a dynamically collected error fingerprint.
type Observed struct {
	Key         string    `json:"key"`
	Label       string    `json:"label"`
	Signal      string    `json:"signal"`
	Code        string    `json:"code"`
	StatusCode  int       `json:"status_code"`
	Count       int64     `json:"count"`
	LastAt      time.Time `json:"last_at"`
	Sample      string    `json:"sample"`
	LastAuth    string    `json:"last_auth"`
	LastFile    string    `json:"last_file"`
}

// BuiltinDefaults seeds known xAI failure classes.
func BuiltinDefaults() map[string]Policy {
	return map[string]Policy{
		"free_usage_429": {
			Key: "free_usage_429", Label: "免费额度耗尽(429)", Enabled: true,
			Action: ActionCooldown, Threshold: 1, CooldownSec: 0, Source: "builtin",
			Note: "额度类：应冷却，不删号",
		},
		"spending_limit_402": {
			Key: "spending_limit_402", Label: "消费限额(402)", Enabled: true,
			Action: ActionCooldown, Threshold: 1, CooldownSec: 86400, NeverTrash: true, Source: "builtin",
			Note: "硬规则：永不自动进垃圾箱",
		},
		"auth_401": {
			Key: "auth_401", Label: "凭证失效(401)", Enabled: true,
			Action: ActionCandidate, Threshold: 2, CooldownSec: 3600, Source: "builtin",
			Note: "连续命中后进候选，人工确认",
		},
		"permission_403": {
			Key: "permission_403", Label: "权限拒绝(403)", Enabled: true,
			Action: ActionCooldown, Threshold: 3, CooldownSec: 1800, Source: "builtin",
			Note: "默认可恢复，不建议自动删除",
		},
	}
}

// KeyFromMatch builds catalog key from classifier result + status.
func KeyFromMatch(res match.Result, statusCode int) string {
	if res.Signal != match.SignalNone {
		return string(res.Signal)
	}
	if res.Code != "" {
		return "code:" + sanitize(res.Code)
	}
	if statusCode > 0 {
		return fmt.Sprintf("http_%d", statusCode)
	}
	if res.Reason != "" && res.Reason != "unmatched" {
		return "reason:" + sanitize(res.Reason)
	}
	return "unmatched"
}

func LabelOf(key string, res match.Result, statusCode int) string {
	switch key {
	case "free_usage_429":
		return "免费额度耗尽(429)"
	case "spending_limit_402":
		return "消费限额(402)"
	case "auth_401":
		return "凭证失效(401)"
	case "permission_403":
		return "权限拒绝(403)"
	}
	if strings.HasPrefix(key, "code:") {
		return "错误码 " + strings.TrimPrefix(key, "code:")
	}
	if strings.HasPrefix(key, "http_") {
		return "HTTP " + strings.TrimPrefix(key, "http_")
	}
	if res.Reason == "region_block" {
		return "区域限制(仅观察)"
	}
	if statusCode > 0 {
		return fmt.Sprintf("未分类错误 HTTP %d", statusCode)
	}
	return "未分类错误"
}

func sanitize(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.ReplaceAll(s, " ", "_")
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}

// HardNeverTrash returns true for keys that must never auto-trash.
func HardNeverTrash(key string) bool {
	return key == "spending_limit_402" || strings.Contains(key, "spending-limit")
}
