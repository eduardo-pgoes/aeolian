package store

import (
	"errors"
	"net/url"
	"slices"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type agentRow struct {
	ID          string `gorm:"primaryKey"`
	SessionID   string
	TmuxSession string
	Dir         string
	Prompt      string
	ParentID    string
	Role        string
	RunID       string `gorm:"index"`
	Step        string
	Headless    bool
	State       string `gorm:"index"`
	Reason      string `gorm:"not null;default:''"`
	LastHook    string
	LastHookAt  *time.Time
	CreatedAt   time.Time
	DoneAt      *time.Time
	GCAt        *time.Time
}

func (agentRow) TableName() string { return "agents" }

type handoffRow struct {
	AgentID     string `gorm:"primaryKey"`
	Text        string
	Synthesized bool `gorm:"not null;default:false"`
	CreatedAt   time.Time
	// CollectedAt is when the handoff reached the agent's parent; nil
	// while it has not.
	CollectedAt *time.Time
	Worktrees   []worktreeRow `gorm:"foreignKey:AgentID;references:AgentID"`
}

func (handoffRow) TableName() string { return "handoffs" }

type worktreeRow struct {
	ID      uint   `gorm:"primaryKey"`
	AgentID string `gorm:"index"`
	Repo    string
	Path    string
	Branch  string
}

func (worktreeRow) TableName() string { return "worktrees" }

type runRow struct {
	ID         string `gorm:"primaryKey"`
	Goal       string
	Workflow   string
	Source     string            `gorm:"index:idx_runs_source_ref"`
	Ref        string            `gorm:"index:idx_runs_source_ref"`
	Account    string            `gorm:"not null;default:''"`
	Env        map[string]string `gorm:"type:text;serializer:json;not null;default:''"`
	GitConfig  [][2]string       `gorm:"type:text;serializer:json;not null;default:''"`
	ContextCap int64             `gorm:"not null;default:0"`
	CapKills   int               `gorm:"not null;default:0"`
	TokenCap   int64             `gorm:"not null;default:0"`
	Dir        string            `gorm:"not null;default:''"`
	State      string
	Reason     string `gorm:"not null;default:''"`
	CreatedAt  time.Time
	DoneAt     *time.Time
	AckedAt    *time.Time
	Steps      []stepRow `gorm:"foreignKey:RunID;references:ID"`

	TicketID         string `gorm:"index"`
	TicketIdentifier string
}

func (runRow) TableName() string { return "runs" }

type stepRow struct {
	RunID     string `gorm:"primaryKey"`
	Idx       int    `gorm:"primaryKey;autoIncrement:false"`
	Name      string
	AgentID   string
	State     string
	Reason    string `gorm:"not null;default:''"`
	StartedAt *time.Time
	DoneAt    *time.Time
}

func (stepRow) TableName() string { return "steps" }

type sqliteStore struct {
	db   *gorm.DB
	subs subscribers
}

// Open opens or creates the database at path in WAL mode and migrates it.
func Open(path string) (Store, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return nil, err
	}
	m := db.Migrator()
	addsAck := !m.HasColumn(&runRow{}, "acked_at")
	if addsAck && m.HasColumn(&runRow{}, "reported_at") {
		if err := m.RenameColumn(&runRow{}, "reported_at", "acked_at"); err != nil {
			return nil, err
		}
	}
	addsReason := !m.HasColumn(&runRow{}, "reason")
	addsCollected := m.HasTable(&handoffRow{}) && !m.HasColumn(&handoffRow{}, "collected_at")
	if err := db.AutoMigrate(&agentRow{}, &handoffRow{}, &worktreeRow{}, &runRow{}, &stepRow{}, &partRow{}, &claimRow{}, &managedWorktreeRow{}); err != nil {
		return nil, err
	}
	if err := migrateEvents(db); err != nil {
		return nil, err
	}
	// Only on the Open that adds reasons: afterwards every finish writes
	// its own.
	if addsReason {
		if err := backfillReasons(db); err != nil {
			return nil, err
		}
	}
	err = db.Model(&runRow{}).Where("ticket_id <> '' AND (source IS NULL OR source = '')").
		Updates(map[string]any{"source": SourceLinear, "ref": gorm.Expr("ticket_id")}).Error
	if err != nil {
		return nil, err
	}
	// Only on the Open that adds collected_at: every handoff before it was
	// returned by spawn or typed in, as await's letters lived in memory.
	if addsCollected {
		if err := db.Model(&handoffRow{}).Where("collected_at IS NULL").Update("collected_at", gorm.Expr("created_at")).Error; err != nil {
			return nil, err
		}
	}
	// Only on the Open that adds the ack: afterwards an unacked finished
	// run is one nobody has seen yet.
	if addsAck {
		err = db.Model(&runRow{}).Where("ticket_id = '' AND acked_at IS NULL AND state IN ?", runFinished).
			Update("acked_at", gorm.Expr("done_at")).Error
		if err != nil {
			return nil, err
		}
	}
	// Last, so the events see every row as the migrations above left it.
	if _, err := backfillEvents(db); err != nil {
		return nil, err
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// write runs fn in a transaction and appends the events it returns in the
// same transaction, so a row change and its events commit together or not
// at all; subscribers hear of them once they have committed. Inside fn,
// write before you read: a read first pins a snapshot SQLite refuses to
// upgrade to a write once another writer commits (SQLITE_BUSY_SNAPSHOT),
// and busy_timeout does not retry that.
func (s *sqliteStore) write(fn func(tx *gorm.DB) ([]Event, error)) error {
	var evs []Event
	err := s.db.Transaction(func(tx *gorm.DB) error {
		var err error
		if evs, err = fn(tx); err != nil || len(evs) == 0 {
			return err
		}
		return appendTx(tx, evs)
	})
	if err == nil && len(evs) > 0 {
		s.subs.notify()
	}
	return err
}

// lockAgent takes the write lock with a write that changes nothing, then
// reads agent id's row, so the read sees the latest commit.
func lockAgent(tx *gorm.DB, id string) (agentRow, error) {
	res := tx.Exec("UPDATE agents SET state = state WHERE id = ?", id)
	if res.Error != nil {
		return agentRow{}, res.Error
	}
	if res.RowsAffected == 0 {
		return agentRow{}, ErrNotFound
	}
	var r agentRow
	err := tx.First(&r, "id = ?", id).Error
	return r, err
}

func (s *sqliteStore) CreateAgent(a Agent) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		err := tx.Create(&agentRow{
			ID: a.ID, SessionID: a.SessionID, TmuxSession: a.TmuxSession, Dir: a.Dir,
			Prompt: a.Prompt, ParentID: a.ParentID, Role: a.Role, RunID: a.RunID, Step: a.Step, Headless: a.Headless, State: a.State,
			CreatedAt: a.CreatedAt,
		}).Error
		if err != nil {
			return nil, err
		}
		return []Event{agentCreated(a, false)}, nil
	})
}

