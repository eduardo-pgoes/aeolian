package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) post(t *testing.T, path string, out any) int {
	t.Helper()
	resp, err := http.Post(f.srv.URL+path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func (f *fixture) runs(t *testing.T) []RunView {
	t.Helper()
	resp, err := http.Get(f.srv.URL + "/api/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var runs []RunView
	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	return runs
}

func killed(t *testing.T, k *fakeKeeper) string {
	t.Helper()
	select {
	case name := <-k.killed:
		return name
	case <-time.After(5 * time.Second):
		t.Fatal("no session killed")
		return ""
	}
}

func TestListRunsWithAgents(t *testing.T) {
	f := newFixture(t)
	if runs := f.runs(t); runs == nil || len(runs) != 0 {
		t.Fatalf("no runs: %+v", runs)
	}
	f.start(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "the parser").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	wt := store.Worktree{Repo: "/r", Path: t.TempDir(), Branch: "parser"}
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer", wt)
	helper := f.helperOf(t, build.ID)

	runs := f.runs(t)
	if len(runs) != 1 || runs[0].ID != run.ID || runs[0].Goal != "the parser" || len(runs[0].Steps) != 2 {
		t.Fatalf("runs: %+v", runs)
	}
	steps := runs[0].Steps
	if steps[0].Ink != "sakura" || steps[0].Mark != "hammer" || steps[0].AgentID != build.ID || steps[1].Ink != "" || steps[1].Mark != "" {
		t.Fatalf("steps: %+v", steps)
	}
	agents := runs[0].Agents
	if len(agents) != 2 || agents[0].ID != build.ID || !agents[0].Blocked || agents[1].ID != helper.ID || agents[1].Blocked {
		t.Fatalf("agents: %+v", agents)
	}
	if agents[0].Ink != "sakura" || agents[0].Mark != "hammer" || agents[1].Ink != "sky" || agents[1].Mark != "eye" {
		t.Fatalf("agent inks: %+v", agents)
	}
	// tunings: the hash of each mark, as the map drew it before; the step
	// view and its agent agree
	if steps[0].Tuning != "grain" || agents[0].Tuning != "grain" || agents[1].Tuning != "saw" || steps[1].Tuning == "" {
		t.Fatalf("tunings: steps %+v agents %+v", steps, agents)
	}
	if agents[0].Cols != 120 || agents[0].Rows != 40 || agents[1].Cols != 120 || agents[1].Rows != 40 {
		t.Fatalf("agent sizes: %+v", agents)
	}

	f.done(t, helper.ID, "looks good", wt)
	<-call
	agents = f.runs(t)[0].Agents
	if agents[0].Blocked || agents[1].State != store.StateDone || agents[1].Handoff == nil || agents[1].Handoff.Text != "looks good" {
		t.Fatalf("agents after helper done: %+v", agents)
	}
	killed(t, f.keeper)
	agents = f.runs(t)[0].Agents
	if agents[1].Cols != 0 || agents[1].Rows != 0 || agents[1].Ink != "sky" {
		t.Fatalf("finished helper: %+v", agents[1])
	}
}

func TestHostInfo(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct{ max, want int }{{0, DefaultMaxAgents}, {5, 5}} {
		f.host.MaxAgents = c.max
		resp, err := http.Get(f.srv.URL + "/api/host")
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]int
		json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if _, ok := got["last_event_id"]; len(got) != 4 || !ok || got["max_agents"] != c.want || got["busy"] != 0 || got["idle"] != 0 {
			t.Fatalf("max %d: %v", c.max, got)
		}
	}
	f.start(t)
	f.hook(t, f.start(t).ID, "Stop", struct{}{})
	resp, err := http.Get(f.srv.URL + "/api/host")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got HostView
	json.NewDecoder(resp.Body).Decode(&got)
	if got.Busy != 1 || got.Idle != 1 {
		t.Fatalf("load: %+v", got)
	}
}

func TestKillStepAgentFailsRun(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 1
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	queued := make(chan store.Agent, 1)
	go func() { queued <- f.start(t) }()
	waitFor(t, "start to queue", func() bool { return f.host.queued() == 1 })

	var got store.Agent
	if code := f.post(t, "/api/agents/"+build.ID+"/kill", &got); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	if name := killed(t, f.keeper); name != build.TmuxSession {
		t.Fatalf("killed %q", name)
	}
	if got.State != store.StateKilled || got.DoneAt == nil {
		t.Fatalf("killed agent: %+v", got)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateFailed || r.Steps[0].State != store.StateFailed || r.Steps[1].State != store.StatePending {
		t.Fatalf("run after kill: %+v", r)
	}
	<-queued

	if code := f.post(t, "/api/agents/"+build.ID+"/kill", nil); code != http.StatusConflict {
		t.Fatalf("second kill: %d", code)
	}
	if code := f.post(t, "/api/agents/nobody/kill", nil); code != http.StatusNotFound {
		t.Fatalf("unknown kill: %d", code)
	}
}

func TestKillHelperHandsParentKilled(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	wt := store.Worktree{Repo: "/r", Path: t.TempDir(), Branch: "b"}
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "reviewer", wt)
	helper := f.helperOf(t, parent.ID)

	if code := f.post(t, "/api/agents/"+helper.ID+"/kill", nil); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	killed(t, f.keeper)
	c := <-call
	if c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), killedHandoff.Text) {
		t.Fatalf("spawn after kill: %v %+v", c.err, c.res)
	}
}

