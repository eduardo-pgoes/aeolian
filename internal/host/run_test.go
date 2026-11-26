package host

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const testWorkflow = `
steps:
  - name: build
    ink: sakura
    mark: hammer
    prompt: "Build {{.Goal}}."
    helpers: [reviewer]
  - name: check
    prompt: "Check {{.Goal}}. Step one said: {{.Handoff}}{{range .Worktrees}} [{{.Path}} on {{.Branch}}]{{end}}"
roles:
  reviewer:
    ink: sky
    mark: eye
    prompt: "Review {{.Handoff}} for {{.Goal}}. {{.ExtraInstructions}}"
  tester:
    prompt: "Test."
`

func (f *fixture) startRun(t *testing.T, src, goal string) *http.Response {
	t.Helper()
	return f.postRun(t, RunRequest{Workflow: src, Goal: goal})
}

func (f *fixture) postRun(t *testing.T, req RunRequest) *http.Response {
	t.Helper()
	body, _ := json.Marshal(req)
	resp, err := http.Post(f.srv.URL+"/api/runs", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func (f *fixture) run(t *testing.T, id string) store.Run {
	t.Helper()
	run, err := f.store.Run(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func (f *fixture) stepAgent(t *testing.T, runID, step string) store.Agent {
	t.Helper()
	var found store.Agent
	waitFor(t, "step "+step+" to start", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.RunID == runID && a.Step == step && a.ParentID == "" {
				found = a
				r, _ := f.store.Run(runID)
				for _, st := range r.Steps {
					if st.Name == step && st.AgentID == a.ID {
						return true
					}
				}
			}
		}
		return false
	})
	return found
}

func TestRunWalksStepsInOrder(t *testing.T) {
	f := newFixture(t)
	resp := f.startRun(t, testWorkflow, "the parser")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start run: %s", resp.Status)
	}
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.ID == "" || run.State != store.StateBusy || len(run.Steps) != 2 {
		t.Fatalf("run: %+v", run)
	}

	build := f.stepAgent(t, run.ID, "build")
	if build.Prompt != "Build the parser." || build.Dir != f.host.Layout.ScratchDir(build.ID) {
		t.Fatalf("build agent: %+v", build)
	}
	session := f.connect(t, build.ID)
	wt := store.Worktree{Repo: "/r", Path: t.TempDir(), Branch: "parser"}

	c := <-f.spawn(context.Background(), t, session, "tester", wt)
	want := `role "tester" not allowed in step "build"; allowed: reviewer`
	if c.err != nil || !c.res.IsError || resultText(c.res) != want {
		t.Fatalf("disallowed spawn: %v %+v", c.err, c.res)
	}
	if n := f.keeper.startedCount(); n != 1 {
		t.Fatalf("disallowed spawn started an agent: %d started", n)
	}

	call := f.spawn(context.Background(), t, session, "reviewer", wt)
	helper := f.helperOf(t, build.ID)
	if helper.RunID != run.ID || helper.Step != "build" || helper.Prompt != "Review review the fix for the parser. be brief" {
		t.Fatalf("helper: %+v", helper)
	}
	f.done(t, helper.ID, "looks good", wt)
	if c := <-call; c.err != nil || c.res.IsError {
		t.Fatalf("spawn: %v %+v", c.err, c.res)
	}
	if st := f.run(t, run.ID).Steps[0].State; st != store.StateBusy {
		t.Fatalf("helper's done moved the step to %s", st)
	}

	f.done(t, build.ID, "parser builds", wt)
	check := f.stepAgent(t, run.ID, "check")
	if want := "Check the parser. Step one said: parser builds [" + wt.Path + " on parser]"; check.Prompt != want {
		t.Fatalf("check prompt %q, want %q", check.Prompt, want)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateBusy || r.Steps[0].State != store.StateDone || r.Steps[1].State != store.StateBusy {
		t.Fatalf("run after step one: %+v", r)
	}

	c = <-f.spawn(context.Background(), t, f.connect(t, check.ID), "reviewer", wt)
	if c.err != nil || !c.res.IsError || resultText(c.res) != `role "reviewer" not allowed in step "check"; allowed: none` {
		t.Fatalf("spawn from check: %v %+v", c.err, c.res)
	}

	f.done(t, check.ID, "checked")
	waitFor(t, "run to finish", func() bool { return f.run(t, run.ID).State == store.StateDone })
	r = f.run(t, run.ID)
	if r.DoneAt == nil || r.Steps[1].State != store.StateDone {
		t.Fatalf("finished run: %+v", r)
	}
	if n := f.keeper.startedCount(); n != 3 {
		t.Fatalf("%d agents started, want 3", n)
	}
}

func TestRunRefusesBadWorkflow(t *testing.T) {
	f := newFixture(t)
	if resp := f.startRun(t, "steps: [{name: a, prompt: p, helpers: [ghost]}]", "g"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad workflow: %s", resp.Status)
	}
	if resp := f.startRun(t, testWorkflow, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty goal: %s", resp.Status)
	}
	if n := f.keeper.startedCount(); n != 0 {
		t.Fatalf("%d agents started", n)
	}
}

func TestRunSourceAndRef(t *testing.T) {
	f := newFixture(t)
	req := RunRequest{Workflow: testWorkflow, Goal: "g", Source: "slack-docs", Ref: "t1"}
	resp := f.postRun(t, req)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start: %s", resp.Status)
	}
	var first store.Run
	if err := json.NewDecoder(resp.Body).Decode(&first); err != nil || first.Source != "slack-docs" || first.Ref != "t1" {
		t.Fatalf("first run: %v %+v", err, first)
	}

	resp = f.postRun(t, req)
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(msg), first.ID) {
		t.Fatalf("duplicate: %s %s", resp.Status, msg)
	}
	if resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g", Ref: "t1"}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ref without source: %s", resp.Status)
	}
	for _, other := range []RunRequest{
		{Workflow: testWorkflow, Goal: "g", Source: "slack-docs", Ref: "t2"},
		{Workflow: testWorkflow, Goal: "g", Source: "slack-docs"},
		{Workflow: testWorkflow, Goal: "g", Source: "slack-docs"},
	} {
		if resp := f.postRun(t, other); resp.StatusCode != http.StatusCreated {
			t.Fatalf("start %+v: %s", other, resp.Status)
		}
	}

	list := func(query string) []RunView {
		t.Helper()
		resp, err := http.Get(f.srv.URL + "/api/runs" + query)
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
	if runs := list("?source=slack-docs&ref=t1"); len(runs) != 1 || runs[0].ID != first.ID {
		t.Fatalf("filtered: %+v", runs)
	}
	if runs := list("?source=slack-ops"); len(runs) != 0 {
		t.Fatalf("other source: %+v", runs)
	}
	if runs := list(""); len(runs) != 4 {
		t.Fatalf("all runs: %d", len(runs))
	}
}

