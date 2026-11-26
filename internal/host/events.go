package host

import (
	"encoding/json"
	"log"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

// emit appends one event of kind to the log, with data marshalled as its
// payload. It never fails its caller: an event that cannot be stored is
// logged and dropped. Like every store write, it must not run under h.mu.
func (h *Host) emit(runID, agentID, kind string, data any) {
	var raw json.RawMessage
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			log.Printf("event %s: %v", kind, err)
			return
		}
		raw = b
	}
	err := h.Store.Append(store.Event{At: time.Now(), RunID: runID, AgentID: agentID, Kind: kind, Data: raw})
	if err != nil {
		log.Printf("event %s: %v", kind, err)
	}
}

// wf returns run runID's workflow, parsed once and then cached: a run's
// workflow never changes after it is stored. A *Workflow is safe to share,
// since its templates execute concurrently.
func (h *Host) wf(runID string) (*workflow.Workflow, error) {
	if w, ok := h.wfs.Load(runID); ok {
		return w.(*workflow.Workflow), nil
	}
	run, err := h.Store.Run(runID)
	if err != nil {
		return nil, err
	}
	return h.wfOf(run)
}

// wfOf is wf for a run the caller has loaded already. A workflow that does
// not parse is not cached, so each call reports the error again.
func (h *Host) wfOf(run store.Run) (*workflow.Workflow, error) {
	if w, ok := h.wfs.Load(run.ID); ok {
		return w.(*workflow.Workflow), nil
	}
	w, err := workflow.Parse([]byte(run.Workflow))
	if err != nil {
		return nil, err
	}
	actual, _ := h.wfs.LoadOrStore(run.ID, w)
	return actual.(*workflow.Workflow), nil
}

// agentEnded runs once an agent's end is stored: after done, and after
// markEnded records it killed or failed. a is the agent as it was before
// it ended. It is the one place later work hooks into an agent's end; it
// reads the last token use from the agent's transcript, in the background.
// Like emit, it must not run under h.mu.
func (h *Host) agentEnded(a store.Agent) {
	h.launchBlocked.Delete(a.ID)
	go h.tailUsage(a.ID)
}
