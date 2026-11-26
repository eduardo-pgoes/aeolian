package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const DefaultCapKills = 3

func checkCap(contextCap int64, capKills int) (int, error) {
	switch {
	case contextCap < 0:
		return 0, errors.New("context_cap: must not be negative")
	case capKills < 0:
		return 0, errors.New("cap_kills: must not be negative")
	case capKills > 0 && contextCap == 0:
		return 0, errors.New("cap_kills: needs a context_cap")
	case contextCap > 0 && capKills == 0:
		return DefaultCapKills, nil
	}
	return capKills, nil
}

func (h *Host) enforceCap(agentID string, level int64) {
	a, err := h.Store.Agent(agentID)
	if err != nil {
		log.Printf("context cap %s: %v", agentID, err)
		return
	}
	if store.Finished(a.State) || !a.Headless || a.RunID == "" {
		return
	}
	run, err := h.Store.Run(a.RunID)
	if err != nil {
		log.Printf("context cap %s: %v", a.ID, err)
		return
	}
	if run.ContextCap <= 0 || level <= run.ContextCap || h.reopened(a.ID) {
		return
	}
	why := fmt.Sprintf("the agent reached its context cap (%d of %d tokens) and the host stopped it before it called done", level, run.ContextCap)
	if a.ParentID != "" {
		why = fmt.Sprintf("the helper reached its context cap (%d of %d tokens) and the host stopped it before it called done; spawn a fresh helper with this handoff to pick up the work", level, run.ContextCap)
	}
	now := time.Now()
	h.stop(a, store.End{State: store.StateFailed, Reason: store.ReasonBudget, At: now}, h.synthesizeHandoff(a, why))
	if a.ParentID == "" || isWitness(a) {
		return
	}
	kills, err := h.capKills(run.ID)
	if err != nil {
		log.Printf("context cap run %s: %v", run.ID, err)
		return
	}
	if kills < run.CapKills {
		return
	}
	log.Printf("run %s: %d helpers reached the context cap, ending the run", run.ID, kills)
	if err := h.stopRun(run.ID, store.End{State: store.StateFailed, Reason: store.ReasonBudget, At: now}); err != nil && !errors.Is(err, store.ErrFinished) {
		log.Printf("run %s: %v", run.ID, err)
	}
}

func (h *Host) enforceTokenCap(runID string) {
	if runID == "" {
		return
	}
	run, err := h.Store.Run(runID)
	if err != nil {
		log.Printf("token cap run %s: %v", runID, err)
		return
	}
	if run.TokenCap <= 0 || run.State != store.StateBusy || h.handedBack(run) {
		return
	}
	tokens, err := h.Store.RunTokens([]string{run.ID})
	if err != nil {
		log.Printf("token cap run %s: %v", run.ID, err)
		return
	}
	reopened, err := h.Store.ReopenedTokens(run.ID)
	if err != nil {
		log.Printf("token cap run %s: %v", run.ID, err)
		return
	}
	used := tokens[run.ID] - reopened
	if used <= run.TokenCap {
		return
	}
	log.Printf("run %s: used %d of its %d token cap, ending the run", run.ID, used, run.TokenCap)
	if err := h.stopRun(run.ID, store.End{State: store.StateFailed, Reason: store.ReasonBudget, At: time.Now()}); err != nil && !errors.Is(err, store.ErrFinished) {
		log.Printf("run %s: %v", run.ID, err)
	}
}

func (h *Host) handedBack(run store.Run) bool {
	for _, st := range run.Steps {
		if st.AgentID == "" {
			continue
		}
		a, err := h.Store.Agent(st.AgentID)
		if err != nil {
			log.Printf("token cap run %s: %v", run.ID, err)
			continue
		}
		if a.State != store.StateDone {
			continue
		}
		if a.Reason == store.ReasonBlocked || (a.Reason == store.ReasonDelivered && st.Index == len(run.Steps)-1) {
			return true
		}
	}
	return false
}

func (h *Host) capKills(runID string) (int, error) {
	agents, err := h.Store.AgentsOfRuns([]string{runID})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, a := range agents {
		if a.ParentID != "" && !isWitness(a) && a.State == store.StateFailed && a.Reason == store.ReasonBudget {
			n++
		}
	}
	return n, nil
}

func (h *Host) reopened(agentID string) bool {
	h.hooks.mu.Lock()
	v, ok := h.hooks.reopened[agentID]
	h.hooks.mu.Unlock()
	if ok {
		return v
	}
	q := store.EventQuery{AgentID: agentID, Kinds: []string{store.KindAgentState}}
	for !v {
		evs, err := h.Store.Events(q)
		if err != nil {
			log.Printf("context cap %s: %v", agentID, err)
			return true
		}
		for _, e := range evs {
			var d store.AgentState
			if json.Unmarshal(e.Data, &d) == nil && d.Cause == store.CauseReopen {
				v = true
			}
		}
		if len(evs) < store.DefaultEventLimit {
			break
		}
		q.After = evs[len(evs)-1].ID
	}
	h.hooks.mu.Lock()
	defer h.hooks.mu.Unlock()
	if h.hooks.reopened == nil {
		h.hooks.reopened = map[string]bool{}
	}
	if _, ok := h.hooks.reopened[agentID]; !ok {
		h.hooks.reopened[agentID] = v
	}
	return h.hooks.reopened[agentID]
}

func (h *Host) markReopened(agentID string) {
	h.hooks.mu.Lock()
	defer h.hooks.mu.Unlock()
	if h.hooks.reopened == nil {
		h.hooks.reopened = map[string]bool{}
	}
	h.hooks.reopened[agentID] = true
}
