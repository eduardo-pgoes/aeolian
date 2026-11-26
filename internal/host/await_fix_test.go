package host

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// TestNamedAwaitOnQueuedHelper: under the default cap the busy lead and
// helper a fill both slots, so helper b waits for one and has no row yet.
// Naming it in await is still waiting on a helper the lead started.
func TestNamedAwaitOnQueuedHelper(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 2
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	a := f.spawnLater(t, session, "reviewer", "a")
	b := f.spawnLater(t, session, "reviewer", "b")
	f.waitStarted(t, a)
	if _, err := f.store.Agent(b); err == nil {
		t.Fatal("b started over the cap")
	}
	call := f.await(session, map[string]any{"helpers": []string{a, b}})
	// The lead blocked in await frees its slot, so b starts.
	f.waitStarted(t, b)
	f.done(t, a, "result a")
	f.done(t, b, "result b")
	c := <-call
	if c.isErr || !strings.Contains(c.text, "result a") || !strings.Contains(c.text, "result b") {
		t.Fatalf("await: %v %q", c.isErr, c.text)
	}
}

// TestAwaitAfterRestartReturnsUncollected: the mailbox lives in memory, so
// after a host restart a no-name await reads the finished helpers whose
// handoff never reached the lead from the store, once each.
func TestAwaitAfterRestartReturnsUncollected(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 4
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	a := f.spawnLater(t, session, "reviewer", "a")
	b := f.spawnLater(t, session, "reviewer", "b")
	c := f.spawnLater(t, session, "reviewer", "c")
	for _, id := range []string{a, b, c} {
		f.waitStarted(t, id)
	}
	f.done(t, a, "collected before")
	if got := <-f.await(session, map[string]any{"helpers": []string{a}}); !strings.Contains(got.text, "collected before") {
		t.Fatalf("await a: %q", got.text)
	}
	f.done(t, b, "precious result")
	// The restart: the in-memory mailboxes are gone.
	f.host.mu.Lock()
	f.host.mail = nil
	f.host.mu.Unlock()
	call := f.await(session, map[string]any{"mode": "any"})
	got := <-call
	if got.isErr || !strings.Contains(got.text, "precious result") || strings.Contains(got.text, "collected before") {
		t.Fatalf("after restart: %q", got.text)
	}
	if !strings.Contains(got.text, "Still running: reviewer ("+c+")") {
		t.Fatalf("c not outstanding: %q", got.text)
	}
	f.done(t, c, "last one")
	got = <-f.await(session, map[string]any{})
	if strings.Contains(got.text, "precious result") || !strings.Contains(got.text, "last one") {
		t.Fatalf("second await: %q", got.text)
	}
	evs, err := f.store.Events(store.EventQuery{Kinds: []string{store.KindHandoffCollected}})
	if err != nil || len(evs) != 3 {
		t.Fatalf("collected events: %v %d", err, len(evs))
	}
}

// TestAwaitCatchesHelperFinishingDuringSetup: an interactive lead's helper
// that finishes while await is setting up lands in the await, not typed
// into the terminal with the await then hanging until its timeout.
func TestAwaitCatchesHelperFinishingDuringSetup(t *testing.T) {
	for _, named := range []bool{true, false} {
		f := newFixture(t)
		f.host.MaxAgents = 3
		parent := f.start(t)
		session := f.connect(t, parent.ID)
		helper := f.spawnLater(t, session, "reviewer", "x")
		f.waitStarted(t, helper)
		awaitRegistered = func(string) {
			f.host.done(helper, DoneInput{Text: "mid-setup", Worktrees: []WorktreeInput{}})
		}
		args := map[string]any{"timeout_s": 5}
		if named {
			args["helpers"] = []string{helper}
		}
		select {
		case c := <-f.await(session, args):
			if c.isErr || !strings.Contains(c.text, "mid-setup") || strings.Contains(c.text, "Still running") {
				t.Fatalf("named %v: %q", named, c.text)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("named %v: await hung", named)
		}
		awaitRegistered = nil
		select {
		case ty := <-f.keeper.typed:
			t.Fatalf("named %v: typed %q", named, ty.text)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestAwaitWakeDoesNotOverAdmit: with the lead blocked in await any and
// the cap full of helpers, a helper's done wakes the lead; the queued
// helper it frees a slot for must not start too, or max+1 work.
func TestAwaitWakeDoesNotOverAdmit(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 2
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	var ids []string
	for range 4 {
		ids = append(ids, f.spawnLater(t, session, "reviewer", "x"))
	}
	call := f.await(session, map[string]any{"mode": "any"})
	f.waitStarted(t, ids[0])
	f.waitStarted(t, ids[1])
	f.done(t, ids[0], "r0")
	<-call
	time.Sleep(50 * time.Millisecond)
	f.host.mu.Lock()
	n, _ := f.host.liveLocked()
	f.host.mu.Unlock()
	if n > 2 {
		busy, _ := f.store.BusyAgentIDs()
		t.Fatalf("%d working over a cap of 2 (busy %v)", n, busy)
	}
	// The next await frees the lead's slot again.
	call = f.await(session, map[string]any{"mode": "any"})
	f.waitStarted(t, ids[2])
	f.done(t, ids[1], "r1")
	<-call
}

// TestClosingResumedEarlierStepKeepsRun: the build step's lead handed
// back, check runs, and the person resumes build and leaves it idle. The
// watchdog closing it must not finish the run under check.
func TestClosingResumedEarlierStepKeepsRun(t *testing.T) {
	f := newFixture(t)
	f.host.IdleCloseAfter = time.Minute
	f.host.MaxAgents = 5
	resp := f.startRun(t, testWorkflow, "x")
	var run store.Run
	json.NewDecoder(resp.Body).Decode(&run)
	b := f.stepAgent(t, run.ID, "build")
	f.done(t, b.ID, "built")
	c := f.stepAgent(t, run.ID, "check")
	f.keeper.vanish(b.TmuxSession)
	if _, err := f.host.Resume(b.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Hook(b.ID, "Stop", time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	f.host.Sweep()
	if a := f.agent(t, b.ID); a.State != store.StateKilled || a.Reason != store.ReasonClosed {
		t.Fatalf("resumed build %s/%s", a.State, a.Reason)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateBusy || r.Steps[0].State != store.StateDone || r.Steps[0].Reason != store.ReasonDelivered {
		t.Fatalf("run %s/%s, build step %s/%s", r.State, r.Reason, r.Steps[0].State, r.Steps[0].Reason)
	}
	f.done(t, c.ID, "checked")
	waitFor(t, "run done", func() bool { return f.run(t, run.ID).State == store.StateDone })
	if r := f.run(t, run.ID); r.Reason != store.ReasonDelivered {
		t.Fatalf("run %s/%s", r.State, r.Reason)
	}
}
