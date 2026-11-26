package host

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const oneStepWorkflow = `
steps:
  - name: build
    prompt: "Build {{.Goal}}."
`

func (f *fixture) postRunOK(t *testing.T, req RunRequest) store.Run {
	t.Helper()
	resp := f.postRun(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start %+v: %s", req, resp.Status)
	}
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *fixture) runAgents(t *testing.T, runID string) int {
	t.Helper()
	agents, err := f.store.Agents()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range agents {
		if a.RunID == runID {
			n++
		}
	}
	return n
}

func TestSourcedRunsQueueOverMaxRuns(t *testing.T) {
	f := newFixture(t)
	f.host.MaxRuns = 1
	first := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "slack"})
	if first.State != store.StateBusy {
		t.Fatalf("first run: %+v", first)
	}
	build := f.stepAgent(t, first.ID, "build")

	second := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "slack"})
	if second.State != store.StateQueued {
		t.Fatalf("second run: %+v", second)
	}
	plain := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "c"})
	if plain.State != store.StateBusy {
		t.Fatalf("run without a source: %+v", plain)
	}
	f.stepAgent(t, plain.ID, "build")
	third := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "d", Source: "cli"})
	if third.State != store.StateQueued {
		t.Fatalf("third run: %+v", third)
	}

	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, second.ID); n != 0 {
		t.Fatalf("queued run started %d agents", n)
	}

	f.done(t, build.ID, "built")
	waitFor(t, "first run to finish", func() bool { return f.run(t, first.ID).State == store.StateDone })
	f.stepAgent(t, second.ID, "build")
	if r := f.run(t, second.ID); r.State != store.StateBusy {
		t.Fatalf("second run after the first finished: %+v", r)
	}
	if r := f.run(t, third.ID); r.State != store.StateQueued {
		t.Fatalf("third run while the second is busy: %+v", r)
	}
}

func TestKillQueuedRun(t *testing.T) {
	f := newFixture(t)
	f.host.MaxRuns = 1
	first := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "slack"})
	f.stepAgent(t, first.ID, "build")
	queued := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "slack"})

	resp := f.killRun(t, queued.ID)
	var got store.Run
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || got.State != store.StateKilled {
		t.Fatalf("kill queued run: %s %v %+v", resp.Status, err, got)
	}
	if got.DoneAt == nil || got.Steps[0].State != store.StatePending {
		t.Fatalf("killed queued run: %+v", got)
	}

	if _, err := f.host.KillRun(first.ID); err != nil {
		t.Fatal(err)
	}
	f.host.AdmitRuns()
	time.Sleep(20 * time.Millisecond)
	if r := f.run(t, queued.ID); r.State != store.StateKilled || f.runAgents(t, queued.ID) != 0 {
		t.Fatalf("killed queued run after the slot freed: %+v", r)
	}
	if n := f.keeper.startedCount(); n != 1 {
		t.Fatalf("%d sessions started, want the first run's", n)
	}
}

func TestQueuedRunStartsAfterRestart(t *testing.T) {
	f := newFixture(t)
	f.host.MaxRuns = 1
	first := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "slack"})
	f.stepAgent(t, first.ID, "build")
	queued := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "slack"})
	if err := f.store.FinishRun(first.ID, store.End{State: store.StateFailed, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if r := f.run(t, queued.ID); r.State != store.StateQueued {
		t.Fatalf("queued run before the restart: %+v", r)
	}

	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond, MaxRuns: 1}
	f.srv.Config.Handler = restarted.Handler()
	restarted.AdmitRuns()
	f.stepAgent(t, queued.ID, "build")
	if r := f.run(t, queued.ID); r.State != store.StateBusy {
		t.Fatalf("queued run after the restart: %+v", r)
	}
}

func TestSourceWithItsOwnCapQueuesInItsOwnLane(t *testing.T) {
	f := newFixture(t)
	f.host.MaxRuns = 1
	f.host.MaxRunsBySource = map[string]int{"ekky-qa": 1}

	linear := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "linear"})
	if linear.State != store.StateBusy {
		t.Fatalf("linear run: %+v", linear)
	}
	qa1 := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "ekky-qa", Ref: "x/01@aaa"})
	if qa1.State != store.StateBusy {
		t.Fatalf("first qa run, its lane is empty: %+v", qa1)
	}
	qa2 := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "c", Source: "ekky-qa", Ref: "x/02@bbb"})
	if qa2.State != store.StateQueued {
		t.Fatalf("second qa run, its lane is full: %+v", qa2)
	}
	linear2 := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "d", Source: "linear"})
	if linear2.State != store.StateQueued {
		t.Fatalf("second linear run: %+v", linear2)
	}

	build := f.stepAgent(t, linear.ID, "build")
	f.done(t, build.ID, "built")
	waitFor(t, "first linear run to finish", func() bool { return f.run(t, linear.ID).State == store.StateDone })
	f.stepAgent(t, linear2.ID, "build")
	if r := f.run(t, qa2.ID); r.State != store.StateQueued {
		t.Fatalf("qa run took the linear lane's slot: %+v", r)
	}

	qaBuild := f.stepAgent(t, qa1.ID, "build")
	f.done(t, qaBuild.ID, "tested")
	waitFor(t, "first qa run to finish", func() bool { return f.run(t, qa1.ID).State == store.StateDone })
	f.stepAgent(t, qa2.ID, "build")
}
