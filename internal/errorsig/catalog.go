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
			Key: "free_usage_429", Label: "429·免费额度用尽", Enabled: true,
			Action: ActionCooldown, Threshold: 1, CooldownSec: 0, Source: "builtin",
			Note: "额度类：应冷却，不删号；请求/巡查统一此标签",
		},
		"spending_limit_402": {
			Key: "spending_limit_402", Label: "402·消费限额", Enabled: true,
			Action: ActionCooldown, Threshold: 1, CooldownSec: 86400, NeverTrash: true, Source: "builtin",
			Note: "硬规则：永不自动进垃圾箱",
		},
		"auth_401": {
			Key: "auth_401", Label: "401·凭证失效", Enabled: true,
			Action: ActionCandidate, Threshold: 2, CooldownSec: 3600, Source: "builtin",
			Note: "连续命中后候删，人工确认",
		},
		"permission_403": {
			Key: "permission_403", Label: "403·权限拒绝", Enabled: true,
			Action: ActionCooldown, Threshold: 3, CooldownSec: 1800, Source: "builtin",
			Note: "默认可恢复，不建议自动删除",
		},
		"code:invalid-argument": {
			Key: "code:invalid-argument", Label: "参数无效/上下文过长", Enabled: true,
			Action: ActionObserve, Threshold: 3, CooldownSec: 0, NeverTrash: true, Source: "builtin",
			Note: "常见于上下文超长/非法参数；默认仅观察",
		},
		"http_404": {
			Key: "http_404", Label: "404·路径/网关", Enabled: true,
			Action: ActionObserve, Threshold: 3, CooldownSec: 0, NeverTrash: true, Source: "builtin",
			Note: "多为路径/网关问题，不等于账号死号",
		},
	}
}

// KeyFromMatch builds catalog key from classifier result + status.
func KeyFromMatch(res match.Result, statusCode int) string {
	if res.Signal != match.SignalNone {
		return string(res.Signal)
	}
	// collapse common HTTP statuses into builtin keys when possible
	if statusCode == 401 {
		return "auth_401"
	}
	if statusCode == 403 {
		return "permission_403"
	}
	if statusCode == 402 {
		return "spending_limit_402"
	}
	if statusCode == 429 {
		return "free_usage_429"
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
		return "429·免费额度用尽"
	case "spending_limit_402":
		return "402·消费限额"
	case "auth_401":
		return "401·凭证失效"
	case "permission_403":
		return "403·权限拒绝"
	case "unmatched":
		return "未分类错误"
	case "http_404":
		return "404·路径/网关"
	case "code:invalid-argument":
		return "参数无效/上下文过长"
	case "http_0_disabled", "reason:cpa_disabled":
		return "CPA文件禁用(同步)"
	case "reason:region_block":
		return "区域限制"
	}
	if strings.HasPrefix(key, "code:") {
		return "错误码 " + strings.TrimPrefix(key, "code:")
	}
	if strings.HasPrefix(key, "http_") {
		return "HTTP " + strings.TrimPrefix(key, "http_")
	}
	if res.Reason == "region_block" {
		return "区域限制"
	}
	if res.Code != "" {
		return "错误码 " + res.Code
	}
	if statusCode > 0 {
		return fmt.Sprintf("未分类 HTTP %d", statusCode)
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
	switch {
	case key == "free_usage_429" || strings.Contains(low, "free-usage") || strings.Contains(low, "free_usage"):
		return "免费额度用尽"
	case key == "spending_limit_402" || strings.Contains(low, "spending-limit") || strings.Contains(low, "run out of credits"):
		return "消费限额"
	case key == "permission_403" || strings.Contains(low, "permission-denied") || strings.Contains(low, "access to the chat endpoint is denied"):
		return "权限拒绝"
	case key == "auth_401" || strings.Contains(low, "authentication required") || strings.Contains(low, "invalid or expired credentials") || strings.Contains(low, "unauthorized"):
		return "凭证失效"
	case key == "http_404" || strings.Contains(low, "404 not found") || strings.Contains(low, "<html"):
		return "路径/网关 404"
	case key == "code:invalid-argument" || (strings.Contains(low, "invalid-argument") && strings.Contains(low, "maximum prompt")):
		return "上下文过长/参数无效"
	case strings.Contains(low, "invalid-argument"):
		return "参数无效"
	case strings.Contains(low, "unexpected eof") || strings.Contains(low, ": eof") || strings.HasSuffix(low, "eof"):
		return "连接中断"
	case strings.Contains(low, "cpa auth file disabled") || strings.Contains(low, "cpa_disabled"):
		return "CPA文件已禁用"
	case strings.Contains(low, "region"):
		return "区域限制"
	case key != "" && !strings.HasPrefix(key, "unmatched") && !strings.HasPrefix(key, "http_") && !strings.HasPrefix(key, "code:") && !strings.HasPrefix(key, "reason:"):
		// known key label without parens noise
		lab := LabelOf(key, match.Result{}, status)
		return strings.TrimSpace(strings.Split(lab, "(")[0])
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
		if status > 0 {
			return fmt.Sprintf("HTTP %d", status)
		}
		return "未分类错误"
	}
	// shorten URL-ish network errors
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
	// plain short text
	if len(s) > 48 {
		s = s[:48] + "…"
	}
	return s
}

// ShapeOf returns a stable fingerprint for splitting unmatched errors.
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
	case status == 401 || strings.Contains(low, "凭证失效") || strings.Contains(low, "authentication"):
		return "auth_401", "401·凭证失效", "auth_401"
	case status == 403 || strings.Contains(low, "权限拒绝") || strings.Contains(low, "permission"):
		return "permission_403", "403·权限拒绝", "permission_403"
	case status == 404:
		return "http_404", "404·路径/网关", "http_404"
	case status > 0:
		return fmt.Sprintf("http_%d", status), fmt.Sprintf("HTTP %d", status), fmt.Sprintf("http_%d", status)
	default:
		// fingerprint first 40 runes of human msg
		fp := msg
		if len(fp) > 40 {
			fp = fp[:40]
		}
		return "msg:" + sanitize(fp), msg, "reason:" + sanitize(fp)
	}
}

func htmlUnescape(s string) string {
	// tiny local unescape to avoid importing html in this package cycle risk — use strings replacer
	r := strings.NewReplacer(
		"&#34;", `"`, "&quot;", `"`, "&#39;", "'", "&apos;", "'",
		"&lt;", "<", "&gt;", ">", "&amp;", "&",
		"\\r", "", "\\n", " ",
	)
	return r.Replace(s)
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
