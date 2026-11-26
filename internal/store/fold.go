package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"time"

	"gorm.io/gorm"
)

// Snapshot is the projection tables as the event log says they are.
type Snapshot struct {
	// Runs are in the order they were created, each with its steps.
	Runs []Run
	// Agents are in the order they were created, each with its handoff.
	Agents []Agent
}

// Fold rebuilds the runs, steps and agents from events alone, read oldest
// first. It reads only ProjectionKinds and ignores the rest, so the log up
// to any id is the world as of that id. An event about a run or agent that
// was never created is skipped.
func Fold(events []Event) Snapshot {
	runs := map[string]*Run{}
	agents := map[string]*Agent{}
	var runOrder, agentOrder []string
	for _, e := range events {
		at := e.At
		switch e.Kind {
		case KindRunCreated:
			var d RunCreated
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			r := &Run{ID: e.RunID, Goal: d.Goal, Workflow: d.Workflow, Source: d.Source, Ref: d.Ref, Account: d.Account, Env: d.Env, GitConfig: d.GitConfig, ContextCap: d.ContextCap, CapKills: d.CapKills, TokenCap: d.TokenCap, Dir: d.Dir, State: d.State, CreatedAt: at, Steps: []Step{}}
			for i, name := range d.Steps {
				r.Steps = append(r.Steps, Step{Index: i, Name: name, State: StatePending})
			}
			if _, ok := runs[e.RunID]; !ok {
				runOrder = append(runOrder, e.RunID)
			}
			runs[e.RunID] = r
		case KindRunStarted:
			if r := runs[e.RunID]; r != nil {
				r.State = StateBusy
			}
		case KindRunFinished:
			var d Ended
			if r := runs[e.RunID]; r != nil && json.Unmarshal(e.Data, &d) == nil {
				r.State, r.Reason, r.DoneAt = d.State, d.Reason, &at
			}
		case KindRunAcked:
			if r := runs[e.RunID]; r != nil && r.AckedAt == nil {
				r.AckedAt = &at
			}
		case KindStepStarted:
			var d StepStarted
			if r := runs[e.RunID]; r != nil && json.Unmarshal(e.Data, &d) == nil && d.Index >= 0 && d.Index < len(r.Steps) {
				st := &r.Steps[d.Index]
				st.AgentID, st.State, st.StartedAt = d.AgentID, StateBusy, &at
			}
		case KindStepFinished:
			var d Ended
			if r := runs[e.RunID]; r != nil && json.Unmarshal(e.Data, &d) == nil && d.Index != nil && *d.Index >= 0 && *d.Index < len(r.Steps) {
				st := &r.Steps[*d.Index]
				st.State, st.Reason, st.DoneAt = d.State, d.Reason, &at
			}
		case KindAgentCreated:
			var d AgentCreated
			if json.Unmarshal(e.Data, &d) != nil {
				continue
			}
			if _, ok := agents[e.AgentID]; !ok {
				agentOrder = append(agentOrder, e.AgentID)
			}
			agents[e.AgentID] = &Agent{
				ID: e.AgentID, SessionID: d.SessionID, TmuxSession: d.Tmux, Dir: d.Dir, Prompt: d.Prompt, ParentID: d.ParentID,
				Role: d.Role, RunID: e.RunID, Step: d.Step, Headless: d.Headless, State: d.State, CreatedAt: at,
			}
		case KindAgentState:
			var d AgentState
			if a := agents[e.AgentID]; a != nil && json.Unmarshal(e.Data, &d) == nil {
				a.State = d.To
				if d.Cause == CauseReopen {
					a.Reason, a.DoneAt = "", nil
				}
			}
		case KindAgentDone:
			var d AgentDone
			if a := agents[e.AgentID]; a != nil && json.Unmarshal(e.Data, &d) == nil {
				a.State, a.Reason, a.DoneAt, a.Handoff = StateDone, d.Reason, &at, d.Handoff
				if d.State != "" {
					a.State = d.State
				}
				if a.Handoff != nil && a.Handoff.Worktrees == nil {
					a.Handoff.Worktrees = []Worktree{}
				}
			}
		case KindAgentEnded:
			var d Ended
			if a := agents[e.AgentID]; a != nil && json.Unmarshal(e.Data, &d) == nil {
				a.State, a.Reason, a.DoneAt = d.State, d.Reason, &at
			}
		case KindHook:
			var d struct {
				Event string `json:"event"`
			}
			if a := agents[e.AgentID]; a != nil && json.Unmarshal(e.Data, &d) == nil && d.Event != "" {
				a.LastHook, a.LastHookAt = d.Event, &at
			}
		case KindToolUsed:
			if a := agents[e.AgentID]; a != nil {
				a.LastHook, a.LastHookAt = "PostToolUse", &at
			}
		case KindAgentGC:
			if a := agents[e.AgentID]; a != nil {
				a.GCAt = &at
			}
		}
	}
	var snap Snapshot
	for _, id := range runOrder {
		snap.Runs = append(snap.Runs, *runs[id])
	}
	for _, id := range agentOrder {
		snap.Agents = append(snap.Agents, *agents[id])
	}
	return snap
}

