package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Event is one entry of the append-only log of what happened. ID grows
// with every append and is never reused, so it is a reader's cursor. Data
// is shaped by Kind; payloads only ever gain fields.
type Event struct {
	ID      int64           `json:"id"`
	At      time.Time       `json:"at"`
	RunID   string          `json:"run_id,omitempty"`
	AgentID string          `json:"agent_id,omitempty"`
	Kind    string          `json:"kind"`
	Data    json.RawMessage `json:"data"`
}

// EventQuery selects events with an id above After, matching every filter
// set: RunID, AgentID, and Kind among Kinds. Limit caps how many come back;
// zero means DefaultEventLimit.
type EventQuery struct {
	After   int64
	RunID   string
	AgentID string
	Kinds   []string
	Limit   int
}

// DefaultEventLimit is how many events Events returns when the query sets
// no limit.
const DefaultEventLimit = 1000

type eventRow struct {
	ID      int64 `gorm:"primaryKey"`
	At      time.Time
	RunID   string
	AgentID string
	Kind    string
	Data    string
}

func (eventRow) TableName() string { return "events" }

// eventsSchema is written by hand so the id is AUTOINCREMENT: SQLite then
// never hands out an id again, even after the newest rows are deleted.
var eventsSchema = []string{
	`CREATE TABLE IF NOT EXISTS events(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at DATETIME NOT NULL,
		run_id TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		data TEXT NOT NULL DEFAULT '{}')`,
	`CREATE INDEX IF NOT EXISTS idx_events_run ON events(run_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_agent ON events(agent_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind, id)`,
	`CREATE INDEX IF NOT EXISTS idx_events_run_kind ON events(run_id, kind)`,
}

func migrateEvents(db *gorm.DB) error {
	for _, q := range eventsSchema {
		if err := db.Exec(q).Error; err != nil {
			return err
		}
	}
	return nil
}

// subscribers fans a wake-up out to every Subscribe channel.
type subscribers struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func (s *subscribers) add() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan struct{}]struct{}{}
	}
	s.subs[c] = struct{}{}
	s.mu.Unlock()
	var once sync.Once
	return c, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, c)
			s.mu.Unlock()
		})
	}
}

// notify signals every subscriber without blocking; a subscriber that has
// a signal pending already keeps just the one.
func (s *subscribers) notify() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

func (s *sqliteStore) Subscribe() (<-chan struct{}, func()) { return s.subs.add() }

func (s *sqliteStore) Append(evs ...Event) error {
	if len(evs) == 0 {
		return nil
	}
	if err := s.db.Transaction(func(tx *gorm.DB) error { return appendTx(tx, evs) }); err != nil {
		return err
	}
	s.subs.notify()
	return nil
}

// appendTx writes evs inside tx. A mutation that emits its events in its
// own transaction calls it there, then s.subs.notify once the transaction
// has committed.
func appendTx(tx *gorm.DB, evs []Event) error {
	rows := make([]eventRow, len(evs))
	for i, e := range evs {
		if e.Kind == "" {
			return errors.New("event without a kind")
		}
		at := e.At
		if at.IsZero() {
			at = time.Now()
		}
		data := string(e.Data)
		if len(e.Data) == 0 || string(e.Data) == "null" {
			data = "{}"
		}
		rows[i] = eventRow{At: at, RunID: e.RunID, AgentID: e.AgentID, Kind: e.Kind, Data: data}
	}
	return tx.Create(&rows).Error
}

func (s *sqliteStore) LastEventID() (int64, error) {
	var id int64
	err := s.db.Raw("SELECT COALESCE(MAX(id), 0) FROM events").Scan(&id).Error
	return id, err
}