func agentCreated(a Agent, backfilled bool) Event {
	return event(a.CreatedAt, a.RunID, a.ID, KindAgentCreated, AgentCreated{
		V: EventVersion, SessionID: a.SessionID, Tmux: a.TmuxSession, Dir: a.Dir, ParentID: a.ParentID, Role: a.Role,
		Step: a.Step, Headless: a.Headless, State: a.State, Prompt: a.Prompt, Backfilled: backfilled,
	})
}

func (s *sqliteStore) Agent(id string) (Agent, error) {
	var r agentRow
	err := s.db.First(&r, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Agent{}, ErrNotFound
	}
	if err != nil {
		return Agent{}, err
	}
	a := fromRow(r)
	var h handoffRow
	err = s.db.Preload("Worktrees").First(&h, "agent_id = ?", id).Error
	switch {
	case err == nil:
		a.Handoff = fromHandoffRow(h)
		if a.Handoff.Parts, err = partsOf(s.db, id); err != nil {
			return Agent{}, err
		}
	case !errors.Is(err, gorm.ErrRecordNotFound):
		return Agent{}, err
	}
	return a, nil
}

func (s *sqliteStore) Agents() ([]Agent, error) {
	var rows []agentRow
	if err := s.db.Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	var handoffs []handoffRow
	if err := s.db.Preload("Worktrees").Find(&handoffs).Error; err != nil {
		return nil, err
	}
	return withHandoffs(rows, handoffs), nil
}

