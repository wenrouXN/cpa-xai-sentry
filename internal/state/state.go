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
)

type AccountState string

const (
	Active             AccountState = "active"
	CooldownQuota      AccountState = "cooldown_quota"
	CooldownSpending   AccountState = "cooldown_spending"
	CooldownPermission AccountState = "cooldown_permission"
	CandidateDead      AccountState = "candidate_dead"
	UserManual         AccountState = "user_manual"
	Trashed            AccountState = "trashed"
	Purged             AccountState = "purged"
)

const Owner = "cpa_xai_sentry"

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
	Version       string                    `json:"version"`
	Accounts      map[string]*Account       `json:"accounts"`
	Logs          []ActionLog               `json:"logs"`
	Trash         []TrashMeta               `json:"trash"`
	ErrorPolicies map[string]ErrorPolicy    `json:"error_policies"`
	Observed      map[string]*ObservedError `json:"observed_errors"`
	Metrics       MetricsFloor              `json:"metrics"`
	mu            sync.Mutex
	path          string
}

// EscalationRule is one tier: when streak/count >= Streak, apply Action.
type EscalationRule struct {
	Streak     int    `json:"streak"`               // threshold N
	Action     string `json:"action"`               // observe|cooldown|candidate|disable|trash
	CooldownSec int    `json:"cooldown_seconds,omitempty"`
}

// ErrorPolicy is persisted per-error control (dynamic catalog).
type ErrorPolicy struct {
	Key         string           `json:"key"`
	Label       string           `json:"label"`
	Enabled     bool             `json:"enabled"`
	Action      string           `json:"action"` // legacy single action: observe|cooldown|candidate|trash|disable
	Threshold   int              `json:"threshold"`
	CooldownSec int              `json:"cooldown_seconds"`
	// CountMode: "streak" (default, success clears) | "total" (accumulate until reset)
	CountMode   string           `json:"count_mode,omitempty"`
	// Escalations optional multi-tier rules; if empty, Threshold+Action is used as one tier.
	Escalations []EscalationRule `json:"escalations,omitempty"`
	NeverTrash  bool             `json:"never_trash"`
	Note        string           `json:"note"`
	Source      string           `json:"source"`
	UpdatedAt   string           `json:"updated_at,omitempty"`
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
	Sample string    `json:"sample,omitempty"`
}

type MetricsFloor struct {
	DayKey              string `json:"day_key"`
	TokensFloor         int64  `json:"tokens_floor"`
	CallsFloor          int64  `json:"calls_floor"`
	BackfillSource      string `json:"backfill_source,omitempty"`
	BackfillAt          string `json:"backfill_at,omitempty"`
	LastCPAMPTokens     int64  `json:"last_cpamp_tokens,omitempty"`
	LastCPAMPCalls      int64  `json:"last_cpamp_calls,omitempty"`
	LastCPAMPSuccess    int64  `json:"last_cpamp_success,omitempty"`
	LastCPAMPFailure    int64  `json:"last_cpamp_failure,omitempty"`
}

const maxLogs = 2000
const maxErrorHits = 500
const errorHitRetention = 7 * 24 * time.Hour