func (s *sqliteStore) Events(q EventQuery) ([]Event, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	db := s.db.Where("id > ?", q.After).Order("id").Limit(limit)
	if q.RunID != "" {
		db = db.Where("run_id = ?", q.RunID)
	}
	if q.AgentID != "" {
		db = db.Where("agent_id = ?", q.AgentID)
	}
	if len(q.Kinds) > 0 {
		db = db.Where("kind IN ?", q.Kinds)
	}
	var rows []eventRow
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	evs := make([]Event, len(rows))
	for i, r := range rows {
		evs[i] = Event{ID: r.ID, At: r.At, RunID: r.RunID, AgentID: r.AgentID, Kind: r.Kind, Data: json.RawMessage(r.Data)}
	}
	return evs, nil
}

func (s *sqliteStore) LastEvents(kind string, agentIDs []string) (map[string]Event, error) {
	out := map[string]Event{}
	for chunk := range slices.Chunk(agentIDs, 500) {
		var rows []eventRow
		last := s.db.Model(&eventRow{}).Select("MAX(id)").Where("kind = ? AND agent_id IN ?", kind, chunk).Group("agent_id")
		if err := s.db.Where("id IN (?)", last).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.AgentID] = Event{ID: r.ID, At: r.At, RunID: r.RunID, AgentID: r.AgentID, Kind: r.Kind, Data: json.RawMessage(r.Data)}
		}
	}
	return out, nil
}

func (s *sqliteStore) RunTokens(runIDs []string) (map[string]int64, error) {
	out := map[string]int64{}
	for chunk := range slices.Chunk(runIDs, 500) {
		var rows []struct {
			RunID  string
			Tokens int64
		}
		err := s.db.Model(&eventRow{}).
			Select(`run_id, COALESCE(SUM(COALESCE(json_extract(data, '$.input'), 0) + COALESCE(json_extract(data, '$.output'), 0) + COALESCE(json_extract(data, '$.cache_write'), 0)), 0) AS tokens`).
			Where("kind = ? AND run_id IN ?", KindUsage, chunk).
			Group("run_id").
			Scan(&rows).Error
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.RunID] = r.Tokens
		}
	}
	return out, nil
}

func (s *sqliteStore) ReopenedTokens(runID string) (int64, error) {
	var tokens int64
	err := s.db.Raw(`SELECT COALESCE(SUM(COALESCE(json_extract(u.data, '$.input'), 0) + COALESCE(json_extract(u.data, '$.output'), 0) + COALESCE(json_extract(u.data, '$.cache_write'), 0)), 0)
		FROM events u
		WHERE u.run_id = ? AND u.kind = ? AND EXISTS (
			SELECT 1 FROM events r
			WHERE r.agent_id = u.agent_id AND r.kind = ? AND r.id < u.id AND json_extract(r.data, '$.cause') = ?)`,
		runID, KindUsage, KindAgentState, CauseReopen).Scan(&tokens).Error
	return tokens, err
}

func (s *sqliteStore) CompactEvents(runIDs, kinds []string) (int64, error) {
	for _, k := range kinds {
		if !slices.Contains(BulkyKinds, k) {
			return 0, fmt.Errorf("cannot compact %s events: only %v", k, BulkyKinds)
		}
	}
	if len(runIDs) == 0 || len(kinds) == 0 {
		return 0, nil
	}
	// A pruned hook event keeps its hook's name, which Fold reads.
	pruned := gorm.Expr(`CASE WHEN kind = ? THEN json_object('v', 1, 'pruned', json('true'), 'bytes', length(data), 'event', json_extract(data, '$.event'))
		ELSE json_object('v', 1, 'pruned', json('true'), 'bytes', length(data)) END`, KindHook)
	var n int64
	for chunk := range slices.Chunk(runIDs, 500) {
		res := s.db.Model(&eventRow{}).
			Where("run_id IN ? AND kind IN ? AND json_extract(data, '$.pruned') IS NOT 1", chunk, kinds).
			Update("data", pruned)
		if res.Error != nil {
			return n, res.Error
		}
		n += res.RowsAffected
	}
	return n, nil
}
