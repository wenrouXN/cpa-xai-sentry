package state

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/errorfp"
)

type AccountState string

const (
	Active             AccountState = "active"
	CooldownQuota      AccountState = "cooldown_quota"      // generic cool bucket (free_usage 429 OR fingerprint cool); display uses last_signal
	CooldownSpending   AccountState = "cooldown_spending"   // 402 spending
	CooldownPermission AccountState = "cooldown_permission" // 403 permission cool
	CandidateDead      AccountState = "candidate_dead"
	UserManual         AccountState = "user_manual"
	Trashed            AccountState = "trashed"
	Purged             AccountState = "purged"
)

const Owner = "cpa_xai_sentry"
const schemaVersion = "2"

type Account struct {
	AuthIndex     string         `json:"auth_index"`
	FileName      string         `json:"file_name"`
	Email         string         `json:"email"`
	Tier          string         `json:"tier"`
	State         AccountState   `json:"state"`
	DisableSource string         `json:"disable_source"` // plugin_auto|user_manual|""
	Owner         string         `json:"owner"`
	PreDisabled   bool           `json:"pre_disabled"`
	LastSignal    string         `json:"last_signal"`
	Streaks       map[string]int `json:"streaks"`
	RecoverAt     time.Time      `json:"recover_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	// LastActionAt/LastAction: last sentry action log on this account (cooldown/reopen/...)
	LastActionAt time.Time `json:"last_action_at,omitempty"`
	LastAction   string    `json:"last_action,omitempty"`
	// PendingObserve: after cool/候删 auto-recover (ResetToActive). UI shows「恢复待观察」
	// until a successful request proves the account is clean. Ladder streaks stay intact.
	PendingObserve bool `json:"pending_observe,omitempty"`
	// PendingSince: when PendingObserve was first set (not refreshed by heal re-touch).
	// Used for idle TTL: long-idle pending without traffic → auto clear.
	PendingSince time.Time `json:"pending_since,omitempty"`
	// HealFailStreak: consecutive heal cycles where CPA file stayed/returned disabled.
	// When high enough, escalate to CPA已禁用 and stop force-open spam.
	HealFailStreak int `json:"heal_fail_streak,omitempty"`
	// LastHealAt: last heal_active_file *attempt* (success or fail). Hard rate-limit key —
	// do not rely on LastAction (may be overwritten by cooldown/patrol and re-trigger heal spam).
	LastHealAt time.Time `json:"last_heal_at,omitempty"`
	// Best-effort quota accounting (from error bodies / CPAMP day floors).
	QuotaLimit     int64     `json:"quota_limit,omitempty"`
	QuotaUsed      int64     `json:"quota_used,omitempty"`
	QuotaRemaining int64     `json:"quota_remaining,omitempty"`
	QuotaSource    string    `json:"quota_source,omitempty"`
	QuotaUpdatedAt time.Time `json:"quota_updated_at,omitempty"`
	DayCalls       int64     `json:"day_calls,omitempty"`
	DayFailCalls   int64     `json:"day_fail_calls,omitempty"`
	DayTokens      int64     `json:"day_tokens,omitempty"`
	DayKey         string    `json:"day_key,omitempty"`
}

type ActionLog struct {
	At     time.Time `json:"at"`
	Auth   string    `json:"auth"`
	Source string    `json:"source"`
	Signal string    `json:"signal"`
	Action string    `json:"action"`
	Reason string    `json:"reason"`
}

type TrashMeta struct {
	ID        string    `json:"id"`
	AuthIndex string    `json:"auth_index"`
	Email     string    `json:"email"`
	FileName  string    `json:"file_name"`
	Tier      string    `json:"tier"`
	Signal    string    `json:"signal"`
	Source    string    `json:"source"`
	TrashedAt time.Time `json:"trashed_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Store struct {
	Version       string                 `json:"version"`
	Accounts      map[string]*Account    `json:"accounts"`
	Logs          []ActionLog            `json:"logs"`
	Trash         []TrashMeta            `json:"trash"`
	ErrorPolicies map[string]ErrorPolicy `json:"error_policies"`
	// HiddenPolicyKeys: user 降回未分类'd catalog keys — do not re-seed empty builtin cards.
	// New real hits of the same class still re-create a card via HandleUsage seed.
	HiddenPolicyKeys []string                  `json:"hidden_policy_keys,omitempty"`
	Observed         map[string]*ObservedError `json:"observed_errors"`
	Metrics          MetricsFloor              `json:"metrics"`
	mu               sync.Mutex
	path             string
}

// EscalationRule is one tier: when streak/count >= Streak, apply Action.
type EscalationRule struct {
	Streak      int    `json:"streak"` // threshold N
	Action      string `json:"action"` // observe|cooldown|candidate|disable|trash
	CooldownSec int    `json:"cooldown_seconds,omitempty"`
}