func TestGetRun(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	resp, err := http.Get(f.srv.URL + "/api/runs/" + run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got store.Run
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || got.ID != run.ID || got.Goal != "g" {
		t.Fatalf("get run: %v %+v", err, got)
	}
	if resp, _ := http.Get(f.srv.URL + "/api/runs/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown run: %s", resp.Status)
	}
}

func TestStepAndRoleModelsReachArgv(t *testing.T) {
	f := newFixture(t)
	src := strings.Replace(strings.Replace(testWorkflow,
		"    helpers: [reviewer]\n", "    helpers: [reviewer]\n    model: opus\n", 1),
		"    mark: eye\n", "    mark: eye\n    model: sonnet\n", 1)
	resp := f.startRun(t, src, "g")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	cases := []struct {
		a    store.Agent
		want string
	}{
		{build, "opus"},
		{store.Agent{ID: "h", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "reviewer"}, "sonnet"},
		{store.Agent{ID: "t", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "tester"}, ""},
		{store.Agent{ID: "c", RunID: run.ID, Step: "check"}, ""},
		{store.Agent{ID: "x"}, ""},
	}
	for _, c := range cases {
		if got := f.host.model(c.a); got != c.want {
			t.Errorf("agent %s: model %q, want %q", c.a.ID, got, c.want)
		}
	}
	argv := f.keeper.argv[0]
	if i := slices.Index(argv, "--model"); i < 0 || argv[i+1] != "opus" || promptOf(t, argv) != "Build g." {
		t.Fatalf("argv: %q", argv)
	}
}

func TestWildcardStepSpawnsAnyRole(t *testing.T) {
	f := newFixture(t)
	src := strings.Replace(testWorkflow, "helpers: [reviewer]", "helpers: [\"*\"]", 1)
	resp := f.startRun(t, src, "the parser")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	roles := RunRoles{Store: f.store}
	got, err := roles.Resolve(build, "reviewer", store.Handoff{Text: "the fix"}, "be brief")
	if err != nil || got != "Review the fix for the parser. be brief" {
		t.Fatalf("declared role: %q %v", got, err)
	}
	got, err = roles.Resolve(build, "researcher", store.Handoff{Text: "find the bug"}, "look in lexer.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Goal of the run: the parser", "researcher helper", "find the bug", "look in lexer.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("free role prompt lacks %q: %q", want, got)
		}
	}
	check := store.Agent{RunID: run.ID, Step: "check"}
	if _, err := roles.Resolve(check, "researcher", store.Handoff{}, ""); err == nil {
		t.Fatal("check step spawned a role it does not list")
	}
}