// FoldDiff folds every event in s and compares the result with s's tables,
// both read in one transaction so a write landing meanwhile cannot show as
// a difference. It returns one line per difference; none means the log
// rebuilds the tables exactly. Hook times are not compared.
func FoldDiff(s Store) ([]string, error) {
	ss, ok := s.(*sqliteStore)
	if !ok {
		return nil, fmt.Errorf("fold diff needs the sqlite store, not %T", s)
	}
	var diff []string
	err := ss.db.Transaction(func(tx *gorm.DB) error {
		in := &sqliteStore{db: tx}
		var evs []Event
		for after := int64(0); ; {
			page, err := in.Events(EventQuery{After: after, Kinds: ProjectionKinds})
			if err != nil {
				return err
			}
			evs = append(evs, page...)
			if len(page) < DefaultEventLimit {
				break
			}
			after = page[len(page)-1].ID
		}
		agents, err := in.Agents()
		if err != nil {
			return err
		}
		runs, err := in.Runs(RunFilter{})
		if err != nil {
			return err
		}
		diff = compare(Fold(evs), Snapshot{Runs: runs, Agents: agents})
		return nil
	})
	return diff, err
}

func compare(folded, tables Snapshot) []string {
	var diff []string
	runs := map[string]Run{}
	for _, r := range folded.Runs {
		runs[r.ID] = r
	}
	for _, r := range tables.Runs {
		f, ok := runs[r.ID]
		delete(runs, r.ID)
		if !ok {
			diff = append(diff, fmt.Sprintf("run %s: in the tables, not in the log", r.ID))
			continue
		}
		diff = append(diff, fieldDiff("run "+r.ID, normRun(f), normRun(r))...)
	}
	for _, id := range sortedKeys(runs) {
		diff = append(diff, fmt.Sprintf("run %s: in the log, not in the tables", id))
	}
	agents := map[string]Agent{}
	for _, a := range folded.Agents {
		agents[a.ID] = a
	}
	for _, a := range tables.Agents {
		f, ok := agents[a.ID]
		delete(agents, a.ID)
		if !ok {
			diff = append(diff, fmt.Sprintf("agent %s: in the tables, not in the log", a.ID))
			continue
		}
		diff = append(diff, fieldDiff("agent "+a.ID, normAgent(f), normAgent(a))...)
	}
	for _, id := range sortedKeys(agents) {
		diff = append(diff, fmt.Sprintf("agent %s: in the log, not in the tables", id))
	}
	return diff
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// normRun and normAgent make two equal records deeply equal: times in UTC
// without a monotonic reading, nil and empty lists alike, and hook times
// dropped.
func normRun(r Run) Run {
	r.CreatedAt = normTime(r.CreatedAt)
	r.DoneAt, r.AckedAt = normTimeP(r.DoneAt), normTimeP(r.AckedAt)
	steps := make([]Step, len(r.Steps))
	for i, st := range r.Steps {
		st.StartedAt, st.DoneAt = normTimeP(st.StartedAt), normTimeP(st.DoneAt)
		steps[i] = st
	}
	r.Steps = steps
	return r
}

func normAgent(a Agent) Agent {
	a.CreatedAt = normTime(a.CreatedAt)
	a.DoneAt, a.GCAt = normTimeP(a.DoneAt), normTimeP(a.GCAt)
	a.LastHookAt = nil
	if a.Handoff != nil {
		h := *a.Handoff
		h.Worktrees = slices.Clone(h.Worktrees)
		if h.Worktrees == nil {
			h.Worktrees = []Worktree{}
		}
		// Agents does not load parts; the log carries them in agent.done.
		h.Parts = nil
		a.Handoff = &h
	}
	return a
}

func normTime(t time.Time) time.Time { return t.UTC().Round(0) }

func normTimeP(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := normTime(*t)
	return &n
}

// fieldDiff names each top-level field of two structs of one type that
// differs.
func fieldDiff(what string, folded, table any) []string {
	fv, tv := reflect.ValueOf(folded), reflect.ValueOf(table)
	var diff []string
	for i := range fv.NumField() {
		f, t := fv.Field(i).Interface(), tv.Field(i).Interface()
		if !reflect.DeepEqual(f, t) {
			fj, _ := json.Marshal(f)
			tj, _ := json.Marshal(t)
			diff = append(diff, fmt.Sprintf("%s: %s: log %s, tables %s", what, fv.Type().Field(i).Name, fj, tj))
		}
	}
	return diff
}