// ErrorPolicy is persisted per-error control (dynamic catalog).
type ErrorPolicy struct {
	Key   string `json:"key"`
	Label string `json:"label"` // 显示名称（用户可改；不再被硬编码覆盖）
	// DisplayMsg: 报错日志「错误信息」短文案；空则用 HumanMsg 默认。
	DisplayMsg string `json:"display_msg,omitempty"`
	// SplitShape is the legacy single route shape. SplitShapes is the full set.
	SplitShape  string   `json:"split_shape,omitempty"`
	SplitShapes []string `json:"split_shapes,omitempty"`
	Enabled     bool     `json:"enabled"`
	Action      string   `json:"action"` // legacy single action: observe|cooldown|candidate|trash|disable
	Threshold   int      `json:"threshold"`
	CooldownSec int      `json:"cooldown_seconds"`
	// CountMode: "streak" (default, success clears) | "total" (accumulate until reset)
	CountMode string `json:"count_mode,omitempty"`
	// Escalations optional multi-tier rules; if empty, Threshold+Action is used as one tier.
	Escalations []EscalationRule `json:"escalations,omitempty"`
	NeverTrash  bool             `json:"never_trash"`
	Note        string           `json:"note"`
	Source      string           `json:"source"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
}

// SplitShapeList returns all route shapes owned by this policy, preserving
// legacy split_shape while de-duplicating split_shapes.
func (p ErrorPolicy) SplitShapeList() []string {
	return normalizeSplitShapes(p.SplitShape, p.SplitShapes...)
}

func normalizeSplitShapes(primary string, shapes ...string) []string {
	out := make([]string, 0, 1)
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(primary)
	for _, v := range shapes {
		add(v)
	}
	return out
}

func setPolicySplitShapes(p *ErrorPolicy, shapes []string) {
	shapes = normalizeSplitShapes("", shapes...)
	p.SplitShapes = shapes
	if len(shapes) > 0 {
		p.SplitShape = shapes[0]
	} else {
		p.SplitShape = ""
	}
}

func mergePolicySplitShapes(p *ErrorPolicy, shapes ...string) {
	if p == nil {
		return
	}
	all := append(append([]string{}, p.SplitShapes...), shapes...)
	all = normalizeSplitShapes(p.SplitShape, all...)
	setPolicySplitShapes(p, all)
}

// NormalizedEscalations returns tiers sorted by streak ascending; synthesizes from legacy fields if needed.
func (p ErrorPolicy) NormalizedEscalations() []EscalationRule {
	if len(p.Escalations) > 0 {
		out := append([]EscalationRule(nil), p.Escalations...)
		// sort ascending
		for i := 0; i < len(out); i++ {
			for j := i + 1; j < len(out); j++ {
				if out[j].Streak < out[i].Streak {
					out[i], out[j] = out[j], out[i]
				}
			}
		}
		for i := range out {
			if out[i].Streak <= 0 {
				out[i].Streak = 1
			}
			if out[i].Action == "" {
				out[i].Action = "observe"
			}
		}
		return out
	}
	th := p.Threshold
	if th <= 0 {
		th = 1
	}
	act := p.Action
	if act == "" {
		act = "observe"
	}
	return []EscalationRule{{Streak: th, Action: act, CooldownSec: p.CooldownSec}}
}

type ObservedError struct {
	Key        string    `json:"key"`
	Label      string    `json:"label"`
	Shape      string    `json:"shape,omitempty"`
	Signal     string    `json:"signal"`
	Code       string    `json:"code"`
	StatusCode int       `json:"status_code"`
	Count      int64     `json:"count"`
	LastAt     time.Time `json:"last_at"`
	Sample     string    `json:"sample"`
	LastAuth   string    `json:"last_auth"`
	LastFile   string    `json:"last_file"`
	// Hits keeps recent per-account error events for policy log UI.
	Hits []ErrorHit `json:"hits,omitempty"`
}

// ErrorHit is one observed failure instance (for error-policy account log).
type ErrorHit struct {
	At     time.Time `json:"at"`
	Auth   string    `json:"auth"`
	File   string    `json:"file,omitempty"`
	Source string    `json:"source,omitempty"` // usage|patrol|tick|panel
	Status int       `json:"status,omitempty"`
	Shape  string    `json:"shape,omitempty"`
	Sample string    `json:"sample,omitempty"`
	Model  string    `json:"model,omitempty"` // request model when known
}

type MetricsFloor struct {
	DayKey           string `json:"day_key"`
	TokensFloor      int64  `json:"tokens_floor"`
	CallsFloor       int64  `json:"calls_floor"`
	BackfillSource   string `json:"backfill_source,omitempty"`
	BackfillAt       string `json:"backfill_at,omitempty"`
	LastCPAMPTokens  int64  `json:"last_cpamp_tokens,omitempty"`
	LastCPAMPCalls   int64  `json:"last_cpamp_calls,omitempty"`
	LastCPAMPSuccess int64  `json:"last_cpamp_success,omitempty"`
	LastCPAMPFailure int64  `json:"last_cpamp_failure,omitempty"`
	// LastCPAMPFailMS watermark for failover failure backfill (usage.sqlite timestamp_ms).
	LastCPAMPFailMS int64 `json:"last_cpamp_fail_ms,omitempty"`
}

const maxLogs = 2000
const maxErrorHits = 500
const errorHitRetention = 7 * 24 * time.Hour

func New(path string) *Store {
	return &Store{
		Version:       schemaVersion,
		Accounts:      map[string]*Account{},
		Logs:          nil,
		Trash:         nil,
		ErrorPolicies: map[string]ErrorPolicy{},
		Observed:      map[string]*ObservedError{},
		path:          path,
	}
}

func Load(path string) (*Store, error) {
	s := New(path)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	// v2 deliberately starts the error engine from a clean model. Operational
	// account state, usage, logs and trash survive; old classifier output and
	// policy routing do not.
	migrated := false
	if s.Version != schemaVersion {
		s.Version = schemaVersion
		s.ErrorPolicies = map[string]ErrorPolicy{}
		s.Observed = map[string]*ObservedError{}
		s.HiddenPolicyKeys = nil
		for _, a := range s.Accounts {
			if a == nil {
				continue
			}
			a.LastSignal = ""
			a.Streaks = map[string]int{}
		}
		migrated = true
	}
	if s.Accounts == nil {
		s.Accounts = map[string]*Account{}
	}
	if s.ErrorPolicies == nil {
		s.ErrorPolicies = map[string]ErrorPolicy{}
	}
	if s.Observed == nil {
		s.Observed = map[string]*ObservedError{}
	}
	s.path = path
	s.backfillLastActionsFromLogs()
	if migrated && path != "" {
		// persist clean schema immediately so restart does not re-migrate
		if err := s.Save(); err != nil {
			return s, err
		}
	}
	return s, nil
}

// backfillLastActionsFromLogs fills LastAction/LastActionAt from retained logs (newest wins).
func (s *Store) backfillLastActionsFromLogs() {
	if s == nil || len(s.Logs) == 0 || s.Accounts == nil {
		return
	}
	for _, e := range s.Logs {
		if e.Auth == "" || e.Action == "" {
			continue
		}
		s.stampLastActionLocked(e)
	}
}

func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) Touch(authIndex string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.Accounts[authIndex]
	if !ok {
		acc = &Account{
			AuthIndex: authIndex,
			State:     Active,
			Streaks:   map[string]int{},
		}
		s.Accounts[authIndex] = acc
	}
	if acc.Streaks == nil {
		acc.Streaks = map[string]int{}
	}
	acc.UpdatedAt = time.Now()
	return acc
}

// Get returns a snapshot copy of the account (safe for concurrent readers).
// Callers must not assume mutations on the result are persisted — use store setters.
func (s *Store) Get(authIndex string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return nil
	}
	return cloneAccount(acc)
}

func cloneAccount(acc *Account) *Account {
	if acc == nil {
		return nil
	}
	cp := *acc
	if acc.Streaks != nil {
		cp.Streaks = make(map[string]int, len(acc.Streaks))
		for k, v := range acc.Streaks {
			cp.Streaks[k] = v
		}
	}
	return &cp
}

func (s *Store) IncStreak(authIndex, signal string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, State: Active, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	if acc.Streaks == nil {
		acc.Streaks = map[string]int{}
	}
	acc.Streaks[signal]++
	// any_error is a global ladder counter only — never overwrite primary last_signal
	// (free_usage_429 / permission_403 / …) or cool rows show last_signal=any_error.
	if signal != "any_error" {
		acc.LastSignal = signal
	}
	acc.UpdatedAt = time.Now()
	return acc.Streaks[signal]
}

func (s *Store) ClearAuthStreaks(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return
	}
	// full streak reset so consecutive-N can restart cleanly after success/recovery
	acc.Streaks = map[string]int{}
	acc.UpdatedAt = time.Now()
}

// ClearAuthStreaksExcept clears all streaks except keys in keep (total/accumulate mode).
func (s *Store) ClearAuthStreaksExcept(authIndex string, keep map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil || acc.Streaks == nil {
		return
	}
	for k := range acc.Streaks {
		if keep[k] {
			continue
		}
		delete(acc.Streaks, k)
	}
	acc.UpdatedAt = time.Now()
}

// ClearCoolDownResidue clears half-recovered cool-down fields but keeps streaks.
func (s *Store) ClearCoolDownResidue(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return
	}
	acc.State = Active
	if acc.DisableSource == "plugin_auto" {
		acc.DisableSource = ""
	}
	acc.RecoverAt = time.Time{}
	// do not clear LastSignal/Streaks — needed for policy ladders on live accounts
	// half-recover residue also means "just came back" → pending observe until success
	if !acc.PendingObserve {
		acc.PendingSince = time.Now()
	}
	acc.PendingObserve = true
	acc.UpdatedAt = time.Now()
}

// ResetToActive recovers from cool-down to Active for traffic.
// Clears cool-down locks and recover timer.
// IMPORTANT: does NOT clear Streaks — policy ladders (e.g. 403 ≥3 cooldown, ≥15 disable)
// must survive cool-down cycles. Streaks clear only on successful requests (streak mode)
// or via ClearAuthStreaks / panel permanent re-enable paths that call ClearManualLock.
// Marks PendingObserve so UI shows「恢复待观察」until a real success proves clean.
// PendingSince is set only on the transition into pending (heal re-touch does not refresh TTL).
func (s *Store) ResetToActive(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex}
		s.Accounts[authIndex] = acc
	}
	acc.State = Active
	acc.DisableSource = ""
	acc.PreDisabled = false
	acc.RecoverAt = time.Time{}
	if !acc.PendingObserve {
		acc.PendingSince = time.Now()
	} else if acc.PendingSince.IsZero() {
		acc.PendingSince = time.Now()
	}
	acc.PendingObserve = true
	// keep LastSignal + Streaks for escalation ladder continuity
	if acc.Owner == "" {
		acc.Owner = Owner
	}
	acc.UpdatedAt = time.Now()
}

// ClearPendingObserve marks post-recover watch as done (successful request while Active).
func (s *Store) ClearPendingObserve(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return
	}
	if acc.PendingObserve || !acc.PendingSince.IsZero() || acc.HealFailStreak != 0 {
		acc.PendingObserve = false
		acc.PendingSince = time.Time{}
		acc.HealFailStreak = 0
		acc.UpdatedAt = time.Now()
	}
}

// SetPendingSince forces PendingSince (tests / ops repair). Does not toggle PendingObserve.
func (s *Store) SetPendingSince(authIndex string, t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return
	}
	acc.PendingSince = t
	acc.UpdatedAt = time.Now()
}

// ExpireIdlePending clears long-idle「恢复待观察」without wiping ladder streaks.
// Also drops free_usage/spending residual last_signal so UI can become 正常·可用.
// 403/401 streaks stay → UI shows 观察·403×N after pending is cleared.
func (s *Store) ExpireIdlePending(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return
	}
	acc.PendingObserve = false
	acc.PendingSince = time.Time{}
	switch acc.LastSignal {
	case "free_usage_429", "spending_limit_402":
		acc.LastSignal = ""
	}
	acc.UpdatedAt = time.Now()
}

// IncHealFailStreak increments sticky-heal counter; returns new value.
func (s *Store) IncHealFailStreak(authIndex string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	acc.HealFailStreak++
	acc.UpdatedAt = time.Now()
	return acc.HealFailStreak
}

// ClearHealFailStreak resets after a verified force-open.
func (s *Store) ClearHealFailStreak(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc := s.Accounts[authIndex]; acc != nil && acc.HealFailStreak != 0 {
		acc.HealFailStreak = 0
		acc.UpdatedAt = time.Now()
	}
}

// TouchLastHealAt stamps heal attempt time (rate-limit source of truth).
func (s *Store) TouchLastHealAt(authIndex string, at time.Time) {
	if s == nil || authIndex == "" {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	acc.LastHealAt = at
	acc.UpdatedAt = at
}

// MarkCPAFileDisabled marks sticky CPA已禁用 (not panel permanent ban).
// Used when heal cannot keep the file open (stuck residual).
func (s *Store) MarkCPAFileDisabled(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	acc.State = UserManual
	acc.DisableSource = "cpa_file_disabled"
	acc.PendingObserve = false
	acc.PendingSince = time.Time{}
	acc.RecoverAt = time.Time{}
	acc.UpdatedAt = time.Now()
}

func (s *Store) CanAutoReenable(authIndex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return false
	}
	// never auto-open permanent / operator locks
	if acc.State == UserManual || acc.DisableSource == "user_manual" ||
		acc.DisableSource == "cpa_file_disabled" || acc.DisableSource == "cpa_disabled" ||
		acc.PreDisabled {
		return false
	}
	// trash is not re-enabled via recover_at path
	if acc.State == Trashed || acc.State == Purged {
		return false
	}
	// sentry-owned cool-down / 候删: plugin_auto is sufficient (Owner may be empty on legacy rows)
	if acc.DisableSource == "plugin_auto" {
		return true
	}
	// legacy: cool-down state without source still treated as ours if recover_at set
	switch acc.State {
	case CooldownQuota, CooldownSpending, CooldownPermission, CandidateDead:
		return !acc.RecoverAt.IsZero()
	}
	return false
}

// ClearManualLock clears user_manual / pre_disabled after an explicit panel enable.
func (s *Store) ClearManualLock(authIndex string) {
	// panel 启用: full reset including streaks (operator override)
	s.ResetToActive(authIndex)
	s.ClearAuthStreaks(authIndex)
	s.mu.Lock()
	if acc := s.Accounts[authIndex]; acc != nil {
		acc.LastSignal = ""
		// operator full reset → clean normal, no「恢复待观察」
		acc.PendingObserve = false
		acc.PendingSince = time.Time{}
		acc.HealFailStreak = 0
	}
	s.mu.Unlock()
}

func (s *Store) Log(entry ActionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	s.Logs = append(s.Logs, entry)
	// stamp per-account last action for panel "动作时间" column
	s.stampLastActionLocked(entry)
	// keep at most 7 days and hard cap
	cut := time.Now().Add(-errorHitRetention)
	kept := s.Logs[:0]
	for _, e := range s.Logs {
		if e.At.After(cut) {
			kept = append(kept, e)
		}
	}
	s.Logs = kept
	if len(s.Logs) > maxLogs {
		// Prefer retaining business-critical actions over maintenance noise
		// (heal_summary / force-open / reassert spam) when the ring is full.
		s.Logs = trimLogsPreferCritical(s.Logs, maxLogs)
	}
}

// logActionPriority: higher = keep longer when ring is full.
func logActionPriority(action string) int {
	switch action {
	case "cooldown", "manual_disable", "candidate", "reenable", "reenable_failed",
		"reenable_file_still_closed", "cooldown_failed", "candidate_disable_failed",
		"trash", "manual_enable", "manual_enable_file_still_closed", "permanent_disable",
		"policy_permanent_disable",
		// register lifecycle must survive permanent-patrol observe spam
		"register", "patrol_alive_reopen":
		return 3
	case "cooldown_reassert", "cooldown_file_still_open", "heal_active_file",
		"heal_active_file_failed", "heal_active_file_stuck", "patrol_alive", "patrol_alive_open",
		"reopen_foreign", "maintenance", "observe":
		// observe demoted: permanent_skip_demote floods ring during permanent patrol
		return 2
	case "heal_summary", "auto_backfill", "scrub_active", "clear_cpa_disabled_tag":
		return 0
	default:
		return 1
	}
}

// trimLogsPreferCritical keeps up to maxN logs, preferring high-priority actions
// and newer entries. Order of remaining logs is preserved (time ascending).
func trimLogsPreferCritical(logs []ActionLog, maxN int) []ActionLog {
	if maxN <= 0 || len(logs) <= maxN {
		return logs
	}
	type scored struct {
		i, score int
	}
	ss := make([]scored, len(logs))
	for i, e := range logs {
		// priority dominates; among same priority prefer newer (higher index)
		ss[i] = scored{i: i, score: logActionPriority(e.Action)*100000 + i}
	}
	// selection sort desc by score (small N; maxLogs=2000 worst case ok for rare trim)
	for i := 0; i < len(ss); i++ {
		best := i
		for j := i + 1; j < len(ss); j++ {
			if ss[j].score > ss[best].score {
				best = j
			}
		}
		ss[i], ss[best] = ss[best], ss[i]
	}
	keep := make(map[int]bool, maxN)
	for k := 0; k < maxN && k < len(ss); k++ {
		keep[ss[k].i] = true
	}
	out := make([]ActionLog, 0, maxN)
	for i, e := range logs {
		if keep[i] {
			out = append(out, e)
		}
	}
	return out
}

func (s *Store) stampLastActionLocked(entry ActionLog) {
	if entry.Auth == "" || entry.Action == "" {
		return
	}
	// skip pure noise that is not an account action
	switch entry.Action {
	case "scrub_active", "clear_cpa_disabled_tag":
		// still useful as last touch — keep
	}
	acc := s.Accounts[entry.Auth]
	if acc == nil {
		// match by filename / email / basename
		key := strings.ToLower(strings.TrimSpace(entry.Auth))
		base := key
		if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
			base = base[i+1:]
		}
		for _, a := range s.Accounts {
			if a == nil {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(a.AuthIndex), entry.Auth) {
				acc = a
				break
			}
			fn := strings.ToLower(strings.TrimSpace(a.FileName))
			if fn != "" && (fn == key || fn == base || strings.HasSuffix(fn, base) || strings.HasSuffix(base, fn)) {
				acc = a
				break
			}
			if a.Email != "" && strings.EqualFold(a.Email, entry.Auth) {
				acc = a
				break
			}
		}
	}
	if acc == nil {
		return
	}
	acc.LastActionAt = entry.At
	acc.LastAction = entry.Action
	// keep UpdatedAt as general mutation time too
	acc.UpdatedAt = entry.At
}

// StampLastAction records last action without appending action logs (for batched heal).
func (s *Store) StampLastAction(authIndex, action string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stampLastActionLocked(ActionLog{At: time.Now(), Auth: authIndex, Action: action})
}

func (s *Store) AddTrash(meta TrashMeta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// replace same id
	out := s.Trash[:0]
	for _, t := range s.Trash {
		if t.ID != meta.ID {
			out = append(out, t)
		}
	}
	s.Trash = append(out, meta)
}

func (s *Store) RemoveTrash(id string) *TrashMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *TrashMeta
	out := s.Trash[:0]
	for _, t := range s.Trash {
		if t.ID == id {
			cp := t
			found = &cp
			continue
		}
		out = append(out, t)
	}
	s.Trash = out
	return found
}

func (s *Store) ListTrash() []TrashMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TrashMeta, len(s.Trash))
	copy(out, s.Trash)
	return out
}

func (s *Store) SetAccountState(authIndex string, st AccountState, disableSource string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	acc.State = st
	if disableSource != "" {
		acc.DisableSource = disableSource
		acc.Owner = Owner
	}
	// re-enter cool/候删/禁用/垃圾箱: leave the post-recover watch phase
	switch st {
	case CooldownQuota, CooldownSpending, CooldownPermission, CandidateDead, UserManual, Trashed, Purged:
		acc.PendingObserve = false
		acc.PendingSince = time.Time{}
	}
	acc.UpdatedAt = time.Now()
}

func (s *Store) SetRecoverAt(authIndex string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if acc := s.Accounts[authIndex]; acc != nil {
		acc.RecoverAt = at
		acc.UpdatedAt = time.Now()
	}
}

// SetLastSignal stamps last_signal without bumping streaks (for maintenance sync).
func (s *Store) DeleteAccount(authIndex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Accounts, authIndex)
}

func (s *Store) SetLastSignal(authIndex, signal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, State: Active, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	// empty string clears residual signal (closed-loop recovery / success)
	acc.LastSignal = signal
	// do not bump UpdatedAt — maintenance sync / success cleanup is not a request event
}

func (s *Store) AccountsSnapshot() []*Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Account, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		out = append(out, cloneAccount(a))
	}
	return out
}

func (s *Store) UpdateMeta(authIndex, fileName, email, tierName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, State: Active, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	// Prefer human-readable filename/email over opaque hash placeholders.
	changed := false
	if fileName != "" {
		if acc.FileName == "" || isOpaqueMeta(acc.FileName) || (!isOpaqueMeta(fileName) && acc.FileName != fileName && strings.Contains(fileName, "@")) {
			if acc.FileName != fileName {
				acc.FileName = fileName
				changed = true
			}
		} else if acc.FileName == authIndex && fileName != authIndex {
			acc.FileName = fileName
			changed = true
		}
	}
	if email != "" && acc.Email != email {
		acc.Email = email
		changed = true
	}
	if tierName != "" && (acc.Tier == "" || acc.Tier == "unknown") {
		acc.Tier = tierName
		changed = true
	}
	// Do NOT bump UpdatedAt here — metadata refresh is not a real request event.
	_ = changed
}

func isOpaqueMeta(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, "@") || strings.HasSuffix(strings.ToLower(s), ".json") || strings.HasPrefix(strings.ToLower(s), "xai-") {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == '-') {
			return false
		}
	}
	return len(s) >= 12
}

func (s *Store) SnapshotLogs() []ActionLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionLog, len(s.Logs))
	copy(out, s.Logs)
	return out
}

// SnapshotLogsPage returns logs newest-first with offset/limit.
// offset=0 is the newest page. total is the full log count before paging.
func (s *Store) SnapshotLogsPage(offset, limit int) (page []ActionLog, total int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total = len(s.Logs)
	if total == 0 {
		return nil, 0
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	// Logs stored oldest→newest; serve newest first
	// newest index = total-1-offset
	startNewest := total - 1 - offset
	if startNewest < 0 {
		return nil, total
	}
	endNewest := startNewest - limit + 1
	if endNewest < 0 {
		endNewest = 0
	}
	// collect from startNewest down to endNewest inclusive
	n := startNewest - endNewest + 1
	page = make([]ActionLog, 0, n)
	for i := startNewest; i >= endNewest; i-- {
		page = append(page, s.Logs[i])
	}
	return page, total
}

func (s *Store) ObserveError(key, label, signal, code, sample, auth, file, source string, status int, model ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Observed == nil {
		s.Observed = map[string]*ObservedError{}
	}
	o := s.Observed[key]
	if o == nil {
		o = &ObservedError{Key: key, Label: label}
		s.Observed[key] = o
	}
	if label != "" {
		o.Label = label
	}
	shape := ""
	modelVals := model
	if len(modelVals) > 0 {
		last := strings.TrimSpace(modelVals[len(modelVals)-1])
		if strings.HasPrefix(last, "fp_") || last == "free_usage_429" || last == "permission_403" {
			shape = last
			modelVals = modelVals[:len(modelVals)-1]
		}
	}
	if shape == "" && (key == "unmatched" || strings.HasPrefix(key, "reason:fp_")) {
		shape, _, _ = shapeOfLocked(sample, status)
	}
	if shape != "" {
		o.Shape = shape
	}
	o.Signal = signal
	o.Code = code
	o.StatusCode = status
	o.Count++
	o.LastAt = time.Now()
	if sample != "" {
		sample = html.UnescapeString(sample)
		sample = strings.TrimSpace(sample)
		if len(sample) > 1200 {
			sample = sample[:1200]
		}
		o.Sample = sample
	}
	o.LastAuth = auth
	o.LastFile = file
	hitSample := sample
	if len(hitSample) > 240 {
		hitSample = hitSample[:240]
	}
	mod := ""
	if len(modelVals) > 0 {
		mod = strings.TrimSpace(modelVals[0])
	}
	if len(mod) > 80 {
		mod = mod[:80]
	}
	o.Hits = append(o.Hits, ErrorHit{
		At: o.LastAt, Auth: auth, File: file, Source: source, Status: status, Shape: shape, Sample: hitSample, Model: mod,
	})
	// retain max 7 days + hard cap
	cut := time.Now().Add(-errorHitRetention)
	kept := o.Hits[:0]
	for _, h := range o.Hits {
		if h.At.After(cut) {
			kept = append(kept, h)
		}
	}
	o.Hits = kept
	if len(o.Hits) > maxErrorHits {
		o.Hits = o.Hits[len(o.Hits)-maxErrorHits:]
	}
}

// ReclassifyErrorKey moves an observed error key (and optional policy) to a new key.
// Special case to=unmatched: also works when there is no observed ring (empty builtin card)
// and marks the key hidden so EnsureBuiltinPolicies will not re-seed it until new hits arrive.
func (s *Store) ReclassifyErrorKey(from, to, newLabel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return fmt.Errorf("invalid keys")
	}
	if from == "any_error" {
		return fmt.Errorf("cannot unclassify any_error")
	}
	if s.Observed == nil {
		s.Observed = map[string]*ObservedError{}
	}
	src := s.Observed[from]
	// Empty builtin card: no observed hits — just drop policy + hide reseeding
	if src == nil {
		if to != "unmatched" {
			return fmt.Errorf("source key not found")
		}
		if s.ErrorPolicies != nil {
			delete(s.ErrorPolicies, from)
		}
		s.hidePolicyLocked(from)
		return nil
	}
	dst := s.Observed[to]
	if dst == nil {
		dst = &ObservedError{Key: to, Label: newLabel}
		s.Observed[to] = dst
	}
	if newLabel != "" {
		dst.Label = newLabel
	}
	if dst.Label == "" {
		dst.Label = src.Label
	}
	dst.Count += src.Count
	if src.LastAt.After(dst.LastAt) {
		dst.LastAt = src.LastAt
		dst.Sample = src.Sample
		dst.LastAuth = src.LastAuth
		dst.LastFile = src.LastFile
		dst.StatusCode = src.StatusCode
		dst.Code = src.Code
		if src.Signal != "" {
			dst.Signal = src.Signal
		}
	}
	dst.Hits = append(dst.Hits, src.Hits...)
	if len(dst.Hits) > maxErrorHits {
		dst.Hits = dst.Hits[len(dst.Hits)-maxErrorHits:]
	}
	delete(s.Observed, from)
	// move/drop policy; always carry source split routes so future fingerprints
	// continue to route to the merged card.
	if s.ErrorPolicies != nil {
		if p, ok := s.ErrorPolicies[from]; ok {
			delete(s.ErrorPolicies, from)
			if to != "unmatched" {
				if dest, exists := s.ErrorPolicies[to]; exists {
					mergePolicySplitShapes(&dest, p.SplitShapeList()...)
					if strings.TrimSpace(dest.DisplayMsg) == "" && strings.TrimSpace(p.DisplayMsg) != "" {
						dest.DisplayMsg = p.DisplayMsg
					}
					if newLabel != "" {
						dest.Label = newLabel
					}
					s.ErrorPolicies[to] = dest
				} else {
					p.Key = to
					setPolicySplitShapes(&p, p.SplitShapeList())
					if newLabel != "" {
						p.Label = newLabel
					}
					s.ErrorPolicies[to] = p
				}
			}
		}
	}
	if to == "unmatched" {
		s.hidePolicyLocked(from)
	}
	return nil
}

func (s *Store) hidePolicyLocked(key string) {
	key = strings.TrimSpace(key)
	if key == "" || key == "unmatched" || key == "any_error" {
		return
	}
	for _, k := range s.HiddenPolicyKeys {
		if k == key {
			return
		}
	}
	s.HiddenPolicyKeys = append(s.HiddenPolicyKeys, key)
}

func (s *Store) isPolicyHiddenLocked(key string) bool {
	for _, k := range s.HiddenPolicyKeys {
		if k == key {
			return true
		}
	}
	return false
}

// IsPolicyHidden returns true if the user explicitly deleted (recycled) this policy.
// Hidden policies should not be re-seeded by runtime or EnsureBuiltinPolicies.
func (s *Store) IsPolicyHidden(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.isPolicyHiddenLocked(key)
}

// UnhidePolicy clears a hidden mark (e.g. when user re-enables a class manually).
func (s *Store) UnhidePolicy(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key = strings.TrimSpace(key)
	out := s.HiddenPolicyKeys[:0]
	for _, k := range s.HiddenPolicyKeys {
		if k != key {
			out = append(out, k)
		}
	}
	s.HiddenPolicyKeys = out
}

// SplitObservedByShape moves hits matching shape from unmatched (or from) into a new/existing key.
// Returns moved hit count.
func (s *Store) SplitObservedByShape(from, to, newLabel, shape string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	shape = strings.TrimSpace(shape)
	if from == "" {
		from = "unmatched"
	}
	if to == "" || shape == "" {
		return 0, fmt.Errorf("to/shape required")
	}
	if s.Observed == nil {
		return 0, fmt.Errorf("no observed errors")
	}
	src := s.Observed[from]
	if src == nil {
		return 0, fmt.Errorf("source key not found")
	}
	// partition hits
	keep := src.Hits[:0]
	moved := make([]ErrorHit, 0)
	for _, h := range src.Hits {
		sh := strings.TrimSpace(h.Shape)
		if sh == "" {
			sh, _, _ = shapeOfLocked(h.Sample, h.Status)
		}
		// also allow match by human message equality as shape key msg:...
		if sh == shape || strings.Contains(strings.ToLower(h.Sample), strings.ToLower(strings.TrimPrefix(shape, "msg:"))) {
			moved = append(moved, h)
		} else {
			keep = append(keep, h)
		}
	}
	// if no hits ring, fall back to whole key move when sample matches
	if len(src.Hits) == 0 {
		sh, _, _ := shapeOfLocked(src.Sample, src.StatusCode)
		if sh != shape {
			return 0, fmt.Errorf("no matching hits for this error shape")
		}
		// whole-key move under same lock
		dst := s.Observed[to]
		if dst == nil {
			dst = &ObservedError{Key: to, Label: newLabel}
			s.Observed[to] = dst
		}
		if newLabel != "" {
			dst.Label = newLabel
		}
		n := int(src.Count)
		dst.Count += src.Count
		dst.Hits = append(dst.Hits, src.Hits...)
		if src.LastAt.After(dst.LastAt) {
			dst.LastAt, dst.Sample, dst.LastAuth, dst.LastFile, dst.StatusCode, dst.Shape = src.LastAt, src.Sample, src.LastAuth, src.LastFile, src.StatusCode, src.Shape
		}
		delete(s.Observed, from)
		if s.ErrorPolicies != nil {
			if p, ok := s.ErrorPolicies[from]; ok {
				p.Key = to
				if newLabel != "" {
					p.Label = newLabel
				}
				if dest, exists := s.ErrorPolicies[to]; exists {
					mergePolicySplitShapes(&dest, p.SplitShapeList()...)
					if strings.TrimSpace(dest.DisplayMsg) == "" && strings.TrimSpace(p.DisplayMsg) != "" {
						dest.DisplayMsg = p.DisplayMsg
					}
					s.ErrorPolicies[to] = dest
				} else {
					mergePolicySplitShapes(&p, shape)
					s.ErrorPolicies[to] = p
				}
				delete(s.ErrorPolicies, from)
			}
		}
		if s.ErrorPolicies == nil {
			s.ErrorPolicies = map[string]ErrorPolicy{}
		}
		if _, ok := s.ErrorPolicies[to]; !ok {
			s.ErrorPolicies[to] = ErrorPolicy{Key: to, Label: newLabel, Enabled: true, Action: "observe", Threshold: 1, Source: "split", Note: "从错误形态拆分"}
		}
		p := s.ErrorPolicies[to]
		mergePolicySplitShapes(&p, shape)
		s.ErrorPolicies[to] = p
		return n, nil
	}
	if len(moved) == 0 {
		return 0, fmt.Errorf("no matching hits for this error shape")
	}
	dst := s.Observed[to]
	if dst == nil {
		dst = &ObservedError{Key: to, Label: newLabel}
		s.Observed[to] = dst
	}
	if newLabel != "" {
		dst.Label = newLabel
	}
	if dst.Label == "" {
		dst.Label = newLabel
	}
	dst.Hits = append(dst.Hits, moved...)
	if len(dst.Hits) > maxErrorHits {
		dst.Hits = dst.Hits[len(dst.Hits)-maxErrorHits:]
	}
	// recount
	dst.Count += int64(len(moved))
	// update last from moved
	last := moved[len(moved)-1]
	if last.At.After(dst.LastAt) {
		dst.LastAt = last.At
		dst.Sample = last.Sample
		dst.LastAuth = last.Auth
		dst.LastFile = last.File
		dst.StatusCode = last.Status
		dst.Shape = last.Shape
	}
	src.Hits = keep
	src.Count -= int64(len(moved))
	if src.Count < 0 {
		src.Count = int64(len(keep))
	}
	if len(keep) == 0 {
		// keep key with zero? delete empty unmatched clutter only if count<=0
		if src.Count <= 0 {
			delete(s.Observed, from)
		} else {
			src.Sample = ""
			src.LastAuth = ""
		}
	} else {
		// refresh last from keep
		lastK := keep[len(keep)-1]
		src.LastAt = lastK.At
		src.Sample = lastK.Sample
		src.LastAuth = lastK.Auth
		src.LastFile = lastK.File
		src.StatusCode = lastK.Status
		src.Shape = lastK.Shape
	}
	// ensure policy for target
	if s.ErrorPolicies == nil {
		s.ErrorPolicies = map[string]ErrorPolicy{}
	}
	if _, ok := s.ErrorPolicies[to]; !ok {
		s.ErrorPolicies[to] = ErrorPolicy{
			Key: to, Label: newLabel, Enabled: true, Action: "observe",
			Threshold: 1, Source: "split", Note: "从错误形态拆分",
		}
	}
	p := s.ErrorPolicies[to]
	mergePolicySplitShapes(&p, shape)
	s.ErrorPolicies[to] = p
	return len(moved), nil
}

func shapeOfLocked(sample string, status int) (shape, label, key string) {
	fp := errorfp.Build(sample, status)
	return fp.Shape, fp.Message, fp.SuggestKey
}

func (s *Store) ListObserved() []ObservedError {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ObservedError, 0, len(s.Observed))
	for _, o := range s.Observed {
		if o != nil {
			out = append(out, *o)
		}
	}
	return out
}

func (s *Store) EnsureBuiltinPolicies(builtins map[string]ErrorPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ErrorPolicies == nil {
		s.ErrorPolicies = map[string]ErrorPolicy{}
	}
	for k, p := range builtins {
		if s.isPolicyHiddenLocked(k) {
			// user 降回未分类 — do not resurrect empty builtin card
			continue
		}
		if _, ok := s.ErrorPolicies[k]; !ok {
			s.ErrorPolicies[k] = p
		}
	}
}

func (s *Store) GetErrorPolicy(key string) (ErrorPolicy, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.ErrorPolicies[key]
	return p, ok
}

func (s *Store) UpsertErrorPolicy(p ErrorPolicy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ErrorPolicies == nil {
		s.ErrorPolicies = map[string]ErrorPolicy{}
	}
	// saving a policy explicitly un-hides it
	if p.Key != "" {
		out := s.HiddenPolicyKeys[:0]
		for _, k := range s.HiddenPolicyKeys {
			if k != p.Key {
				out = append(out, k)
			}
		}
		s.HiddenPolicyKeys = out
	}
	// Preserve and merge split routes if panel save omitted them or sent only one
	// shape. User split/merge can intentionally append routes, but saving a card
	// must not drop existing routing fingerprints.
	if old, ok := s.ErrorPolicies[p.Key]; ok {
		oldShapes := old.SplitShapeList()
		inShapes := p.SplitShapeList()
		allShapes := append(append([]string{}, oldShapes...), inShapes...)
		setPolicySplitShapes(&p, allShapes)
		if strings.TrimSpace(p.DisplayMsg) == "" && strings.TrimSpace(old.DisplayMsg) != "" {
			p.DisplayMsg = old.DisplayMsg
		}
		if strings.TrimSpace(p.Note) == "" && strings.TrimSpace(old.Note) != "" {
			p.Note = old.Note
		}
	}
	// derive split_shape only for fingerprint keys (reason:fp_*)
	if strings.HasPrefix(p.Key, "reason:fp_") {
		mergePolicySplitShapes(&p, strings.TrimPrefix(p.Key, "reason:"))
	} else {
		setPolicySplitShapes(&p, p.SplitShapeList())
	}
	p.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.ErrorPolicies[p.Key] = p
}

func (s *Store) ListErrorPolicies() []ErrorPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ErrorPolicy, 0, len(s.ErrorPolicies))
	for _, p := range s.ErrorPolicies {
		out = append(out, p)
	}
	return out
}

func (s *Store) ApplyCPAMPBackfill(dayKey string, tokens, calls, success, failure int64, source string) MetricsFloor {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Metrics.DayKey != dayKey {
		s.Metrics = MetricsFloor{DayKey: dayKey}
	}
	if tokens > s.Metrics.TokensFloor {
		s.Metrics.TokensFloor = tokens
	}
	if calls > s.Metrics.CallsFloor {
		s.Metrics.CallsFloor = calls
	}
	s.Metrics.BackfillSource = source
	s.Metrics.BackfillAt = time.Now().UTC().Format(time.RFC3339)
	s.Metrics.LastCPAMPTokens = tokens
	s.Metrics.LastCPAMPCalls = calls
	s.Metrics.LastCPAMPSuccess = success
	s.Metrics.LastCPAMPFailure = failure
	return s.Metrics
}

func (s *Store) MetricsSnapshot() MetricsFloor {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Metrics
}

// SetLastCPAMPFailMS advances the failover backfill watermark (only forward).
func (s *Store) SetLastCPAMPFailMS(ms int64) {
	if s == nil || ms <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms > s.Metrics.LastCPAMPFailMS {
		s.Metrics.LastCPAMPFailMS = ms
	}
}

// UpdateQuota writes best-effort per-account quota numbers.
// touchUpdated=false keeps UpdatedAt untouched (for background/display rehydrate).
func (s *Store) UpdateQuota(authIndex string, limit, used, remaining int64, source string, resetAt time.Time) {
	s.updateQuota(authIndex, limit, used, remaining, source, resetAt, true)
}

// UpdateQuotaQuiet updates quota fields without treating it as a request event.
func (s *Store) UpdateQuotaQuiet(authIndex string, limit, used, remaining int64, source string, resetAt time.Time) {
	s.updateQuota(authIndex, limit, used, remaining, source, resetAt, false)
}

func (s *Store) updateQuota(authIndex string, limit, used, remaining int64, source string, resetAt time.Time, touchUpdated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, State: Active, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	if limit > 0 {
		acc.QuotaLimit = limit
	}
	if used >= 0 {
		acc.QuotaUsed = used
	}
	if remaining >= 0 {
		acc.QuotaRemaining = remaining
	}
	if source != "" {
		acc.QuotaSource = source
	}
	acc.QuotaUpdatedAt = time.Now()
	if !resetAt.IsZero() && (acc.RecoverAt.IsZero() || resetAt.Before(acc.RecoverAt)) {
		// only hint; cooldown path owns RecoverAt when auto-cooling
		if acc.State == Active {
			acc.RecoverAt = resetAt
		}
	}
	if touchUpdated {
		acc.UpdatedAt = time.Now()
	}
}

// IncDayUsage bumps local day counters for an account (Shanghai day key expected).
func (s *Store) IncDayUsage(authIndex, dayKey string, calls, failCalls, tokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		acc = &Account{AuthIndex: authIndex, State: Active, Streaks: map[string]int{}}
		s.Accounts[authIndex] = acc
	}
	if acc.DayKey != dayKey {
		acc.DayKey = dayKey
		acc.DayCalls = 0
		acc.DayFailCalls = 0
		acc.DayTokens = 0
	}
	acc.DayCalls += calls
	acc.DayFailCalls += failCalls
	acc.DayTokens += tokens
	acc.UpdatedAt = time.Now()
}

// CooldownStats derives cooldown inventory for KPI/capacity view.
func (s *Store) CooldownStats(now time.Time) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cooling, pendingSuggest int
	var earliest, latest time.Time
	var knownLimit, knownUsed, knownRemain int64
	var withQuota int
	for _, a := range s.Accounts {
		if a == nil {
			continue
		}
		st := string(a.State)
		if strings.HasPrefix(st, "cooldown") {
			cooling++
			if !a.RecoverAt.IsZero() {
				if earliest.IsZero() || a.RecoverAt.Before(earliest) {
					earliest = a.RecoverAt
				}
				if latest.IsZero() || a.RecoverAt.After(latest) {
					latest = a.RecoverAt
				}
			}
		}
		if a.State == Active && a.LastSignal == "free_usage_429" {
			pendingSuggest++
		}
		if a.QuotaLimit > 0 || a.QuotaRemaining > 0 || a.QuotaUsed > 0 {
			withQuota++
			knownLimit += a.QuotaLimit
			knownUsed += a.QuotaUsed
			knownRemain += a.QuotaRemaining
		}
	}
	out := map[string]any{
		"cooling":          cooling,
		"pending_suggest":  pendingSuggest,
		"with_quota":       withQuota,
		"quota_limit_sum":  knownLimit,
		"quota_used_sum":   knownUsed,
		"quota_remain_sum": knownRemain,
	}
	if !earliest.IsZero() {
		out["earliest_recover_at"] = earliest.UTC().Format(time.RFC3339)
		out["earliest_recover_in_sec"] = int(earliest.Sub(now).Seconds())
	}
	if !latest.IsZero() {
		out["latest_recover_at"] = latest.UTC().Format(time.RFC3339)
	}
	return out
}