func TestStepAndRoleMemoryReachArgv(t *testing.T) {
	f := newFixture(t)
	src := strings.Replace(strings.Replace(testWorkflow,
		"    helpers: [reviewer]\n", "    helpers: [reviewer, \"*\"]\n    memory: 4G\n", 1),
		"    mark: eye\n", "    mark: eye\n    memory: 1536M\n", 1)
	resp := f.startRun(t, src, "g")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	for _, c := range []struct {
		a    store.Agent
		want string
	}{
		{build, "4G"},
		{store.Agent{ID: "h", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "reviewer"}, "1536M"},
		{store.Agent{ID: "t", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "tester"}, ""},
		{store.Agent{ID: "r", RunID: run.ID, Step: "build", ParentID: build.ID, Role: "researcher"}, "4G"},
		{store.Agent{ID: "c", RunID: run.ID, Step: "check"}, ""},
		{store.Agent{ID: "x"}, ""},
	} {
		if got := f.host.memory(c.a); got != c.want {
			t.Errorf("agent %s: memory %q, want %q", c.a.ID, got, c.want)
		}
	}
	argv := f.keeper.argv[0]
	if !slices.Contains(argv, "MemoryMax=4G") || !slices.Contains(argv, "MemoryHigh=3276M") {
		t.Fatalf("build argv: %q", argv)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := f.connect(t, build.ID)
	f.spawn(ctx, t, session, "reviewer")
	reviewer := f.helperOf(t, build.ID)
	if argv := f.argvOf(t, reviewer); !slices.Contains(argv, "MemoryMax=1536M") || !slices.Contains(argv, "MemoryHigh=1228M") {
		t.Fatalf("reviewer argv: %q", argv)
	}
	f.done(t, reviewer.ID, "fine")
	f.spawn(ctx, t, session, "researcher")
	var researcher store.Agent
	waitFor(t, "researcher to start", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.Role == "researcher" {
				researcher = a
				f.keeper.mu.Lock()
				defer f.keeper.mu.Unlock()
				return slices.Contains(f.keeper.started, a.TmuxSession)
			}
		}
		return false
	})
	if argv := f.argvOf(t, researcher); !slices.Contains(argv, "MemoryMax=4G") || !slices.Contains(argv, "MemoryHigh=3276M") {
		t.Fatalf("free helper argv: %q", argv)
	}
}

func TestStepAndRoleDirs(t *testing.T) {
	f := newFixture(t)
	stepDir, roleDir := t.TempDir(), t.TempDir()
	src := strings.Replace(strings.Replace(testWorkflow,
		"    helpers: [reviewer]\n", "    helpers: [reviewer]\n    dir: "+stepDir+"\n", 1),
		"    mark: eye\n", "    mark: eye\n    dir: "+roleDir+"\n", 1)
	resp := f.postRun(t, RunRequest{Workflow: src, Goal: "g", Dir: t.TempDir()})
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	if build.Dir != stepDir {
		t.Fatalf("step started in %q, want %q", build.Dir, stepDir)
	}
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)
	if helper.Dir != roleDir {
		t.Fatalf("helper started in %q, want %q", helper.Dir, roleDir)
	}
	f.done(t, helper.ID, "ok")
	<-call
	wt := store.Worktree{Repo: stepDir, Path: stepDir, Branch: "b"}
	call = f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer", wt)
	var second store.Agent
	waitFor(t, "second helper", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.ParentID == build.ID && a.ID != helper.ID {
				second = a
				return true
			}
		}
		return false
	})
	if second.Dir != stepDir {
		t.Fatalf("handed worktree lost to role dir: %q", second.Dir)
	}
	f.done(t, second.ID, "ok")
	<-call
}

func TestMissingStepDirFailsRun(t *testing.T) {
	f := newFixture(t)
	missing := filepath.Join(t.TempDir(), "gone")
	src := strings.Replace(testWorkflow, "    helpers: [reviewer]\n", "    helpers: [reviewer]\n    dir: "+missing+"\n", 1)
	resp := f.startRun(t, src, "g")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run to fail", func() bool { return f.run(t, run.ID).State == store.StateFailed })
	if r := f.run(t, run.ID); r.Steps[0].Reason != store.ReasonStartFailed {
		t.Fatalf("step: %+v", r.Steps[0])
	}
	if n := f.keeper.startedCount(); n != 0 {
		t.Fatalf("%d agents started", n)
	}
}

func TestRunDirIsWhereUnpinnedAgentsStart(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g", Dir: dir})
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if run.Dir != dir {
		t.Fatalf("run dir %q, want %q", run.Dir, dir)
	}
	build := f.stepAgent(t, run.ID, "build")
	if build.Dir != dir {
		t.Fatalf("step started in %q, want %q", build.Dir, dir)
	}
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)
	if helper.Dir != dir {
		t.Fatalf("helper started in %q, want %q", helper.Dir, dir)
	}
	f.done(t, helper.ID, "ok")
	<-call
}

func TestRunDirRefused(t *testing.T) {
	f := newFixture(t)
	file := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"rel/dir", filepath.Join(t.TempDir(), "gone"), file} {
		resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g", Dir: dir})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("dir %q: status %d", dir, resp.StatusCode)
		}
	}
	if n := f.keeper.startedCount(); n != 0 {
		t.Fatalf("%d agents started", n)
	}
}
