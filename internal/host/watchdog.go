package host

import (
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const (
	// DefaultStallAfter is StallAfter when unset. The slowest healthy
	// helper seen went 10 minutes between signs of life.
	DefaultStallAfter = 20 * time.Minute
	// DefaultIdleCloseAfter is IdleCloseAfter when unset.
	DefaultIdleCloseAfter = 30 * time.Minute
)

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

// watchdog ends the agents whose session is alive but who stopped making
// progress, so whoever waits on them wakes up:
//   - a busy helper with no hook and no transcript write for StallAfter,
//     whose systemd scope has not used CPU since the sweep before, is
//     stalled (a hung process): its session is killed and it ends
//     killed/lost, its parent getting a handoff that says so. A long
//     build or test run in one tool call shows no hook but burns CPU;
//   - an idle agent that handed back before it was resumed, left at its
//     prompt for IdleCloseAfter, is closed (killed/closed) and keeps that
//     handoff; an agent that never handed back is never closed.
//
// Agents blocked in spawn or await are waiting on helpers, not stalled. A
// witness whose Hold's timeout is at hand is left to the Hold.
// agents, live and pids (session -> pane process) are the sweep's view;
// lost were ended by it already.
func (h *Host) watchdog(now time.Time, agents []store.Agent, live map[string]bool, pids map[string]int, lost []store.Agent) {
	stallAfter := orDefault(h.StallAfter, DefaultStallAfter)
	idleAfter := orDefault(h.IdleCloseAfter, DefaultIdleCloseAfter)
	if stallAfter < 0 && idleAfter < 0 {
		return
	}
	ended := map[string]bool{}
	for _, a := range lost {
		ended[a.ID] = true
	}
	h.mu.Lock()
	blocked := h.blockedLocked()
	h.mu.Unlock()
	quietOnes := map[string]bool{}
	defer h.forgetCPU(quietOnes)
	for _, a := range agents {
		if ended[a.ID] || blocked[a.ID] || !live[a.TmuxSession] || store.Finished(a.State) {
			continue
		}
		switch {
		case a.State == store.StateBusy && a.ParentID != "" && stallAfter > 0:
			quiet := now.Sub(h.lastSign(a, stallAfter, now))
			if quiet < stallAfter {
				continue
			}
			if isWitness(a) && heldByDeadline(a.ID, now) {
				// Its Hold times out about now and stops it, saying so.
				continue
			}
			quietOnes[a.ID] = true
			switch h.cpuState(a.ID, pids[a.TmuxSession], now) {
			case cpuFirst, cpuWorking:
				continue
			}
			why := fmt.Sprintf("the helper stalled: no sign of life for %s, so the host stopped it before it called done", quiet.Round(time.Second))
			h.stop(a, store.End{State: store.StateKilled, Reason: store.ReasonLost, At: now}, h.synthesizeHandoff(a, why))
			h.emit(a.RunID, a.ID, store.KindAgentStalled, store.AgentStalled{V: store.EventVersion, QuietS: int(quiet.Seconds()), Role: a.Role})
		case a.State == store.StateIdle && idleHook(a.LastHook) && idleAfter > 0:
			if a.LastHookAt == nil || now.Sub(*a.LastHookAt) < idleAfter {
				continue
			}
			handoff, ok := h.idleHandoff(a)
			if !ok {
				continue
			}
			h.stop(a, store.End{State: store.StateKilled, Reason: store.ReasonClosed, At: now}, handoff)
			h.emit(a.RunID, a.ID, store.KindAgentIdleClosed, store.AgentIdleClosed{V: store.EventVersion, IdleS: int(now.Sub(*a.LastHookAt).Seconds())})
		}
	}
}

// lastSign is the last time a showed life: its start, its last hook, or,
// when those are older than quiet, its transcript's last write.
func (h *Host) lastSign(a store.Agent, quiet time.Duration, now time.Time) time.Time {
	last := a.CreatedAt
	if a.LastHookAt != nil && a.LastHookAt.After(last) {
		last = *a.LastHookAt
	}
	if now.Sub(last) < quiet {
		return last
	}
	if path := h.transcript(a); path != "" {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().After(last) {
			last = fi.ModTime()
		}
	}
	return last
}

// idleHandoff is the handoff an idle agent closed by the watchdog keeps,
// and whether it should be closed at all: only an agent that handed back
// before it was resumed is. A helper, or a lead, idle without having
// handed back may be talking to a person or waiting on a background job,
// so it stays open.
func (h *Host) idleHandoff(a store.Agent) (store.Handoff, bool) {
	cur, err := h.Store.Agent(a.ID)
	if err != nil || cur.Handoff == nil || cur.Handoff.Synthesized {
		return store.Handoff{}, false
	}
	return *cur.Handoff, true
}

// stop kills a's session and records its end.
func (h *Host) stop(a store.Agent, end store.End, handoff store.Handoff) {
	if err := h.Keeper.Kill(a.TmuxSession); err != nil && !errors.Is(err, keeper.ErrNoSession) {
		log.Printf("watchdog: kill %s: %v", a.ID, err)
		return
	}
	if err := h.markEnded(a, end, handoff); err != nil {
		if !errors.Is(err, store.ErrFinished) {
			log.Printf("watchdog: agent %s: %v", a.ID, err)
		}
		return
	}
	log.Printf("watchdog: agent %s %s; %s (%s)", a.ID, a.Role, end.State, end.Reason)
}