func New(path string) *Store {
	return &Store{
		Version:       "1",
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
	return s, nil
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

func (s *Store) Get(authIndex string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Accounts[authIndex]
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
	acc.LastSignal = signal
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
	acc.UpdatedAt = time.Now()
}

// ResetToActive is the closed-loop recovery: back to clean normal state.
// Clears cool-down locks, recover timer, last error signal and streaks.
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
	acc.LastSignal = ""
	acc.Streaks = map[string]int{}
	// keep Owner for audit; empty is fine too
	if acc.Owner == "" {
		acc.Owner = Owner
	}
	acc.UpdatedAt = time.Now()
}

func (s *Store) CanAutoReenable(authIndex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc := s.Accounts[authIndex]
	if acc == nil {
		return false
	}
	if acc.DisableSource == "user_manual" || acc.State == UserManual {
		return false
	}
	if acc.PreDisabled {
		return false
	}
	return acc.Owner == Owner && acc.DisableSource == "plugin_auto"
}

// ClearManualLock clears user_manual / pre_disabled after an explicit panel enable.
func (s *Store) ClearManualLock(authIndex string) {
	// full closed-loop reset (same as recover)
	s.ResetToActive(authIndex)
}

func (s *Store) Log(entry ActionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	s.Logs = append(s.Logs, entry)
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
		s.Logs = s.Logs[len(s.Logs)-maxLogs:]
	}
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
		cp := *a
		out = append(out, &cp)
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

func (s *Store) ObserveError(key, label, signal, code, sample, auth, file, source string, status int) {
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
	o.Hits = append(o.Hits, ErrorHit{
		At: o.LastAt, Auth: auth, File: file, Source: source, Status: status, Sample: hitSample,
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
func (s *Store) ReclassifyErrorKey(from, to, newLabel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" || from == to {
		return fmt.Errorf("invalid keys")
	}
	if s.Observed == nil {
		s.Observed = map[string]*ObservedError{}
	}
	src := s.Observed[from]
	if src == nil {
		return fmt.Errorf("source key not found")
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
	// move policy if present
	if s.ErrorPolicies != nil {
		if p, ok := s.ErrorPolicies[from]; ok {
			p.Key = to
			if newLabel != "" {
				p.Label = newLabel
			}
			// don't overwrite builtin target unless missing
			if _, exists := s.ErrorPolicies[to]; !exists {
				s.ErrorPolicies[to] = p
			}
			delete(s.ErrorPolicies, from)
		}
	}
	return nil
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
		sh, _, _ := shapeOfLocked(h.Sample, h.Status)
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
			dst.LastAt, dst.Sample, dst.LastAuth, dst.LastFile, dst.StatusCode = src.LastAt, src.Sample, src.LastAuth, src.LastFile, src.StatusCode
		}
		delete(s.Observed, from)
		if s.ErrorPolicies != nil {
			if p, ok := s.ErrorPolicies[from]; ok {
				p.Key = to
				if newLabel != "" { p.Label = newLabel }
				if _, exists := s.ErrorPolicies[to]; !exists { s.ErrorPolicies[to] = p }
				delete(s.ErrorPolicies, from)
			}
		}
		if s.ErrorPolicies == nil { s.ErrorPolicies = map[string]ErrorPolicy{} }
		if _, ok := s.ErrorPolicies[to]; !ok {
			s.ErrorPolicies[to] = ErrorPolicy{Key: to, Label: newLabel, Enabled: true, Action: "observe", Threshold: 1, Source: "split", Note: "从错误形态拆分"}
		}
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
	return len(moved), nil
}

// shape helper without importing errorsig (avoid cycle) — duplicate minimal logic
func shapeOfLocked(sample string, status int) (shape, label, key string) {
	low := strings.ToLower(sample)
	switch {
	case strings.Contains(low, "eof"):
		return "net_eof", "连接中断", "reason:net_eof"
	case strings.Contains(low, "timeout"):
		return "net_timeout", "请求超时", "reason:net_timeout"
	case strings.Contains(low, "cpa") && strings.Contains(low, "disabled"):
		return "cpa_disabled", "CPA文件已禁用", "reason:cpa_disabled"
	case status == 401 || strings.Contains(low, "authentication required"):
		return "auth_401", "401·凭证失效", "auth_401"
	case status == 403 || strings.Contains(low, "permission-denied"):
		return "permission_403", "403·权限拒绝", "permission_403"
	case status == 404 || strings.Contains(low, "404"):
		return "http_404", "404·路径/网关", "http_404"
	case status > 0:
		return fmt.Sprintf("http_%d", status), fmt.Sprintf("HTTP %d", status), fmt.Sprintf("http_%d", status)
	default:
		fp := sample
		if len(fp) > 40 {
			fp = fp[:40]
		}
		fp = strings.ToLower(strings.TrimSpace(fp))
		fp = strings.ReplaceAll(fp, " ", "_")
		return "msg:" + fp, "未分类片段", "reason:msg"
	}
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
		"cooling":         cooling,
		"pending_suggest": pendingSuggest,
		"with_quota":      withQuota,
		"quota_limit_sum": knownLimit,
		"quota_used_sum":  knownUsed,
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