func (s *sqliteStore) AgentsOfRuns(runIDs []string) ([]Agent, error) {
	if len(runIDs) == 0 {
		return []Agent{}, nil
	}
	var rows []agentRow
	if err := s.db.Where("run_id IN ?", runIDs).Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	var handoffs []handoffRow
	if len(ids) > 0 {
		if err := s.db.Preload("Worktrees").Where("agent_id IN ?", ids).Find(&handoffs).Error; err != nil {
			return nil, err
		}
	}
	return withHandoffs(rows, handoffs), nil
}

func (s *sqliteStore) MarkGC(agentID string, at time.Time) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		r, err := lockAgent(tx, agentID)
		if err != nil {
			return nil, err
		}
		if err := tx.Model(&agentRow{}).Where("id = ?", agentID).Update("gc_at", at).Error; err != nil {
			return nil, err
		}
		return []Event{event(at, r.RunID, agentID, KindAgentGC, Versioned{V: EventVersion})}, nil
	})
}

func withHandoffs(rows []agentRow, handoffs []handoffRow) []Agent {
	byAgent := map[string]*Handoff{}
	for _, h := range handoffs {
		byAgent[h.AgentID] = fromHandoffRow(h)
	}
	agents := make([]Agent, len(rows))
	for i, r := range rows {
		agents[i] = fromRow(r)
		agents[i].Handoff = byAgent[r.ID]
	}
	return agents
}

func (s *sqliteStore) Hook(agentID, hook string, at time.Time, evs ...Event) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		r, err := lockAgent(tx, agentID)
		if err != nil {
			return nil, err
		}
		updates := map[string]any{"last_hook": hook, "last_hook_at": at}
		to := r.State
		if !slices.Contains(agentFinished, r.State) {
			switch hook {
			case "UserPromptSubmit":
				to = StateBusy
			case "Stop":
				to = StateIdle
			}
		}
		var out []Event
		if to != r.State {
			updates["state"] = to
			out = append(out, event(at, r.RunID, agentID, KindAgentState, AgentState{V: EventVersion, From: r.State, To: to, Cause: hook}))
		}
		if err := tx.Model(&agentRow{}).Where("id = ?", agentID).Updates(updates).Error; err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(evs, func(e Event) bool { return e.Kind == KindHook || e.Kind == KindToolUsed }) {
			out = append(out, event(at, r.RunID, agentID, KindHook, HookData{V: EventVersion, Event: hook}))
		}
		for _, e := range evs {
			e.At, e.RunID, e.AgentID = at, r.RunID, agentID
			out = append(out, e)
		}
		return out, nil
	})
}

func (s *sqliteStore) LiveAgents() ([]Agent, error) {
	var rows []agentRow
	if err := s.db.Where("state NOT IN ?", agentFinished).Order("created_at").Find(&rows).Error; err != nil {
		return nil, err
	}
	agents := make([]Agent, len(rows))
	for i, r := range rows {
		agents[i] = fromRow(r)
	}
	return agents, nil
}

func (s *sqliteStore) Done(agentID string, h Handoff, reason string, at time.Time) error {
	return s.EndAgentHandoff(agentID, End{State: StateDone, Reason: reason, At: at}, h)
}

