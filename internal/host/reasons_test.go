package host

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) agent(t *testing.T, id string) store.Agent {
	t.Helper()
	a, err := f.store.Agent(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// wantEnd checks a run's state and reason, then step i's.
func wantEnd(t *testing.T, r store.Run, state, reason string, i int, stepState, stepReason string) {
	t.Helper()
	if r.State != state || r.Reason != reason {
		t.Fatalf("run %s/%s, want %s/%s", r.State, r.Reason, state, reason)
	}
	if st := r.Steps[i]; st.State != stepState || st.Reason != stepReason {
		t.Fatalf("step %d %s/%s, want %s/%s", i, st.State, st.Reason, stepState, stepReason)
	}
}

func TestDeliveredRunReasons(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	f.done(t, build.ID, "built")
	check := f.stepAgent(t, run.ID, "check")
	if r := f.run(t, run.ID); r.Steps[0].Reason != store.ReasonDelivered || r.Reason != "" || r.Steps[1].Reason != "" {
		t.Fatalf("after step one: %+v", r)
	}
	f.done(t, check.ID, "checked")
	waitFor(t, "run to finish", func() bool { return f.run(t, run.ID).State == store.StateDone })
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonDelivered, 1, store.StateDone, store.ReasonDelivered)
	for _, id := range []string{build.ID, check.ID} {
		if a := f.agent(t, id); a.State != store.StateDone || a.Reason != store.ReasonDelivered {
			t.Fatalf("agent %s: %s/%s", id, a.State, a.Reason)
		}
	}
}

func TestBlockedRunReasons(t *testing.T) {
	f := newFixture(t)
	run, build := f.startHeadless(t, headlessWorkflow)
	f.callDone(t, build.ID, map[string]any{"text": "which?", "outcome": "blocked"})
	waitFor(t, "run to block", func() bool { return f.run(t, run.ID).State == store.StateBlocked })
	wantEnd(t, f.run(t, run.ID), store.StateBlocked, store.ReasonBlocked, 0, store.StateBlocked, store.ReasonBlocked)
	if a := f.agent(t, build.ID); a.State != store.StateDone || a.Reason != store.ReasonBlocked {
		t.Fatalf("agent: %s/%s", a.State, a.Reason)
	}
}

func TestSweepReasons(t *testing.T) {
	for _, c := range []struct {
		name, src           string
		agentState, reason  string
		runState, stepState string
	}{
		{"interactive", testWorkflow, store.StateKilled, store.ReasonLost, store.StateFailed, store.StateFailed},
		{"headless", headlessWorkflow, store.StateFailed, store.ReasonExited, store.StateFailed, store.StateFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			run, build := f.startHeadless(t, c.src)
			f.keeper.vanish(build.TmuxSession)
			f.host.Sweep()
			f.host.Sweep()
			if a := f.agent(t, build.ID); a.State != c.agentState || a.Reason != c.reason {
				t.Fatalf("agent: %s/%s", a.State, a.Reason)
			}
			wantEnd(t, f.run(t, run.ID), c.runState, c.reason, 0, c.stepState, c.reason)
		})
	}
}

func TestKillReasons(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	if code := f.post(t, "/api/agents/"+build.ID+"/kill", nil); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	killed(t, f.keeper)
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonKilled {
		t.Fatalf("agent: %s/%s", a.State, a.Reason)
	}
	wantEnd(t, f.run(t, run.ID), store.StateFailed, store.ReasonKilled, 0, store.StateFailed, store.ReasonKilled)
}

func TestKillRunReasons(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	if _, err := f.host.KillRun(run.ID); err != nil {
		t.Fatal(err)
	}
	killed(t, f.keeper)
	wantEnd(t, f.run(t, run.ID), store.StateKilled, store.ReasonKilled, 0, store.StateKilled, store.ReasonKilled)
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonKilled {
		t.Fatalf("agent: %s/%s", a.State, a.Reason)
	}
}