func TestResumeFinishedAgent(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if code := f.post(t, "/api/agents/"+a.ID+"/resume", nil); code != http.StatusConflict {
		t.Fatalf("resume live agent: %d", code)
	}
	f.done(t, a.ID, "ok")
	killed(t, f.keeper)

	var res ResumeResponse
	if code := f.post(t, "/api/agents/"+a.ID+"/resume", &res); code != http.StatusOK {
		t.Fatalf("resume: %d", code)
	}
	if res.TmuxSession != a.TmuxSession {
		t.Fatalf("resume returned %q, want %q", res.TmuxSession, a.TmuxSession)
	}
	f.keeper.mu.Lock()
	name, dir, argv := f.keeper.started[1], f.keeper.dirs[1], f.keeper.argv[1]
	f.keeper.mu.Unlock()
	if name != a.TmuxSession || dir != a.Dir {
		t.Fatalf("resumed %q in %q", name, dir)
	}
	i := slices.Index(argv, "claude")
	if argv[i+1] != "--resume" || argv[i+2] != a.SessionID || slices.Contains(argv, a.Prompt) {
		t.Fatalf("argv: %q", argv)
	}
	got, _ := f.store.Agent(a.ID)
	if got.State != store.StateIdle || got.Handoff == nil {
		t.Fatalf("resumed agent: %+v", got)
	}

	if code := f.post(t, "/api/agents/nobody/resume", nil); code != http.StatusNotFound {
		t.Fatalf("unknown resume: %d", code)
	}
}

func TestResumeRefusesAGoneDir(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g", Dir: dir})
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	a := f.stepAgent(t, run.ID, "build")
	f.done(t, a.ID, "ok")
	waitFor(t, "build to end", func() bool {
		got, _ := f.store.Agent(a.ID)
		return store.Finished(got.State)
	})
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	started := f.keeper.startedCount()
	if code := f.post(t, "/api/agents/"+a.ID+"/resume", nil); code != http.StatusGone {
		t.Fatalf("resume: %d", code)
	}
	if n := f.keeper.startedCount(); n != started {
		t.Fatalf("resume started a session: %d -> %d", started, n)
	}
	if _, err := f.host.Resume(a.ID); !errors.Is(err, ErrDirGone) || !strings.Contains(err.Error(), dir) {
		t.Fatalf("resume error: %v", err)
	}
}
