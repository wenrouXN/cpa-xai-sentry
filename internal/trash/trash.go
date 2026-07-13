package trash

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openclaw-local/cpa-xai-sentry/internal/state"
)

type Store struct {
	Dir           string
	RetentionDays int
	AutoPurge     bool
	State         *state.Store
}

func New(dir string, retentionDays int, autoPurge bool, st *state.Store) *Store {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	return &Store{Dir: dir, RetentionDays: retentionDays, AutoPurge: autoPurge, State: st}
}

func NewID(authIndex string, at time.Time) string {
	h := sha256.Sum256([]byte(authIndex + "|" + at.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(h[:])[:16]
}

func (s *Store) snapshotPath(id string) string {
	return filepath.Join(s.Dir, id+".json")
}

// MoveToTrash writes snapshot then calls deleteLive. If snapshot fails, deleteLive is never called.
func (s *Store) MoveToTrash(meta state.TrashMeta, authJSON []byte, deleteLive func() error) error {
	if len(authJSON) == 0 {
		return errors.New("empty snapshot")
	}
	if meta.ID == "" {
		return errors.New("missing trash id")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	path := s.snapshotPath(meta.ID)
	if err := os.WriteFile(path, authJSON, 0o600); err != nil {
		return err
	}
	if meta.TrashedAt.IsZero() {
		meta.TrashedAt = time.Now()
	}
	if meta.ExpiresAt.IsZero() {
		meta.ExpiresAt = meta.TrashedAt.Add(time.Duration(s.RetentionDays) * 24 * time.Hour)
	}
	s.State.AddTrash(meta)
	s.State.SetAccountState(meta.AuthIndex, state.Trashed, "plugin_auto")
	if err := deleteLive(); err != nil {
		// keep snapshot for manual recovery
		s.State.Log(state.ActionLog{
			Auth: meta.AuthIndex, Action: "trash_delete_failed", Signal: meta.Signal,
			Source: meta.Source, Reason: err.Error(),
		})
		_ = s.State.Save()
		return fmt.Errorf("snapshot ok but live delete failed: %w", err)
	}
	s.State.Log(state.ActionLog{
		Auth: meta.AuthIndex, Action: "trash", Signal: meta.Signal, Source: meta.Source, Reason: "moved",
	})
	return s.State.Save()
}

// Restore loads snapshot, rewrites live auth, removes trash entry.
// If enable is false (or restore_default_disabled path), sets disabled=true on JSON.
func (s *Store) Restore(id string, enable bool, writeLive func(fileName string, raw []byte) error) error {
	meta := s.State.RemoveTrash(id)
	if meta == nil {
		return errors.New("trash item not found")
	}
	path := s.snapshotPath(id)
	raw, err := os.ReadFile(path)
	if err != nil {
		// put meta back
		s.State.AddTrash(*meta)
		return err
	}
	out, err := applyDisabled(raw, !enable)
	if err != nil {
		s.State.AddTrash(*meta)
		return err
	}
	if err := writeLive(meta.FileName, out); err != nil {
		s.State.AddTrash(*meta)
		return err
	}
	_ = os.Remove(path)
	if enable {
		s.State.SetAccountState(meta.AuthIndex, state.Active, "")
	} else {
		s.State.SetAccountState(meta.AuthIndex, state.UserManual, "user_manual")
	}
	s.State.Log(state.ActionLog{
		Auth: meta.AuthIndex, Action: "trash_restore", Source: "manual",
		Reason: fmt.Sprintf("enable=%v", enable),
	})
	return s.State.Save()
}

func applyDisabled(raw []byte, disabled bool) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["disabled"] = disabled
	return json.MarshalIndent(m, "", "  ")
}

func (s *Store) Purge(id string) error {
	meta := s.State.RemoveTrash(id)
	if meta == nil {
		return errors.New("trash item not found")
	}
	_ = os.Remove(s.snapshotPath(id))
	s.State.SetAccountState(meta.AuthIndex, state.Purged, "")
	s.State.Log(state.ActionLog{
		Auth: meta.AuthIndex, Action: "trash_purge", Source: "manual", Reason: id,
	})
	return s.State.Save()
}

func (s *Store) PurgeExpired(now time.Time) (int, error) {
	if !s.AutoPurge {
		return 0, nil
	}
	n := 0
	for _, meta := range s.State.ListTrash() {
		if meta.ExpiresAt.IsZero() || meta.ExpiresAt.After(now) {
			continue
		}
		if err := s.Purge(meta.ID); err != nil {
			return n, err
		}
		s.State.Log(state.ActionLog{
			Auth: meta.AuthIndex, Action: "trash_auto_purge", Source: "tick", Reason: meta.ID,
		})
		n++
	}
	if n > 0 {
		_ = s.State.Save()
	}
	return n, nil
}

// ListMeta never exposes token fields (index only).
func (s *Store) ListMeta() []state.TrashMeta {
	return s.State.ListTrash()
}