// EndAgentHandoff finishes the row first, so the transaction writes before
// it reads anything; see finishRow. It appends agent.done, with the state
// when it is not done, so the fold restores the handoff as well.
func (s *sqliteStore) EndAgentHandoff(agentID string, e End, h Handoff) error {
	e.At = e.at()
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := finishRow(tx.Model(&agentRow{}).Where("id = ?", agentID), e, agentFinished); err != nil {
			return nil, err
		}
		if err := tx.Where("agent_id = ?", agentID).Delete(&worktreeRow{}).Error; err != nil {
			return nil, err
		}
		if err := tx.Where("agent_id = ?", agentID).Delete(&handoffRow{}).Error; err != nil {
			return nil, err
		}
		if err := tx.Where("agent_id = ?", agentID).Delete(&partRow{}).Error; err != nil {
			return nil, err
		}
		if len(h.Parts) > 0 {
			parts := make([]partRow, len(h.Parts))
			for i, p := range h.Parts {
				parts[i] = partRow{AgentID: agentID, Idx: i, Kind: p.Kind, Body: string(p.Body)}
			}
			if err := tx.Create(&parts).Error; err != nil {
				return nil, err
			}
		}
		row := handoffRow{AgentID: agentID, Text: h.Text, Synthesized: h.Synthesized, CreatedAt: e.at()}
		for _, w := range h.Worktrees {
			row.Worktrees = append(row.Worktrees, worktreeRow{Repo: w.Repo, Path: w.Path, Branch: w.Branch})
		}
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
		runID, err := agentRunID(tx, agentID)
		if err != nil {
			return nil, err
		}
		d := AgentDone{V: EventVersion, Reason: e.Reason, Handoff: fromHandoffRow(row)}
		d.Handoff.Parts = h.Parts
		if e.State != StateDone {
			d.State = e.State
		}
		return []Event{event(e.At, runID, agentID, KindAgentDone, d)}, nil
	})
}

func agentRunID(tx *gorm.DB, agentID string) (string, error) {
	var r agentRow
	err := tx.Select("run_id").First(&r, "id = ?", agentID).Error
	return r.RunID, err
}

func (s *sqliteStore) EndAgent(agentID string, e End) error {
	e.At = e.at()
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := finishRow(tx.Model(&agentRow{}).Where("id = ?", agentID), e, agentFinished); err != nil {
			return nil, err
		}
		runID, err := agentRunID(tx, agentID)
		if err != nil {
			return nil, err
		}
		return []Event{event(e.At, runID, agentID, KindAgentEnded, Ended{V: EventVersion, State: e.State, Reason: e.Reason})}, nil
	})
}

func (s *sqliteStore) BusyAgentIDs() ([]string, error) {
	var ids []string
	err := s.db.Model(&agentRow{}).Where("state = ?", StateBusy).Order("created_at").Pluck("id", &ids).Error
	return ids, err
}

func (s *sqliteStore) CollectHandoffs(helperIDs []string, by string, at time.Time) error {
	if len(helperIDs) == 0 {
		return nil
	}
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		var evs []Event
		for _, id := range helperIDs {
			res := tx.Model(&handoffRow{}).Where("agent_id = ? AND collected_at IS NULL", id).Update("collected_at", at)
			if res.Error != nil {
				return nil, res.Error
			}
			if res.RowsAffected == 0 {
				continue
			}
			var r agentRow
			if err := tx.Select("run_id", "parent_id").First(&r, "id = ?", id).Error; err != nil {
				return nil, err
			}
			evs = append(evs, event(at, r.RunID, id, KindHandoffCollected, HandoffCollected{V: EventVersion, ParentID: r.ParentID, By: by}))
		}
		return evs, nil
	})
}

func (s *sqliteStore) Uncollected(parentID string) ([]Agent, error) {
	var ids []string
	err := s.db.Model(&agentRow{}).
		Joins("JOIN handoffs ON handoffs.agent_id = agents.id").
		Where("agents.parent_id = ? AND agents.state IN ? AND handoffs.collected_at IS NULL", parentID, agentFinished).
		Order("handoffs.created_at").Pluck("agents.id", &ids).Error
	if err != nil {
		return nil, err
	}
	agents := make([]Agent, 0, len(ids))
	for _, id := range ids {
		a, err := s.Agent(id)
		if err != nil {
			return nil, err
		}
		agents = append(agents, a)
	}
	return agents, nil
}

