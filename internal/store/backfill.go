package store

import (
	"slices"
	"sort"
	"time"

	"gorm.io/gorm"
)

// backfillEvents gives a database whose rows predate the event log the
// projection events those rows imply, so Fold rebuilds them. It does
// nothing once any run.created or agent.created event exists: from then on
// every mutation appends its own. Each backfilled event is marked
// backfilled and is dated by the row's own timestamps; within one run or
// agent the events keep their logical order even when the stored times
// disagree.
func backfillEvents(db *gorm.DB) (bool, error) {
	var done bool
	err := db.Transaction(func(tx *gorm.DB) error {
		// A write first takes the lock, so nothing commits between the
		// check and the backfill.
		if err := tx.Exec("UPDATE runs SET id = id WHERE 0").Error; err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&eventRow{}).Where("kind IN ?", []string{KindRunCreated, KindAgentCreated}).Limit(1).Count(&n).Error; err != nil || n > 0 {
			return err
		}
		in := &sqliteStore{db: tx}
		agents, err := in.Agents()
		if err != nil {
			return err
		}
		var runRows []runRow
		if err := tx.Preload("Steps", func(db *gorm.DB) *gorm.DB { return db.Order("idx") }).Order("created_at").Find(&runRows).Error; err != nil {
			return err
		}
		if len(agents) == 0 && len(runRows) == 0 {
			return nil
		}
		var evs []keyed
		for _, r := range runRows {
			evs = append(evs, runHistory(r)...)
		}
		for _, a := range agents {
			evs = append(evs, agentHistory(a)...)
		}
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].key.Before(evs[j].key) })
		out := make([]Event, len(evs))
		for i, e := range evs {
			out[i] = e.Event
		}
		for chunk := range slices.Chunk(out, 200) {
			if err := appendTx(tx, chunk); err != nil {
				return err
			}
		}
		done = true
		return nil
	})
	return done, err
}

// keyed is an event with the time it sorts by: its own, or the latest of
// the events before it in its run or agent, whichever is later.
type keyed struct {
	Event
	key time.Time
}

func history(evs []Event) []keyed {
	out := make([]keyed, len(evs))
	var last time.Time
	for i, e := range evs {
		if e.At.After(last) {
			last = e.At
		}
		out[i] = keyed{e, last}
	}
	return out
}

func orTime(t *time.Time, def time.Time) time.Time {
	if t == nil {
		return def
	}
	return *t
}

func runHistory(r runRow) []keyed {
	evs := []Event{runCreated(r, true)}
	for _, st := range r.Steps {
		if st.StartedAt != nil || st.AgentID != "" {
			evs = append(evs, event(orTime(st.StartedAt, r.CreatedAt), r.ID, st.AgentID, KindStepStarted,
				StepStarted{V: EventVersion, Index: st.Idx, AgentID: st.AgentID, Backfilled: true}))
		}
		if slices.Contains(runFinished, st.State) {
			idx := st.Idx
			evs = append(evs, event(orTime(st.DoneAt, orTime(st.StartedAt, r.CreatedAt)), r.ID, "", KindStepFinished,
				Ended{V: EventVersion, Index: &idx, State: st.State, Reason: st.Reason, Backfilled: true}))
		}
	}
	if slices.Contains(runFinished, r.State) {
		evs = append(evs, event(orTime(r.DoneAt, r.CreatedAt), r.ID, "", KindRunFinished,
			Ended{V: EventVersion, State: r.State, Reason: r.Reason, Backfilled: true}))
	}
	if r.AckedAt != nil {
		evs = append(evs, event(*r.AckedAt, r.ID, "", KindRunAcked, Versioned{V: EventVersion, Backfilled: true}))
	}
	return history(evs)
}

func agentHistory(a Agent) []keyed {
	created := a
	if Finished(a.State) {
		created.State = StateBusy
	}
	evs := []Event{agentCreated(created, true)}
	if a.Handoff != nil {
		reason := ""
		if a.State == StateDone {
			reason = a.Reason
		}
		evs = append(evs, event(orTime(a.DoneAt, a.CreatedAt), a.RunID, a.ID, KindAgentDone,
			AgentDone{V: EventVersion, Reason: reason, Handoff: a.Handoff, Backfilled: true}))
		if !Finished(a.State) {
			// A reopened agent keeps the handoff of its earlier done.
			evs = append(evs, event(orTime(a.LastHookAt, a.CreatedAt), a.RunID, a.ID, KindAgentState,
				AgentState{V: EventVersion, From: StateDone, To: a.State, Cause: CauseReopen, Backfilled: true}))
		}
	}
	if a.State == StateKilled || a.State == StateFailed || (a.State == StateDone && a.Handoff == nil) {
		evs = append(evs, event(orTime(a.DoneAt, a.CreatedAt), a.RunID, a.ID, KindAgentEnded,
			Ended{V: EventVersion, State: a.State, Reason: a.Reason, Backfilled: true}))
	}
	if a.LastHook != "" {
		evs = append(evs, event(orTime(a.LastHookAt, a.CreatedAt), a.RunID, a.ID, KindHook,
			HookData{V: EventVersion, Event: a.LastHook, Backfilled: true}))
	}
	if a.GCAt != nil {
		evs = append(evs, event(*a.GCAt, a.RunID, a.ID, KindAgentGC, Versioned{V: EventVersion, Backfilled: true}))
	}
	return history(evs)
}
