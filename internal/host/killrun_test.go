package host

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

func (f *fixture) killRun(t *testing.T, id string) *http.Response {
	t.Helper()
	resp, err := http.Post(f.srv.URL+"/api/runs/"+id+"/kill", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestKillRunStopsEveryStart(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	session := f.connect(t, build.ID)
	wt := store.Worktree{Path: t.TempDir()}

	liveCall := f.spawn(context.Background(), t, session, "reviewer", wt)
	helper := f.helperOf(t, build.ID)
	queuedCall := f.spawn(context.Background(), t, session, "reviewer", wt)
	waitFor(t, "helper start to queue", func() bool { return f.host.queued() == 1 })

	wf, err := workflow.Parse([]byte(testWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	nextStep := make(chan struct{})
	go func() {
		f.host.startStep(run.ID, wf, 1, workflow.Data{Goal: "g"})
		close(nextStep)
	}()
	waitFor(t, "next step to queue", func() bool { return f.host.queued() == 2 })

	resp := f.killRun(t, run.ID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("kill run: %s", resp.Status)
	}
	var got store.Run
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.ID != run.ID || got.State != store.StateKilled {
		t.Fatalf("kill run returned %v %+v", err, got)
	}
	if f.host.queued() != 0 {
		t.Fatalf("%d starts still queued", f.host.queued())
	}
	<-nextStep
	if c := <-liveCall; c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), killedHandoff.Text) {
		t.Fatalf("live helper's spawn: %v %+v", c.err, c.res)
	}
	if c := <-queuedCall; c.err != nil || !c.res.IsError {
		t.Fatalf("queued helper's spawn: %v %+v", c.err, c.res)
	}

	time.Sleep(20 * time.Millisecond)
	r := f.run(t, run.ID)
	if r.State != store.StateKilled || r.DoneAt == nil || r.Steps[0].State != store.StateKilled || r.Steps[1].State != store.StatePending {
		t.Fatalf("killed run: %+v", r)
	}
	agents, _ := f.store.Agents()
	if len(agents) != 2 {
		t.Fatalf("%d agents after kill, want the step agent and one helper", len(agents))
	}
	for _, a := range agents {
		if a.State != store.StateKilled || (a.ID != build.ID && a.ID != helper.ID) {
			t.Fatalf("agent after kill: %+v", a)
		}
	}
	if n := f.keeper.startedCount(); n != 2 {
		t.Fatalf("%d sessions started, want 2", n)
	}

	if resp := f.killRun(t, run.ID); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second kill: %s", resp.Status)
	}
	if resp := f.killRun(t, "nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run: %s", resp.Status)
	}
}

func TestKilledRunStartsNoHelpers(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	if _, err := f.host.KillRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Resume(build.ID); err != nil {
		t.Fatal(err)
	}

	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	f.srv.Config.Handler = restarted.Handler()
	c := <-f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer", store.Worktree{Path: t.TempDir()})
	if c.err != nil || !c.res.IsError {
		t.Fatalf("spawn in a killed run: %v %+v", c.err, c.res)
	}
	if n := f.keeper.startedCount(); n != 2 {
		t.Fatalf("%d sessions started, want the step and its resume", n)
	}
}

func TestKillFinishedRun(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	f.stepAgent(t, run.ID, "build")
	if err := f.store.FinishRun(run.ID, store.End{State: store.StateDone, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if resp := f.killRun(t, run.ID); resp.StatusCode != http.StatusConflict {
		t.Fatalf("kill done run: %s", resp.Status)
	}
}