func (s *sqliteStore) Reopen(agentID string) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		r, err := lockAgent(tx, agentID)
		if err != nil {
			return nil, err
		}
		err = tx.Model(&agentRow{}).Where("id = ?", agentID).
			Updates(map[string]any{"state": StateIdle, "reason": "", "done_at": nil}).Error
		if err != nil {
			return nil, err
		}
		return []Event{event(time.Now(), r.RunID, agentID, KindAgentState, AgentState{V: EventVersion, From: r.State, To: StateIdle, Cause: CauseReopen})}, nil
	})
}

func (s *sqliteStore) CreateRun(r Run) error {
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	row := runRow{ID: r.ID, Goal: r.Goal, Workflow: r.Workflow, Source: r.Source, Ref: r.Ref, Account: r.Account, Env: r.Env, GitConfig: r.GitConfig, ContextCap: r.ContextCap, CapKills: r.CapKills, TokenCap: r.TokenCap, Dir: r.Dir, State: StateBusy, CreatedAt: r.CreatedAt}
	if r.State == StateQueued {
		row.State = StateQueued
	}
	for i, st := range r.Steps {
		row.Steps = append(row.Steps, stepRow{RunID: r.ID, Idx: i, Name: st.Name, State: StatePending})
	}
	// The insert comes first so the transaction holds SQLite's write lock
	// before it reads; a read first would fail on a stale snapshot whenever
	// another write landed in between.
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := tx.Create(&row).Error; err != nil {
			return nil, err
		}
		if r.Ref != "" {
			var live []runRow
			err := tx.Select("id").Where("source = ? AND ref = ? AND id <> ? AND state NOT IN ?", r.Source, r.Ref, r.ID, runFinished).
				Order("created_at").Limit(1).Find(&live).Error
			if err != nil {
				return nil, err
			}
			if len(live) > 0 {
				return nil, &LiveRunError{RunID: live[0].ID, Source: r.Source, Ref: r.Ref}
			}
		}
		return []Event{runCreated(row, false)}, nil
	})
}

func runCreated(row runRow, backfilled bool) Event {
	steps := make([]string, len(row.Steps))
	for i, st := range row.Steps {
		steps[i] = st.Name
	}
	state := StateBusy
	if row.State == StateQueued {
		state = StateQueued
	}
	return event(row.CreatedAt, row.ID, "", KindRunCreated, RunCreated{
		V: EventVersion, Goal: row.Goal, Source: row.Source, Ref: row.Ref, Account: row.Account, Env: row.Env, GitConfig: row.GitConfig, ContextCap: row.ContextCap, CapKills: row.CapKills, TokenCap: row.TokenCap, Dir: row.Dir, State: state, Steps: steps, Workflow: row.Workflow, Backfilled: backfilled,
	})
}

func (s *sqliteStore) StartQueuedRun(runID string) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		res := tx.Model(&runRow{}).Where("id = ? AND state = ?", runID, StateQueued).Update("state", StateBusy)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			var n int64
			if err := tx.Model(&runRow{}).Where("id = ?", runID).Count(&n).Error; err != nil {
				return nil, err
			}
			if n == 0 {
				return nil, ErrNotFound
			}
			return nil, ErrFinished
		}
		return []Event{event(time.Now(), runID, "", KindRunStarted, Versioned{V: EventVersion})}, nil
	})
}

