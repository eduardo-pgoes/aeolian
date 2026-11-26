package host

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// vanish ends name's session behind the host's back.
func (k *fakeKeeper) vanish(name string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.dead == nil {
		k.dead = map[string]bool{}
	}
	k.dead[name] = true
}

func (k *fakeKeeper) failList(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.listErr = err
}

func (f *fixture) state(t *testing.T, id string) string {
	t.Helper()
	a, err := f.store.Agent(id)
	if err != nil {
		t.Fatal(err)
	}
	return a.State
}

func noKill(t *testing.T, k *fakeKeeper) {
	t.Helper()
	select {
	case name := <-k.killed:
		t.Fatalf("killed %q through tmux", name)
	default:
	}
}

func TestSweepKillsStepAgentMissingTwice(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	queued := make(chan store.Agent, 1)
	go func() { queued <- f.start(t) }()
	waitFor(t, "start to queue", func() bool { return f.host.queued() == 1 })

	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	if s := f.state(t, build.ID); s == store.StateKilled {
		t.Fatalf("killed after one sweep")
	}
	if f.host.queued() != 1 {
		t.Fatalf("queued start admitted after one sweep")
	}

	f.host.Sweep()
	if s := f.state(t, build.ID); s != store.StateKilled {
		t.Fatalf("after two sweeps: %s", s)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateFailed || r.Steps[0].State != store.StateFailed || r.Steps[1].State != store.StatePending {
		t.Fatalf("run after sweep: %+v", r)
	}
	<-queued
	noKill(t, f.keeper)
}

func TestSweepHandsParentLostHelper(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	wt := store.Worktree{Repo: "/r", Path: t.TempDir(), Branch: "b"}
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "reviewer", wt)
	helper := f.helperOf(t, parent.ID)

	f.keeper.vanish(helper.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	c := <-call
	if c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), "Helper reviewer ended without calling done:\n\n(the session died while working, before it called done and left no message)") {
		t.Fatalf("spawn after lost helper: %v %+v", c.err, c.res)
	}
	if s := f.state(t, helper.ID); s != store.StateKilled {
		t.Fatalf("helper: %s", s)
	}
	if s := f.state(t, parent.ID); s == store.StateKilled {
		t.Fatalf("parent killed")
	}
}

func TestSweepSparesBriefMiss(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.keeper.vanish(a.TmuxSession)
	f.host.Sweep()
	f.keeper.mu.Lock()
	delete(f.keeper.dead, a.TmuxSession)
	f.keeper.mu.Unlock()
	f.host.Sweep()
	f.keeper.vanish(a.TmuxSession)
	f.host.Sweep()
	if s := f.state(t, a.ID); s == store.StateKilled {
		t.Fatalf("killed on misses that were not consecutive")
	}
}

func TestSweepDoesNothingWhenListFails(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.keeper.vanish(a.TmuxSession)
	f.keeper.failList(errors.New("tmux broke"))
	f.host.Sweep()
	f.host.Sweep()
	f.host.Sweep()
	if s := f.state(t, a.ID); s == store.StateKilled {
		t.Fatalf("killed while List failed")
	}
}

func TestSweepSkipsFinishedAgents(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.done(t, a.ID, "ok")
	killed(t, f.keeper)
	f.host.Sweep()
	f.host.Sweep()
	got, _ := f.store.Agent(a.ID)
	if got.State != store.StateDone || got.Handoff == nil || got.Handoff.Text != "ok" {
		t.Fatalf("done agent after sweeps: %+v", got)
	}
}
