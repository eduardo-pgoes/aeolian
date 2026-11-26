package host

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type runCtx struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// runContext is the context every start of run runID waits on. It ends when
// the run is killed, and starts already ended for a run killed before the
// host last started.
func (h *Host) runContext(runID string) context.Context {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runCtxLocked(runID).ctx
}

func (h *Host) runCtxLocked(runID string) runCtx {
	if c, ok := h.runCtxs[runID]; ok {
		return c
	}
	if h.runCtxs == nil {
		h.runCtxs = map[string]runCtx{}
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := runCtx{ctx, cancel}
	if run, err := h.Store.Run(runID); err == nil && stopped(run) {
		cancel()
	}
	h.runCtxs[runID] = c
	return c
}

// KillRun records the run killed, drops every start of it still waiting for
// a slot or not yet started, then kills its live agents. Once it returns, no
// agent of the run starts. The run's busy step is killed with it; pending
// steps stay pending. A queued run goes straight to killed. Worktrees are
// left as they are.
func (h *Host) KillRun(id string) (store.Run, error) {
	run, err := h.Store.Run(id)
	if err != nil {
		return store.Run{}, err
	}
	if err := h.stopRun(id, store.End{State: store.StateKilled, Reason: store.ReasonKilled, At: time.Now()}); err != nil {
		if errors.Is(err, store.ErrFinished) {
			return store.Run{}, fmt.Errorf("run %s is %s: %w", id, run.State, err)
		}
		return store.Run{}, err
	}
	return h.Store.Run(id)
}

func (h *Host) stopRun(id string, end store.End) error {
	if err := h.closeRun(id, end); err != nil {
		return err
	}
	run, err := h.Store.Run(id)
	if err != nil {
		return err
	}
	for _, st := range run.Steps {
		if st.State == store.StateBusy {
			h.finishStep(id, st.Index, end)
		}
	}
	h.onFinish(id)

	h.starting.Lock()
	h.mu.Lock()
	h.runCtxLocked(id).cancel()
	e := h.ending[id]
	delete(h.ending, id)
	h.mu.Unlock()
	h.starting.Unlock()
	if e != nil {
		e.close()
	}

	agents, err := h.Store.LiveAgents()
	if err != nil {
		return err
	}
	var errs []error
	for _, a := range agents {
		if a.RunID != id || store.Finished(a.State) {
			continue
		}
		if _, err := h.Kill(a.ID); err != nil && !errors.Is(err, store.ErrFinished) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stopped(run store.Run) bool {
	return run.State == store.StateKilled || run.State == store.StateFailed && run.Reason == store.ReasonBudget
}

func (h *Host) closeStepIfRunClosed(runID string, i int) {
	run, err := h.Store.Run(runID)
	if err != nil {
		log.Printf("run %s: %v", runID, err)
		return
	}
	switch {
	case run.State == store.StateKilled:
		h.killStep(runID, i, time.Now())
	case run.State == store.StateFailed && run.Reason == store.ReasonBudget:
		h.finishStep(runID, i, store.End{State: store.StateFailed, Reason: store.ReasonBudget, At: time.Now()})
	case run.State == store.StateDone && run.Reason == store.ReasonEnded:
		end := store.End{State: store.StateDone, Reason: store.ReasonEnded}
		if err := h.Store.FinishStep(runID, i, end); err != nil && !errors.Is(err, store.ErrFinished) {
			log.Printf("run %s step %d: %v", runID, i, err)
		}
	}
}

func (h *Host) killStep(runID string, i int, at time.Time) {
	h.finishStep(runID, i, store.End{State: store.StateKilled, Reason: store.ReasonKilled, At: at})
}

func (h *Host) finishStep(runID string, i int, end store.End) {
	if err := h.Store.FinishStep(runID, i, end); err != nil && !errors.Is(err, store.ErrFinished) {
		log.Printf("run %s step %d: %v", runID, i, err)
	}
}

func (h *Host) killRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.KillRun(r.PathValue("run"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