func (s *sqliteStore) Run(id string) (Run, error) {
	var r runRow
	err := s.db.Preload("Steps", func(db *gorm.DB) *gorm.DB { return db.Order("idx") }).First(&r, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	return fromRunRow(r), nil
}

func (s *sqliteStore) Runs(f RunFilter) ([]Run, error) {
	match := func(q *gorm.DB) *gorm.DB {
		if f.Source != "" {
			q = q.Where("source = ?", f.Source)
		}
		if f.Ref != "" {
			q = q.Where("ref = ?", f.Ref)
		}
		return q
	}
	q := match(s.db.Order("created_at DESC"))
	switch {
	case f.Limit > 0 && f.Active:
		newest := match(s.db.Model(&runRow{}).Select("id").Order("created_at DESC").Limit(f.Limit))
		include := f.Include
		if include == nil {
			include = []string{}
		}
		q = q.Where("id IN (?) OR state NOT IN ? OR acked_at IS NULL OR id IN ?", newest, runFinished, include)
	case f.Limit > 0:
		q = q.Limit(f.Limit)
	}
	return s.runs(q)
}

func (s *sqliteStore) Inbox(source string) ([]Run, error) {
	q := s.db.Where("acked_at IS NULL AND state IN ?", runFinished).Order("done_at").Order("created_at")
	if source != "" {
		q = q.Where("source = ?", source)
	}
	return s.runs(q)
}

func (s *sqliteStore) Ack(runID string, at time.Time) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		res := tx.Model(&runRow{}).Where("id = ? AND acked_at IS NULL AND state IN ?", runID, runFinished).Update("acked_at", at)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected > 0 {
			return []Event{event(at, runID, "", KindRunAcked, Versioned{V: EventVersion})}, nil
		}
		var r runRow
		err := tx.Select("state").First(&r, "id = ?", runID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if !slices.Contains(runFinished, r.State) {
			return nil, ErrNotFinished
		}
		return nil, nil
	})
}

func (s *sqliteStore) runs(q *gorm.DB) ([]Run, error) {
	var rows []runRow
	err := q.Preload("Steps", func(db *gorm.DB) *gorm.DB { return db.Order("idx") }).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	runs := make([]Run, len(rows))
	for i, r := range rows {
		runs[i] = fromRunRow(r)
	}
	return runs, nil
}

func fromRunRow(r runRow) Run {
	run := Run{ID: r.ID, Goal: r.Goal, Workflow: r.Workflow, Source: r.Source, Ref: r.Ref, Account: r.Account, Env: r.Env, GitConfig: r.GitConfig, ContextCap: r.ContextCap, CapKills: r.CapKills, TokenCap: r.TokenCap, Dir: r.Dir, State: r.State, Reason: r.Reason, CreatedAt: r.CreatedAt, DoneAt: r.DoneAt, AckedAt: r.AckedAt, Steps: []Step{}}
	for _, st := range r.Steps {
		run.Steps = append(run.Steps, Step{
			Index: st.Idx, Name: st.Name, AgentID: st.AgentID, State: st.State, Reason: st.Reason, StartedAt: st.StartedAt, DoneAt: st.DoneAt,
		})
	}
	return run
}

func (s *sqliteStore) StartStep(runID string, index int, agentID string, at time.Time) error {
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		res := tx.Model(&stepRow{}).Where("run_id = ? AND idx = ?", runID, index).
			Updates(map[string]any{"agent_id": agentID, "state": StateBusy, "started_at": at})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			return nil, ErrNotFound
		}
		return []Event{event(at, runID, agentID, KindStepStarted, StepStarted{V: EventVersion, Index: index, AgentID: agentID})}, nil
	})
}

var runFinished = []string{StateDone, StateBlocked, StateFailed, StateKilled}

func (s *sqliteStore) FinishStep(runID string, index int, e End) error {
	e.At = e.at()
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := finishRow(tx.Model(&stepRow{}).Where("run_id = ? AND idx = ?", runID, index), e, runFinished); err != nil {
			return nil, err
		}
		return []Event{event(e.At, runID, "", KindStepFinished, Ended{V: EventVersion, Index: &index, State: e.State, Reason: e.Reason})}, nil
	})
}

func (s *sqliteStore) FinishRun(runID string, e End) error {
	e.At = e.at()
	return s.write(func(tx *gorm.DB) ([]Event, error) {
		if err := finishRow(tx.Model(&runRow{}).Where("id = ?", runID), e, runFinished); err != nil {
			return nil, err
		}
		return []Event{event(e.At, runID, "", KindRunFinished, Ended{V: EventVersion, State: e.State, Reason: e.Reason})}, nil
	})
}

