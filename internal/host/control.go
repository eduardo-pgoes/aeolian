package host

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// ErrLive is returned when resuming an agent whose session has not ended.
var ErrLive = errors.New("still running")

var ErrDirGone = errors.New("its folder no longer exists")

// killedHandoff is what a killed helper hands back to its parent.
var killedHandoff = store.Handoff{Text: "The helper was killed before it called done; it did not finish."}

// Kill ends a live agent's session and records it killed; a helper's parent
// gets killedHandoff in place of the helper's own.
func (h *Host) Kill(id string) (store.Agent, error) {
	a, err := h.Store.Agent(id)
	if err != nil {
		return store.Agent{}, err
	}
	if store.Finished(a.State) {
		return store.Agent{}, fmt.Errorf("agent %s is %s: %w", id, a.State, store.ErrFinished)
	}
	if err := h.Keeper.Kill(a.TmuxSession); err != nil && !errors.Is(err, keeper.ErrNoSession) {
		return store.Agent{}, err
	}
	if err := h.markEnded(a, store.End{State: store.StateKilled, Reason: store.ReasonKilled}, killedHandoff); err != nil {
		return store.Agent{}, err
	}
	return h.Store.Agent(id)
}

// markEnded records a, whose session has ended, as end says: killed or
// failed, and why. A synthesized handoff is stored as a's own. A step's
// agent closed by its person (reason closed) closes its step and run as
// done; any other end fails them for the same reason. A helper's parent
// gets handoff.
func (h *Host) markEnded(a store.Agent, end store.End, handoff store.Handoff) error {
	if end.At.IsZero() {
		end.At = time.Now()
	}
	keep := handoff.Synthesized
	if keep {
		// A resumed agent keeps the handoff it gave before.
		if cur, err := h.Store.Agent(a.ID); err == nil && cur.Handoff != nil && !cur.Handoff.Synthesized {
			keep = false
		}
	}
	var err error
	if keep {
		err = h.Store.EndAgentHandoff(a.ID, end, handoff)
	} else {
		err = h.Store.EndAgent(a.ID, end)
	}
	if err != nil {
		return err
	}
	h.agentEnded(a)
	h.forgetMailbox(a.ID)
	if a.ParentID == "" && a.RunID != "" {
		if end.Reason == store.ReasonClosed {
			h.closeStepOf(a, end.At)
		} else {
			h.failStepOf(a, store.End{State: store.StateFailed, Reason: end.Reason, At: end.At})
		}
	}
	if a.ParentID != "" {
		h.helperDone(a, handoff)
	}
	h.admit()
	return nil
}

// Resume starts a finished agent's Claude Code session again in its old
// folder and makes it idle. A human asked for it, so it does not wait for a
// slot under the cap.
func (h *Host) Resume(id string) (store.Agent, error) {
	a, err := h.Store.Agent(id)
	if err != nil {
		return store.Agent{}, err
	}
	if !store.Finished(a.State) {
		return store.Agent{}, fmt.Errorf("agent %s is %s: %w", id, a.State, ErrLive)
	}
	if a.GCAt != nil {
		return store.Agent{}, fmt.Errorf("agent %s cannot be resumed: %w", id, ErrGone)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	setup, err := h.setup(ctx, a, false)
	if err != nil {
		return store.Agent{}, fmt.Errorf("resume agent %s: %w", id, err)
	}
	lay, err := h.agentLayout(a)
	if err != nil {
		return store.Agent{}, fmt.Errorf("resume agent %s: %w", id, err)
	}
	if a.Dir != lay.ScratchDir(a.ID) {
		if fi, err := os.Stat(a.Dir); err != nil || !fi.IsDir() {
			return store.Agent{}, fmt.Errorf("agent %s cannot be resumed in %s: %w", id, a.Dir, ErrDirGone)
		}
	}
	if err := lay.Prepare(a.ID, a.Dir, setup); err != nil {
		return store.Agent{}, err
	}
	access, err := lay.ConnectorDeny(ctx, a.Dir, setup)
	if err != nil {
		return store.Agent{}, fmt.Errorf("resume agent %s: %w", id, err)
	}
	logSkipped(a.ID, access)
	h.markReopened(a.ID)
	argv := lay.Argv(agent.Launch{AgentID: a.ID, SessionID: a.SessionID, Resume: true, Model: h.model(a), Memory: h.memory(a), Boot: setup.Boot, Connectors: access.On, Deny: access.Deny})
	if err := h.Keeper.Start(a.TmuxSession, a.Dir, argv); err != nil {
		return store.Agent{}, fmt.Errorf("resume agent %s: %w", id, err)
	}
	if err := h.Store.Reopen(id); err != nil {
		return store.Agent{}, err
	}
	return h.Store.Agent(id)
}

func (h *Host) killAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.Kill(r.PathValue("agent"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

type ResumeResponse struct {
	TmuxSession string `json:"tmux_session"`
}

func (h *Host) resumeAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.Resume(r.PathValue("agent"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ResumeResponse{TmuxSession: a.TmuxSession})
}
