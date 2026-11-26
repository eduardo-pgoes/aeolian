package store

import (
	"time"

	"gorm.io/gorm"
)

type managedWorktreeRow struct {
	ID        int64  `gorm:"primaryKey"`
	RunID     string `gorm:"index"`
	AgentID   string
	Repo      string
	RepoKey   string
	Path      string `gorm:"uniqueIndex"`
	Branch    string
	Base      string
	State     string
	Reason    string `gorm:"not null;default:''"`
	CreatedAt time.Time
	RemovedAt *time.Time
}

func (managedWorktreeRow) TableName() string { return "managed_worktrees" }

func (s *sqliteStore) CreateManagedWorktree(w ManagedWorktree) (ManagedWorktree, error) {
	if w.CreatedAt.IsZero() {
		w.CreatedAt = time.Now()
	}
	row := managedWorktreeRow{
		RunID: w.RunID, AgentID: w.AgentID, Repo: w.Repo, RepoKey: w.RepoKey, Path: w.Path,
		Branch: w.Branch, Base: w.Base, State: WorktreeActive, CreatedAt: w.CreatedAt,
	}
	err := s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
		return []Event{event(row.CreatedAt, row.RunID, row.AgentID, KindWorktreeCreated, WorktreeData{
			V: EventVersion, ID: row.ID, Repo: row.Repo, Path: row.Path, Branch: row.Branch, Base: row.Base,
		})}, nil
	})
	if err != nil {
		return ManagedWorktree{}, err
	}
	return fromManagedRow(row), nil
}

func (s *sqliteStore) ManagedWorktrees(runID string) ([]ManagedWorktree, error) {
	q := s.db.Order("id")
	if runID != "" {
		q = q.Where("run_id = ?", runID)
	}
	var rows []managedWorktreeRow
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ManagedWorktree, len(rows))
	for i, r := range rows {
		out[i] = fromManagedRow(r)
	}
	return out, nil
}

func (s *sqliteStore) SetManagedWorktree(id int64, state, reason string, at time.Time) error {
	updates := map[string]any{"state": state, "reason": reason}
	if state == WorktreeRemoved {
		updates["removed_at"] = at
	}
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		res := tx.Model(&managedWorktreeRow{}).Where("id = ?", id).Updates(updates)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			return nil, ErrNotFound
		}
		var r managedWorktreeRow
		if err := tx.First(&r, "id = ?", id).Error; err != nil {
			return nil, err
		}
		kind := KindWorktreeKept
		switch state {
		case WorktreeRemoved:
			kind = KindWorktreeRemoved
		case WorktreeActive:
			return nil, nil
		}
		return []Event{event(at, r.RunID, r.AgentID, kind, WorktreeData{
			V: EventVersion, ID: r.ID, Path: r.Path, Branch: r.Branch, Reason: reason,
		})}, nil
	})
}

func fromManagedRow(r managedWorktreeRow) ManagedWorktree {
	return ManagedWorktree{
		ID: r.ID, RunID: r.RunID, AgentID: r.AgentID, Repo: r.Repo, RepoKey: r.RepoKey, Path: r.Path,
		Branch: r.Branch, Base: r.Base, State: r.State, Reason: r.Reason, CreatedAt: r.CreatedAt, RemovedAt: r.RemovedAt,
	}
}
