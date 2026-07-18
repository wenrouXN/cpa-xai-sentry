package errorsig

import (
	"fmt"
	"strings"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
	"github.com/openclaw-local/cpa-xai-sentry/internal/match"
)

// Action for a single error key.
type Action string

const (
	ActionObserve   Action = "observe"   // 仅记录
	ActionCooldown  Action = "cooldown"  // 冷却禁用
	ActionCandidate Action = "candidate" // 候选（人工确认）
	ActionDisable   Action = "disable"   // 永久禁用（不自动恢复）
	ActionTrash     Action = "trash"     // 进垃圾箱
)

// Policy is per-error control knobs.
type Policy struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Enabled     bool   `json:"enabled"`
	Action      Action `json:"action"`
	Threshold   int    `json:"threshold"`
	CooldownSec int    `json:"cooldown_seconds"`
	NeverTrash  bool   `json:"never_trash"`
	Note        string `json:"note"`
	Source      string `json:"source"` // builtin|learned
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// Observed is a dynamically collected error fingerprint.
type Observed struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	Signal     string    `json:"signal"`
	Code       string    `json:"code"`
	StatusCode int       `json:"status_code"`
	Count      int64     `json:"count"`
	LastAt     time.Time `json:"last_at"`
	Sample     string    `json:"sample"`
	LastAuth   string    `json:"last_auth"`
	LastFile   string    `json:"last_file"`
}

// BuiltinDefaults seeds ONLY free_usage_429 + permission_403 (+ callers may add any_error).
// Everything else is catalogued under "unmatched" until the user splits it.
func BuiltinDefaults() map[string]Policy {
	return map[string]Policy{
		"free_usage_429": {
			Key: "free_usage_429", Label: "免费额度用尽", Enabled: true,
			Action: ActionCooldown, Threshold: 1, CooldownSec: 0, Source: "builtin",
			Note: "额度类：应冷却，不删号；请求/巡查统一此标签",
		},
		"permission_403": {
			Key: "permission_403", Label: "权限拒绝", Enabled: true,
			Action: ActionCooldown, Threshold: 3, CooldownSec: 1800, Source: "builtin",
			Note: "默认阶梯：连续≥3冷却；≥15永久禁用（可在面板改）",
		},
	}
}

// IsBuiltinCatalogKey reports whether key is one of the two built-in classes (or any_error).
func IsBuiltinCatalogKey(key string) bool {
	switch strings.TrimSpace(key) {
	case "free_usage_429", "permission_403", "any_error", "unmatched":
		return true
	default:
		return false
	}
}

// NormalizeCatalogKey only fills an empty key. v2 never rewrites persisted
// classifier keys; split/merge is explicit user state.
func NormalizeCatalogKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "unmatched"
	}
	return key
}

// KeyFromMatch builds catalog key from classifier result + status/body.
// Only exact free_usage_429 and permission_403 are auto-classified.
// HTTP status alone never decides the class (bare 429 stays unmatched).
func KeyFromMatch(res match.Result, statusCode int, body ...string) string {
	if res.Signal == match.SignalFreeUsage429 {
		return "free_usage_429"
	}
	if res.Signal == match.SignalPermission403 {
		return "permission_403"
	}
	// body evidence when signal missed (weird status / partial body)
	if len(body) > 0 {
		low := strings.ToLower(body[0])
		if strings.Contains(low, "free-usage") || strings.Contains(low, "free_usage") ||
			strings.Contains(low, "included free usage") {
			return "free_usage_429"
		}
		if strings.Contains(low, "permission-denied") || strings.Contains(low, "access to the chat endpoint is denied") {
			return "permission_403"
		}
	}
	_ = statusCode // status alone is never a class
	return "unmatched"
}

func LabelOf(key string, res match.Result, statusCode int) string {
	key = NormalizeCatalogKey(key)
	switch key {
	case "free_usage_429":
		return "免费额度用尽"
	case "permission_403":
		return "权限拒绝"
	case "unmatched":
		return "未分类错误"
	case "any_error":
		return "任意错误·连续"
	}
	if key != "" {
		return key
	}
	return "未分类错误"
}

