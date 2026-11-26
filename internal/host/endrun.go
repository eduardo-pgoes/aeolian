package host

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultEndGrace is how long EndRun waits for an interactive lead to hand
// back when EndGrace is unset.
const DefaultEndGrace = 90 * time.Second

// ErrEnding is returned when ending a run that is being ended already.
var ErrEnding = errors.New("already ending")

// endMessage is typed into the lead of a run being ended.
const endMessage = "The person ended this run. Call done now with a short summary of what was done and every worktree that holds work. Do not start anything new."

// ending is a run being ended. done is closed once the run has finished.
type ending struct {
	by   string
	once sync.Once
	done chan struct{}
}

func (e *ending) close() { e.once.Do(func() { close(e.done) }) }

func (h *Host) endGrace() time.Duration {
	if h.EndGrace > 0 {
		return h.EndGrace
	}
	return DefaultEndGrace
}

// isEnding reports whether run runID is being ended.
func (h *Host) isEnding(runID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.ending[runID]
	return ok
}

// endingOf returns the ending of run runID, or nil.
func (h *Host) endingOf(runID string) *ending {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ending[runID]
}

// dropEnding forgets e, if it is still run runID's ending, and wakes its
// waiter.
func (h *Host) dropEnding(runID string, e *ending) {
	h.mu.Lock()
	if h.ending[runID] == e {
		delete(h.ending, runID)
	}
	h.mu.Unlock()
	e.close()
}

// EndRun closes a busy run as done for reason ended: the person, or its
// lead, says the work is over. Unless force, an interactive lead is first
// asked to hand back and given EndGrace to call done; a headless, dead or
// silent lead is killed and the host writes its handoff from its last
// message. Helpers are killed without their handoffs being typed in, and no
// later step starts. A queued run goes straight to done. by says who asked.
func (h *Host) EndRun(id, by string, force bool) (store.Run, error) {
	e, err := h.beginEnd(id, by)
	if err != nil || e == nil {
		return h.runOrErr(id, err)
	}
	h.finishEnd(id, e, force)
	return h.Store.Run(id)
}

func (h *Host) runOrErr(id string, err error) (store.Run, error) {
	if err != nil {
		return store.Run{}, err
	}
	return h.Store.Run(id)
}

// beginEnd checks run id can end and marks it ending: no start of it waits
// on a slot or begins after, and its helpers are killed. It returns a nil
// ending for a queued run, which it has finished already.
func (h *Host) beginEnd(id, by string) (*ending, error) {
	run, err := h.Store.Run(id)
	if err != nil {
		return nil, err
	}
	if run.State == store.StateQueued {
		err := h.finishRun(id, store.End{State: store.StateDone, Reason: store.ReasonEnded})
		if err == nil {
			h.emit(id, "", store.KindRunEnding, store.RunEnding{V: store.EventVersion, By: by})
			h.starting.Lock()
			h.mu.Lock()
			h.runCtxLocked(id).cancel()
			h.mu.Unlock()
			h.starting.Unlock()
			return nil, nil
		}
		if !errors.Is(err, store.ErrFinished) {
			return nil, err
		}
		run, _ = h.Store.Run(id)
	}
	if run.State != store.StateBusy {
		return nil, fmt.Errorf("run %s is %s: %w", id, run.State, store.ErrFinished)
	}
	e := &ending{by: by, done: make(chan struct{})}
	h.mu.Lock()
	if _, ok := h.ending[id]; ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("run %s: %w", id, ErrEnding)
	}
	if h.ending == nil {
		h.ending = map[string]*ending{}
	}
	h.ending[id] = e
	h.mu.Unlock()
	h.emit(id, "", store.KindRunEnding, store.RunEnding{V: store.EventVersion, By: by})

	h.starting.Lock()
	h.mu.Lock()
	h.runCtxLocked(id).cancel()
	h.mu.Unlock()
	h.starting.Unlock()

	live, err := h.Store.LiveAgents()
	if err != nil {
		log.Printf("end run %s: %v", id, err)
		return e, nil
	}
	for _, a := range live {
		if a.RunID != id || a.ParentID == "" {
			continue
		}
		if err := h.Keeper.Kill(a.TmuxSession); err != nil && !errors.Is(err, keeper.ErrNoSession) {
			log.Printf("end run %s: kill helper %s: %v", id, a.ID, err)
		}
		err := h.markEnded(a, store.End{State: store.StateKilled, Reason: store.ReasonEnded}, endedHandoff)
		if err != nil && !errors.Is(err, store.ErrFinished) {
			log.Printf("end run %s: helper %s: %v", id, a.ID, err)
		}
	}
	return e, nil
}

// endedHandoff is what a helper killed by EndRun hands back: written by
// the host, so its parent reads that it ended without calling done.
var endedHandoff = store.Handoff{Text: "The run was ended before the helper called done; it did not finish.", Worktrees: []store.Worktree{}, Synthesized: true}

// finishEnd asks the lead to hand back, unless force, then closes the run.
func (h *Host) finishEnd(id string, e *ending, force bool) {
	defer h.dropEnding(id, e)
	lead, ok := h.liveLead(id)
	if ok && !force && !lead.Headless {
		h.tell(lead.ID, endMessage)
		select {
		case <-e.done:
			return
		case <-time.After(h.endGrace()):
		}
		if h.endingOf(id) != e {
			return // KillRun took the run over
		}
		lead, ok = h.liveLead(id)
	}
	now := time.Now()
	end := store.End{State: store.StateDone, Reason: store.ReasonEnded, At: now}
	if ok {
		if err := h.Keeper.Kill(lead.TmuxSession); err != nil && !errors.Is(err, keeper.ErrNoSession) {
			log.Printf("end run %s: kill lead %s: %v", id, lead.ID, err)
		}
		handoff := h.synthesizeHandoff(lead, "the run was ended before its lead called done")
		err := h.Store.EndAgentHandoff(lead.ID, store.End{State: store.StateKilled, Reason: store.ReasonEnded, At: now}, handoff)
		switch {
		case err == nil:
			h.agentEnded(lead)
			h.forgetMailbox(lead.ID)
			h.admit()
		case !errors.Is(err, store.ErrFinished):
			log.Printf("end run %s: lead %s: %v", id, lead.ID, err)
		}
	}
	run, err := h.Store.Run(id)
	if err != nil {
		log.Printf("end run %s: %v", id, err)
		return
	}
	for _, st := range run.Steps {
		if st.State == store.StateBusy {
			if err := h.Store.FinishStep(id, st.Index, end); err != nil && !errors.Is(err, store.ErrFinished) {
				log.Printf("end run %s step %s: %v", id, st.Name, err)
			}
		}
	}
	if err := h.finishRun(id, end); err != nil && !errors.Is(err, store.ErrFinished) {
		log.Printf("end run %s: %v", id, err)
	}
}

// liveLead returns run id's live step agent, if it has one.
func (h *Host) liveLead(id string) (store.Agent, bool) {
	live, err := h.Store.LiveAgents()
	if err != nil {
		log.Printf("end run %s: %v", id, err)
		return store.Agent{}, false
	}
	for _, a := range live {
		if a.RunID == id && a.ParentID == "" {
			return a, true
		}
	}
	return store.Agent{}, false
}

// endRun answers 202 with the run as it is once the end has begun; the
// lead's grace and the close happen after.
func (h *Host) endRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true"
	e, err := h.beginEnd(id, "person")
	if err != nil {
		httpError(w, err)
		return
	}
	if e != nil {
		go h.finishEnd(id, e, force)
	}
	run, err := h.Store.Run(id)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, run)
}
