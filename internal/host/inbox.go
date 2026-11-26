package host

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// InboxEntry is a finished run as whoever asked for it picks it up. Step is
// the step that ended the run, and Handoff and Worktrees come from its
// agent's handoff, as do its Parts. Reason says why the run stopped.
// LogTail is set on a failed run only.
type InboxEntry struct {
	RunID      string           `json:"run_id"`
	Source     string           `json:"source"`
	Ref        string           `json:"ref"`
	State      string           `json:"state"`
	Reason     string           `json:"reason"`
	Goal       string           `json:"goal"`
	FinishedAt *time.Time       `json:"finished_at"`
	Step       string           `json:"step"`
	Handoff    string           `json:"handoff"`
	Worktrees  []store.Worktree `json:"worktrees"`
	LogTail    []string         `json:"log_tail"`
	Parts      []store.Part     `json:"parts,omitempty"`
	Tokens     int64            `json:"tokens"`
	TokenCap   int64            `json:"token_cap,omitempty"`
}

// Inbox returns an entry for every finished run not yet acked, oldest
// finished first; a non-empty source keeps only that source's.
func (h *Host) Inbox(source string) ([]InboxEntry, error) {
	runs, err := h.Store.Inbox(source)
	if err != nil {
		return nil, err
	}
	entries := make([]InboxEntry, len(runs))
	for i, run := range runs {
		entries[i] = h.inboxEntry(run)
	}
	return entries, nil
}

func (h *Host) inboxEntry(run store.Run) InboxEntry {
	e := InboxEntry{
		RunID: run.ID, Source: run.Source, Ref: run.Ref, State: run.State, Reason: run.Reason, Goal: run.Goal, FinishedAt: run.DoneAt,
		Worktrees: []store.Worktree{}, LogTail: []string{}, TokenCap: run.TokenCap,
	}
	if tokens, err := h.Store.RunTokens([]string{run.ID}); err != nil {
		log.Printf("inbox: run %s tokens: %v", run.ID, err)
	} else {
		e.Tokens = tokens[run.ID]
	}
	step := endingStep(run)
	if step == nil {
		return e
	}
	e.Step = step.Name
	if step.AgentID == "" {
		return e
	}
	a, err := h.Store.Agent(step.AgentID)
	if err != nil {
		log.Printf("inbox: run %s step %s: %v", run.ID, step.Name, err)
		return e
	}
	if a.Handoff != nil {
		e.Handoff = a.Handoff.Text
		e.Worktrees = append(e.Worktrees, a.Handoff.Worktrees...)
		e.Parts = a.Handoff.Parts
	}
	if run.State == store.StateFailed {
		path := h.Layout.Log(a.ID)
		if a.GCAt != nil {
			path = h.Layout.KeptLog(a.ID)
		}
		lines, err := tailLog(path, logTailLines)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbox: run %s step %s log: %v", run.ID, step.Name, err)
		}
		for _, l := range lines {
			e.LogTail = append(e.LogTail, redact(l))
		}
	}
	return e
}

// endingStep is the last step in the run's own finished state: a run stops
// at the step that blocked, failed or was killed, and a done run at its
// last step.
func endingStep(run store.Run) *store.Step {
	for i := len(run.Steps) - 1; i >= 0; i-- {
		if run.Steps[i].State == run.State {
			return &run.Steps[i]
		}
	}
	return nil
}

// Ack takes a finished run out of the inbox and returns it. Acking it again
// is not an error.
func (h *Host) Ack(id string) (store.Run, error) {
	if err := h.Store.Ack(id, time.Now()); err != nil {
		if errors.Is(err, store.ErrNotFinished) {
			if run, rerr := h.Store.Run(id); rerr == nil {
				return store.Run{}, fmt.Errorf("run %s is %s: %w", id, run.State, err)
			}
		}
		return store.Run{}, err
	}
	return h.Store.Run(id)
}

func (h *Host) listInbox(w http.ResponseWriter, r *http.Request) {
	entries, err := h.Inbox(r.URL.Query().Get("source"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

func (h *Host) ackRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.Ack(r.PathValue("run"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}
