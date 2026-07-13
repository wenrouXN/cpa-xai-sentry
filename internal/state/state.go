package state

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	Version  string              `json:"version"`
	Accounts map[string]*Account `json:"accounts"`
	Logs     []ActionLog         `json:"logs"`
	Trash    []TrashMeta         `json:"trash"`
	mu       sync.Mutex
	path     string
}

const maxLogs = 1000

func New(path string) *Store {
	return &Store{
		Version:  "1",
		Accounts: map[string]*Account{},
		Logs:     nil,
		Trash:    nil,
		path:     path,
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
	if acc == nil || acc.Streaks == nil {
		return
	}
	delete(acc.Streaks, "auth_401")
	delete(acc.Streaks, "permission_403")
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

func (s *Store) Log(entry ActionLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry.At.IsZero() {
		entry.At = time.Now()
	}
	s.Logs = append(s.Logs, entry)
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
	if fileName != "" && acc.FileName == "" {
		acc.FileName = fileName
	}
	if email != "" && acc.Email == "" {
		acc.Email = email
	}
	if tierName != "" && acc.Tier == "" {
		acc.Tier = tierName
	}
	acc.UpdatedAt = time.Now()
}

func (s *Store) SnapshotLogs() []ActionLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ActionLog, len(s.Logs))
	copy(out, s.Logs)
	return out
}