func TestBadQueuedWorkflowReason(t *testing.T) {
	f := newFixture(t)
	err := f.store.CreateRun(store.Run{ID: "r1", Goal: "g", Workflow: "steps: [", Source: "slack", State: store.StateQueued, Steps: []store.Step{{Name: "a"}}})
	if err != nil {
		t.Fatal(err)
	}
	f.host.AdmitRuns()
	r := f.run(t, "r1")
	if r.State != store.StateFailed || r.Reason != store.ReasonBadWorkflow {
		t.Fatalf("run: %s/%s", r.State, r.Reason)
	}
}

// The dry run in Parse renders with one worktree; the first step gets
// none, so its prompt fails only when the step starts.
func TestStepThatCannotRenderReason(t *testing.T) {
	f := newFixture(t)
	src := `
steps:
  - name: build
    prompt: "Build {{(index .Worktrees 0).Path}}."
`
	var run store.Run
	json.NewDecoder(f.startRun(t, src, "g").Body).Decode(&run)
	waitFor(t, "run to fail", func() bool { return f.run(t, run.ID).State == store.StateFailed })
	wantEnd(t, f.run(t, run.ID), store.StateFailed, store.ReasonStartFailed, 0, store.StateFailed, store.ReasonStartFailed)
}

// A done that lands after the agent was killed must not turn it done.
func TestDoneAfterKillIsRefused(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if code := f.post(t, "/api/agents/"+a.ID+"/kill", nil); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	killed(t, f.keeper)
	res := f.callDone(t, a.ID, map[string]any{"text": "late"})
	if !res.IsError || !strings.Contains(resultText(res), store.ErrFinished.Error()) {
		t.Fatalf("late done: %+v", res)
	}
	got := f.agent(t, a.ID)
	if got.State != store.StateKilled || got.Reason != store.ReasonKilled || got.Handoff != nil {
		t.Fatalf("late done changed the agent: %+v", got)
	}
}

func TestResumedAgentCanCallDone(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if code := f.post(t, "/api/agents/"+a.ID+"/kill", nil); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	killed(t, f.keeper)
	if code := f.post(t, "/api/agents/"+a.ID+"/resume", nil); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	if got := f.agent(t, a.ID); got.State != store.StateIdle || got.Reason != "" {
		t.Fatalf("resumed: %s/%s", got.State, got.Reason)
	}
	f.done(t, a.ID, "finished after all")
	if got := f.agent(t, a.ID); got.State != store.StateDone || got.Reason != store.ReasonDelivered {
		t.Fatalf("done after resume: %s/%s", got.State, got.Reason)
	}
}

func TestEmit(t *testing.T) {
	f := newFixture(t)
	sig, cancel := f.store.Subscribe()
	defer cancel()
	f.host.emit("r1", "a1", "test.thing", map[string]int{"n": 2})
	f.host.emit("", "", "test.empty", nil)
	f.host.emit("", "", "test.bad", func() {}) // cannot marshal: dropped, no panic
	select {
	case <-sig:
	case <-time.After(time.Second):
		t.Fatal("no signal")
	}
	evs, err := f.store.Events(store.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events: %+v", evs)
	}
	if e := evs[0]; e.RunID != "r1" || e.AgentID != "a1" || e.Kind != "test.thing" || string(e.Data) != `{"n":2}` || e.At.IsZero() {
		t.Fatalf("event: %+v %s", e, e.Data)
	}
	if e := evs[1]; e.Kind != "test.empty" || string(e.Data) != `{}` {
		t.Fatalf("empty event: %+v %s", e, e.Data)
	}
}

func TestWorkflowParsedOnce(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	f.stepAgent(t, run.ID, "build")
	w1, err := f.host.wf(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := f.store.Run(run.ID)
	w2, err := f.host.wfOf(r)
	if err != nil {
		t.Fatal(err)
	}
	if w1 != w2 {
		t.Fatal("workflow parsed twice")
	}
	if _, err := f.host.wf("nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown run: %v", err)
	}
	if _, err := f.host.wfOf(store.Run{ID: "bad", Workflow: "steps: ["}); err == nil {
		t.Fatal("bad workflow parsed")
	}
	if _, ok := f.host.wfs.Load("bad"); ok {
		t.Fatal("bad workflow cached")
	}
}
