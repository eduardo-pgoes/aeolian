package host

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// SweepInterval is how often Watch compares agents with the keeper's
// sessions.
const SweepInterval = 5 * time.Second

// Watch picks up held dones, then sweeps once, then every SweepInterval
// until ctx ends. It prunes old event payloads on the first sweep and every
// PruneInterval after. With AutoGC it also collects garbage, now and every
// GCInterval, and returns only once a collection under way has finished.
func (h *Host) Watch(ctx context.Context) {
	h.ResumeDone()
	if h.AutoGC {
		collected := make(chan struct{})
		go func() { h.gcLoop(ctx); close(collected) }()
		defer func() { <-collected }()
	}
	t := time.NewTicker(SweepInterval)
	defer t.Stop()
	var pruned time.Time
	for {
		h.Sweep()
		if now := time.Now(); now.Sub(pruned) >= PruneInterval {
			h.PruneEvents(now)
			pruned = now
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// Sweep records killed every unfinished agent whose session was missing on
// this sweep and the one before, or failed if the agent is headless: its
// session ends with its process, so it exited without calling done. An
// interactive agent that was idle, at its prompt, was closed by its person
// (reason closed): a step's agent then closes its run as done. One that was
// busy was lost. Either way the host writes the agent's handoff from its
// last message. One miss is not enough: launch stores an agent before its
// session starts, and resume starts the session before it reopens the
// agent. A sweep that cannot list sessions changes nothing. Then the
// watchdog looks for stalled and long-idle agents.
func (h *Host) Sweep() {
	sessions, err := h.Keeper.List()
	if err != nil {
		log.Printf("sweep: list sessions: %v", err)
		return
	}
	agents, err := h.Store.LiveAgents()
	if err != nil {
		log.Printf("sweep: %v", err)
		return
	}
	live := map[string]bool{}
	pids := map[string]int{}
	for _, s := range sessions {
		live[s.Name] = true
		pids[s.Name] = s.PID
	}
	missing := map[string]bool{}
	var lost []store.Agent
	h.mu.Lock()
	for _, a := range agents {
		if store.Finished(a.State) || live[a.TmuxSession] {
			continue
		}
		missing[a.ID] = true
		if h.missing[a.ID] {
			lost = append(lost, a)
		}
	}
	h.missing = missing
	h.mu.Unlock()
	for _, a := range lost {
		end, why := sweepEnd(a)
		if unit, ok := h.oomKilled(a.ID); ok {
			end, why = h.oomEnd(a, unit)
		}
		err := h.markEnded(a, end, h.synthesizeHandoff(a, why))
		if errors.Is(err, store.ErrFinished) {
			continue
		}
		if err != nil {
			log.Printf("sweep: agent %s: %v", a.ID, err)
			continue
		}
		log.Printf("sweep: agent %s lost its session %s; %s (%s)", a.ID, a.TmuxSession, end.State, end.Reason)
	}
	h.noteScopes(agents, pids, lost)
	now := time.Now()
	h.watchdog(now, agents, live, pids, lost)
	h.launchWatch(now, agents, live)
}

// sweepEnd is how an agent whose session vanished ends, and why, as its
// synthesized handoff says.
func sweepEnd(a store.Agent) (store.End, string) {
	switch {
	case a.Headless:
		return store.End{State: store.StateFailed, Reason: store.ReasonExited}, "the session exited before it called done"
	// Idle by state: a Notification mid-turn (a permission prompt) leaves
	// the agent busy, and its session dying then is lost work. SessionEnd
	// is the person closing the session.
	case a.State == store.StateIdle || a.LastHook == "SessionEnd":
		return store.End{State: store.StateKilled, Reason: store.ReasonClosed}, "the session was closed while idle, without done"
	}
	return store.End{State: store.StateKilled, Reason: store.ReasonLost}, "the session died while working, before it called done"
}

// ResumeDone moves on every run whose step agent delivered but whose run
// never moved on, because the host stopped while a gate held it or before
// advance ran. Gates hold it again first, so a Hold must cope with being
// run twice for one done.
func (h *Host) ResumeDone() {
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		log.Printf("resume done: %v", err)
		return
	}
	for _, run := range runs {
		if run.State != store.StateBusy {
			continue
		}
		for _, st := range run.Steps {
			if st.State != store.StateBusy || st.AgentID == "" {
				continue
			}
			a, err := h.Store.Agent(st.AgentID)
			// Only a lead advances its run; an arena's step agents have the
			// arena as parent, and the arena drives its own steps.
			if err != nil || a.ParentID != "" || a.State != store.StateDone || a.Handoff == nil {
				continue
			}
			if a.Reason != store.ReasonDelivered && a.Reason != store.ReasonBlocked {
				continue
			}
			outcome := OutcomeDone
			if a.Reason == store.ReasonBlocked {
				outcome = OutcomeBlocked
			}
			log.Printf("resume done: run %s step %s", run.ID, st.Name)
			go func() {
				if h.holdDone(DoneCall{Agent: a, Handoff: *a.Handoff, Outcome: outcome}) {
					h.advance(a, *a.Handoff, outcome == OutcomeBlocked)
				}
			}()
		}
	}
}
