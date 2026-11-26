package host

import (
	"context"
	"log"
	"slices"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultMaxAgents is the cap when MaxAgents is unset.
const DefaultMaxAgents = 2

func (h *Host) maxAgents() int {
	if h.MaxAgents > 0 {
		return h.MaxAgents
	}
	return DefaultMaxAgents
}

// acquire waits first-come-first-served until a start of agent agentID, in
// run runID if any, fits under the cap and reserves it. The caller must
// call release once the agent's row is stored or its start has failed.
func (h *Host) acquire(ctx context.Context, runID, agentID string) error {
	h.mu.Lock()
	if len(h.queue) == 0 {
		n, err := h.liveLocked()
		if err != nil {
			h.mu.Unlock()
			return err
		}
		if n < h.maxAgents() {
			h.reserved++
			h.mu.Unlock()
			return nil
		}
	}
	admitted := make(chan struct{})
	h.queue = append(h.queue, admitted)
	queued := len(h.queue)
	h.mu.Unlock()
	h.emit(runID, agentID, store.KindCapWaiting, store.CapData{V: store.EventVersion, QueueLen: queued})

	select {
	case <-admitted:
		h.mu.Lock()
		queued = len(h.queue)
		h.mu.Unlock()
		h.emit(runID, agentID, store.KindCapAdmitted, store.CapData{V: store.EventVersion, QueueLen: queued})
		return nil
	case <-ctx.Done():
		h.mu.Lock()
		if i := slices.Index(h.queue, admitted); i >= 0 {
			h.queue = slices.Delete(h.queue, i, i+1)
			h.mu.Unlock()
			return ctx.Err()
		}
		h.reserved--
		h.mu.Unlock()
		h.admit()
		return ctx.Err()
	}
}

func (h *Host) release() {
	h.mu.Lock()
	h.reserved--
	h.mu.Unlock()
	h.admit()
}

// admit lets queued starts through, oldest first, while there is room. Call
// it whenever a slot may have freed.
func (h *Host) admit() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for len(h.queue) > 0 {
		n, err := h.liveLocked()
		if err != nil {
			log.Printf("admit: %v", err)
			return
		}
		if n >= h.maxAgents() {
			return
		}
		h.reserved++
		close(h.queue[0])
		h.queue = h.queue[1:]
	}
}

// liveLocked counts the agents that hold a slot: stored agents that are
// busy and not blocked in spawn or await, plus starts admitted but not
// stored yet. An idle agent, at a prompt waiting for a person, holds none.
func (h *Host) liveLocked() (int, error) {
	ids, err := h.Store.BusyAgentIDs()
	if err != nil {
		return 0, err
	}
	blocked := h.blockedLocked()
	n := h.reserved
	for _, id := range ids {
		if !blocked[id] {
			n++
		}
	}
	return n, nil
}

// blockedLocked is the set of agents waiting on helpers, in spawn or await.
func (h *Host) blockedLocked() map[string]bool {
	blocked := map[string]bool{}
	for _, w := range h.spawns {
		blocked[w.parent] = true
	}
	for parent, mb := range h.mail {
		if mb.blockedLocked() {
			blocked[parent] = true
		}
	}
	return blocked
}

// idleHook reports whether event means the agent ended its turn and waits
// for a person.
func idleHook(event string) bool {
	switch event {
	case "Stop", "Notification", "SessionEnd":
		return true
	}
	return false
}