// HumanMsg turns raw sample/body into short Chinese error text (no source suffix).
func HumanMsg(key, sample string, status int) string {
	s := strings.TrimSpace(htmlUnescape(sample))
	low := strings.ToLower(s)
	key = NormalizeCatalogKey(key)
	switch {
	case key == "free_usage_429" || strings.Contains(low, "free-usage") || strings.Contains(low, "free_usage"):
		return "免费额度用尽"
	case key == "permission_403" || strings.Contains(low, "permission-denied") || strings.Contains(low, "access to the chat endpoint is denied"):
		return "权限拒绝"
	case strings.Contains(low, "spending-limit") || strings.Contains(low, "run out of credits") || status == 402:
		return "消费限额"
	case strings.Contains(low, "authentication required") || strings.Contains(low, "invalid or expired credentials") || strings.Contains(low, "unauthorized") || status == 401:
		return "凭证失效"
	case strings.Contains(low, "404 not found") || strings.Contains(low, "<html") || status == 404:
		return "路径/网关 404"
	case strings.Contains(low, "invalid-argument") && strings.Contains(low, "maximum prompt"):
		return "上下文过长/参数无效"
	case strings.Contains(low, "invalid-argument"):
		return "参数无效"
	case strings.Contains(low, "unexpected eof") || strings.Contains(low, ": eof") || strings.HasSuffix(low, "eof"):
		return "连接中断"
	case strings.Contains(low, "cpa auth file disabled") || strings.Contains(low, "cpa_disabled"):
		return "CPA文件已禁用"
	case strings.Contains(low, "region"):
		return "区域限制"
	case strings.Contains(low, "cli version") || strings.Contains(low, "grok cli") && strings.Contains(low, "outdated") ||
		strings.Contains(low, "please update to version") || status == 426:
		return "终端版本过低"
	}
	// strip json/html noise
	if strings.HasPrefix(s, "{") || strings.Contains(low, "<html") {
		if status == 401 {
			return "凭证失效"
		}
		if status == 403 {
			return "权限拒绝"
		}
		if status == 404 {
			return "路径/网关 404"
		}
		if status == 429 {
			return "免费额度用尽"
		}
		if status == 426 {
			return "终端版本过低"
		}
		if status > 0 {
			return fmt.Sprintf("HTTP %d", status)
		}
		return "未分类错误"
	}
	if strings.Contains(low, "post \"http") || strings.Contains(low, "get \"http") {
		if strings.Contains(low, "eof") {
			return "连接中断"
		}
		if strings.Contains(low, "timeout") {
			return "请求超时"
		}
		return "网络错误"
	}
	if s == "" {
		if status > 0 {
			return fmt.Sprintf("HTTP %d", status)
		}
		return "未分类错误"
	}
	if len(s) > 48 {
		s = s[:48] + "…"
	}
	return s
}

// ShapeOf returns a stable fingerprint from the actual response shape.
// HTTP status is one component, never the whole identity. Known builtins retain
// their stable keys; every other shape returns a suggested reason:fp_<hash> key
// for user split/merge, but callers decide whether to promote it.
func ShapeOf(sample string, status int) (shape, label, suggestKey string) {
	fp := errorfp.Build(sample, status)
	return fp.Shape, HumanMsg("", sample, status), fp.SuggestKey
}

func htmlUnescape(s string) string {
	r := strings.NewReplacer(
		"&#34;", `"`, "&quot;", `"`, "&#39;", "'", "&apos;", "'",
		"&lt;", "<", "&gt;", ">", "&amp;", "&",
		"\\r", "", "\\n", " ",
	)
	return r.Replace(s)
}

// ExtractBodyError extracts a short error identifier from a JSON response body.
// Tries "error", "message", "detail", "code" fields. Returns "" if not JSON or no match.
func ExtractBodyError(body string) string {
	if body == "" {
		return ""
	}
	// keep lightweight — full JSON parse lives in match.Classify
	low := strings.ToLower(body)
	if i := strings.Index(low, `"code"`); i >= 0 {
		// crude extract after "code":
		rest := body[i:]
		if j := strings.Index(rest, ":"); j >= 0 {
			rest = strings.TrimSpace(rest[j+1:])
			rest = strings.Trim(rest, `",} `)
			if rest != "" && len(rest) < 80 {
				return rest
			}
		}
	}
	return ""
}

func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			return r
		}
		if r == ' ' || r == '/' || r == ':' {
			return '_'
		}
		return -1
	}, s)
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}

// HardNeverTrash is deprecated: no hard ban; trash follows policy/global config only.
// Kept for API compat — always false.
func HardNeverTrash(key string) bool {
	return false
}