// finishRow moves the one row q selects to e unless it is in one of the
// finished states already; the check and the update are one statement, so
// of two callers racing to finish the same row only one succeeds. It writes
// before it reads: inside a transaction, a read first would pin a snapshot
// that SQLite refuses to upgrade to a write (SQLITE_BUSY_SNAPSHOT) once
// another writer commits, and busy_timeout does not retry that.
func finishRow(q *gorm.DB, e End, finished []string) error {
	res := q.Session(&gorm.Session{}).Where("state NOT IN ?", finished).
		Updates(map[string]any{"state": e.State, "reason": e.Reason, "done_at": e.at()})
	if res.Error != nil || res.RowsAffected > 0 {
		return res.Error
	}
	var n int64
	if err := q.Session(&gorm.Session{}).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return ErrFinished
}

func fromRow(r agentRow) Agent {
	return Agent{
		ID: r.ID, SessionID: r.SessionID, TmuxSession: r.TmuxSession, Dir: r.Dir,
		Prompt: r.Prompt, ParentID: r.ParentID, Role: r.Role, RunID: r.RunID, Step: r.Step, Headless: r.Headless, State: r.State, Reason: r.Reason, LastHook: r.LastHook, LastHookAt: r.LastHookAt,
		CreatedAt: r.CreatedAt, DoneAt: r.DoneAt, GCAt: r.GCAt,
	}
}

func fromHandoffRow(h handoffRow) *Handoff {
	out := &Handoff{Text: h.Text, Worktrees: []Worktree{}, Synthesized: h.Synthesized}
	for _, w := range h.Worktrees {
		out.Worktrees = append(out.Worktrees, Worktree{Repo: w.Repo, Path: w.Path, Branch: w.Branch})
	}
	return out
}

// backfillReasons gives the rows that finished before reasons existed the
// reason their state and their neighbours imply. A step agent killed while
// its step failed was lost by the sweep; a failed step with no agent, or
// whose agent's launch failed, never started; a run that failed takes the
// reason of its failed step.
func backfillReasons(db *gorm.DB) error {
	startFailed := "EXISTS (SELECT 1 FROM handoffs h WHERE h.agent_id = agents.id AND h.text LIKE 'start agent %')"
	return db.Transaction(func(tx *gorm.DB) error {
		for _, q := range []string{
			`UPDATE agents SET reason = CASE
				WHEN state = 'done' AND ` + startFailed + ` THEN 'start_failed'
				WHEN state = 'done' AND EXISTS (SELECT 1 FROM steps s WHERE s.agent_id = agents.id AND s.state = 'blocked') THEN 'blocked'
				WHEN state = 'done' THEN 'delivered'
				WHEN state = 'failed' THEN 'exited'
				WHEN state = 'killed' AND COALESCE(parent_id, '') = '' AND EXISTS (SELECT 1 FROM steps s WHERE s.agent_id = agents.id AND s.state = 'failed') THEN 'lost'
				WHEN state = 'killed' THEN 'killed'
				ELSE '' END
			WHERE reason = ''`,
			`UPDATE steps SET reason = CASE
				WHEN state = 'done' THEN 'delivered'
				WHEN state = 'blocked' THEN 'blocked'
				WHEN state = 'killed' THEN 'killed'
				WHEN state = 'failed' THEN COALESCE(
					(SELECT CASE WHEN a.reason IN ('lost', 'exited', 'start_failed') THEN a.reason
						WHEN a.reason = 'killed' THEN 'killed' ELSE 'exited' END
					 FROM agents a WHERE a.id = steps.agent_id),
					'start_failed')
				ELSE '' END
			WHERE reason = ''`,
			`UPDATE runs SET reason = CASE
				WHEN state = 'done' THEN 'delivered'
				WHEN state = 'blocked' THEN 'blocked'
				WHEN state = 'killed' THEN 'killed'
				WHEN state = 'failed' THEN COALESCE(
					(SELECT s.reason FROM steps s WHERE s.run_id = runs.id AND s.state = 'failed' AND s.reason <> '' ORDER BY s.idx DESC LIMIT 1),
					'exited')
				ELSE '' END
			WHERE reason = ''`,
		} {
			if err := tx.Exec(q).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
