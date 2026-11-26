package host

import (
	"errors"
	"log"
	"slices"

	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

// DefaultMaxRuns is the cap when MaxRuns is unset.
const DefaultMaxRuns = 1

func (h *Host) maxRuns() int {
	if h.MaxRuns > 0 {
		return h.MaxRuns
	}
	return DefaultMaxRuns
}

// createRun stores run, queued when it has a source and the sourced runs
// busy already reach the cap, and starts its first step when it is busy.
func (h *Host) createRun(run store.Run, wf *workflow.Workflow) error {
	h.admitting.Lock()
	defer h.admitting.Unlock()
	run.State = store.StateBusy
	if run.Source != "" {
		runs, err := h.Store.Runs(store.RunFilter{})
		if err != nil {
			return err
		}
		if !h.laneFree(busyByLane(runs, h.MaxRunsBySource), h.lane(run.Source)) {
			run.State = store.StateQueued
		}
	}
	if err := h.Store.CreateRun(run); err != nil {
		return err
	}
	if run.State == store.StateBusy {
		go h.startStep(run.ID, wf, 0, workflow.Data{Goal: run.Goal})
	}
	return nil
}

// AdmitRuns starts queued runs, oldest first, until the sourced runs busy
// reach the cap. Call it whenever a run may have finished, and once when
// the host starts.
func (h *Host) AdmitRuns() {
	h.admitting.Lock()
	defer h.admitting.Unlock()
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		log.Printf("admit runs: %v", err)
		return
	}
	busy := busyByLane(runs, h.MaxRunsBySource)
	slices.Reverse(runs)
	for _, r := range runs {
		if r.State != store.StateQueued {
			continue
		}
		lane := h.lane(r.Source)
		if !h.laneFree(busy, lane) {
			continue
		}
		if err := h.Store.StartQueuedRun(r.ID); err != nil {
			if !errors.Is(err, store.ErrFinished) {
				log.Printf("admit run %s: %v", r.ID, err)
			}
			continue
		}
		wf, err := workflow.Parse([]byte(r.Workflow))
		if err != nil {
			log.Printf("admit run %s: %v", r.ID, err)
			if err := h.Store.FinishRun(r.ID, store.End{State: store.StateFailed, Reason: store.ReasonBadWorkflow}); err != nil {
				log.Printf("admit run %s: %v", r.ID, err)
			} else {
				h.onFinish(r.ID)
			}
			continue
		}
		log.Printf("run %s: starting from the queue", r.ID)
		go h.startStep(r.ID, wf, 0, workflow.Data{Goal: r.Goal})
		busy[lane]++
	}
}

// finishRun closes the run as e says, lets the queue move up, then fires
// on_finish.
func (h *Host) finishRun(runID string, e store.End) error {
	if err := h.closeRun(runID, e); err != nil {
		return err
	}
	h.onFinish(runID)
	return nil
}

// closeRun closes the run as e says and lets the queue move up.
func (h *Host) closeRun(runID string, e store.End) error {
	err := h.Store.FinishRun(runID, e)
	if err == nil {
		h.AdmitRuns()
		go h.tidyRunLater(runID)
	}
	return err
}

func (h *Host) lane(source string) string {
	if _, ok := h.MaxRunsBySource[source]; ok {
		return source
	}
	return ""
}

func (h *Host) laneFree(busy map[string]int, lane string) bool {
	if lane == "" {
		return busy[""] < h.maxRuns()
	}
	return busy[lane] < h.MaxRunsBySource[lane]
}

func busyByLane(runs []store.Run, bySource map[string]int) map[string]int {
	busy := map[string]int{}
	for _, r := range runs {
		if r.Source == "" || r.State != store.StateBusy {
			continue
		}
		if _, ok := bySource[r.Source]; ok {
			busy[r.Source]++
		} else {
			busy[""]++
		}
	}
	return busy
}
