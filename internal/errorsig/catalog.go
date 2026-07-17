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

// CollapseTarget returns where a dirty/legacy key should be merged, if at all.
// User-created split keys (reason:*, custom labels) are NEVER collapsed.
//
// Rules:
//   - free_usage_429:<suffix> → free_usage_429
//   - permission_403:<suffix> → permission_403
//   - legacy bare builtins (auth_401, spending_limit_402, http_404, …) → unmatched
//   - reason:… / user custom keys → keep (ok=false)
//   - free_usage_429 / permission_403 / any_error / unmatched → keep
func CollapseTarget(key string) (target string, ok bool) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "unmatched", true
	}
	switch key {
	case "any_error", "unmatched", "free_usage_429", "permission_403":
		return "", false
	}
	// User splits use reason: / custom names — never auto-merge.
	if strings.HasPrefix(key, "reason:") {
		return "", false
	}
	if i := strings.IndexByte(key, ':'); i > 0 {
		parent := key[:i]
		if parent == "free_usage_429" || parent == "permission_403" {
			return parent, true
		}
		// code:invalid-argument and similar legacy → unmatched
		if parent == "code" || parent == "http" || parent == "msg" {
			return "unmatched", true
		}
		// unknown parent:suffix (user custom) → keep
		return "", false
	}
	// bare legacy keys
	switch key {
	case "auth_401", "spending_limit_402", "http_404", "http_401", "http_0_disabled":
		return "unmatched", true
	}
	if strings.HasPrefix(key, "http_") {
		return "unmatched", true
	}
	// bare custom key → keep
	return "", false
}

// NormalizeCatalogKey is kept for display fallbacks: only forces known builtins/unmatched.
// Does NOT rewrite user split keys (reason:…).
func NormalizeCatalogKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "unmatched"
	}
	if t, ok := CollapseTarget(key); ok {
		return t
	}
	return key
}

// KeyFromMatch builds catalog key from classifier result + status.
// Only 429/403 are auto-classified; everything else goes to "unmatched".
func KeyFromMatch(res match.Result, statusCode int, body ...string) string {
	if res.Signal == match.SignalFreeUsage429 {
		return "free_usage_429"
	}
	if res.Signal == match.SignalPermission403 {
		return "permission_403"
	}
	// status-only collapse for the two builtins when signal empty but HTTP clear
	if statusCode == 429 {
		return "free_usage_429"
	}
	if statusCode == 403 {
		return "permission_403"
	}
	// body hint (free-usage / permission) when status weird
	if len(body) > 0 {
		low := strings.ToLower(body[0])
		if strings.Contains(low, "free-usage") || strings.Contains(low, "free_usage") {
			return "free_usage_429"
		}
		if strings.Contains(low, "permission-denied") || strings.Contains(low, "access to the chat endpoint is denied") {
			return "permission_403"
		}
	}
	return "unmatched"
}

func LabelOf(key string, res match.Result, statusCode int) string {
	key = NormalizeCatalogKey(key)
	switch key {
	case "free_usage_429":
		return "免费额度用尽"
	case "permission_403":
		return "权限拒绝"
	case "reason:http_426", "http_426":
		return "终端版本过低"
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

// ShapeOf returns a stable fingerprint for splitting unmatched errors.
// Suggest keys stay under reason:/shape — never auto-create sibling 403/429 cards.
func ShapeOf(sample string, status int) (shape, label, suggestKey string) {
	msg := HumanMsg("", sample, status)
	low := strings.ToLower(sample + " " + msg)
	switch {
	case strings.Contains(low, "连接中断") || strings.Contains(low, "eof"):
		return "net_eof", "连接中断", "reason:net_eof"
	case strings.Contains(low, "timeout") || strings.Contains(low, "超时"):
		return "net_timeout", "请求超时", "reason:net_timeout"
	case strings.Contains(low, "cpa") && strings.Contains(low, "disabled"):
		return "cpa_disabled", "CPA文件已禁用", "reason:cpa_disabled"
	case strings.Contains(low, "region"):
		return "region_block", "区域限制", "reason:region_block"
	case status == 402 || strings.Contains(low, "spending-limit") || strings.Contains(low, "消费限额") || strings.Contains(low, "run out of credits"):
		return "spending_402", "消费限额", "reason:spending_limit_402"
	case status == 401 || strings.Contains(low, "凭证失效") || strings.Contains(low, "authentication"):
		return "auth_401", "凭证失效", "reason:auth_401"
	case status == 404:
		return "http_404", "路径/网关 404", "reason:http_404"
	case status == 429 || strings.Contains(low, "free-usage") || strings.Contains(low, "免费额度"):
		// should already be free_usage_429 catalog; if under unmatched, suggest merge back
		return "free_usage_429", "免费额度用尽", "free_usage_429"
	case status == 403 || strings.Contains(low, "权限拒绝") || strings.Contains(low, "permission"):
		return "permission_403", "权限拒绝", "permission_403"
	case status == 426 || strings.Contains(low, "cli version") || (strings.Contains(low, "outdated") && strings.Contains(low, "grok")):
		// stable shape so user split + routeBySplitShape keep matching
		return "http_426", "终端版本过低", "reason:http_426"
	case status > 0:
		return fmt.Sprintf("http_%d", status), fmt.Sprintf("HTTP %d", status), fmt.Sprintf("reason:http_%d", status)
	default:
		fp := msg
		if len(fp) > 40 {
			fp = fp[:40]
		}
		return "msg:" + sanitize(fp), msg, "reason:" + sanitize(fp)
	}
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

