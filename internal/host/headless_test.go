package host

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const headlessWorkflow = `
steps:
  - name: build
    headless: true
    helpers: [reviewer]
    prompt: "Build {{.Goal}}."
  - name: check
    headless: true
    helpers: [tester]
    prompt: "Check {{.Goal}}. {{.Handoff}}"
roles:
  reviewer:
    prompt: "Review {{.Handoff}}."
  tester:
    headless: true
    prompt: "Test."
`

func (f *fixture) startHeadless(t *testing.T, src string) (store.Run, store.Agent) {
	t.Helper()
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, src, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	return run, f.stepAgent(t, run.ID, "build")
}

func (f *fixture) argvOf(t *testing.T, a store.Agent) []string {
	t.Helper()
	f.keeper.mu.Lock()
	defer f.keeper.mu.Unlock()
	i := slices.Index(f.keeper.started, a.TmuxSession)
	if i < 0 {
		t.Fatalf("agent %s never started", a.ID)
	}
	return f.keeper.argv[i]
}

func promptOf(t *testing.T, argv []string) string {
	t.Helper()
	c := slices.Index(argv, "claude")
	if c < 0 || argv[0] != "sh" || slices.Index(argv, agent.PromptArg) != c+1 && argv[c+2] != agent.PromptArg {
		t.Fatalf("prompt slot moved: %q", argv)
	}
	b, err := os.ReadFile(argv[3])
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runsHeadless(argv []string) bool {
	i := slices.Index(argv, "claude")
	return i >= 0 && argv[i+1] == "-p" && argv[0] == "sh"
}

func (f *fixture) callDone(t *testing.T, agentID string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	if _, ok := args["worktrees"]; !ok {
		args["worktrees"] = []store.Worktree{}
	}
	res, err := f.connect(t, agentID).CallTool(context.Background(), &mcp.CallToolParams{Name: "done", Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestHeadlessStepLaunchesWithPrint(t *testing.T) {
	f := newFixture(t)
	_, build := f.startHeadless(t, headlessWorkflow)
	if !build.Headless || !runsHeadless(f.argvOf(t, build)) {
		t.Fatalf("build agent %+v argv %q", build, f.argvOf(t, build))
	}
	_, interactive := f.startHeadless(t, testWorkflow)
	if interactive.Headless || runsHeadless(f.argvOf(t, interactive)) {
		t.Fatalf("interactive agent %+v argv %q", interactive, f.argvOf(t, interactive))
	}
	for _, c := range []struct {
		a    store.Agent
		want bool
	}{{build, true}, {interactive, false}} {
		tools, err := f.connect(t, c.a.ID).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(tools.Tools, func(tool *mcp.Tool) bool { return tool.Name == "done" })
		if got := strings.Contains(tools.Tools[i].Description, "outcome blocked"); got != c.want {
			t.Fatalf("done of headless %v tells of blocked: %v", c.a.Headless, got)
		}
	}
}

func TestHeadlessStepFailsWhenSessionEnds(t *testing.T) {
	f := newFixture(t)
	run, build := f.startHeadless(t, headlessWorkflow)
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	if s := f.state(t, build.ID); s != store.StateBusy {
		t.Fatalf("after one sweep: %s", s)
	}
	f.host.Sweep()
	if s := f.state(t, build.ID); s != store.StateFailed {
		t.Fatalf("after two sweeps: %s", s)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateFailed || r.Steps[0].State != store.StateFailed || r.Steps[1].State != store.StatePending {
		t.Fatalf("run: %+v", r)
	}
	noKill(t, f.keeper)
}

func TestKilledHeadlessAgentStaysKilled(t *testing.T) {
	f := newFixture(t)
	_, build := f.startHeadless(t, headlessWorkflow)
	var got store.Agent
	f.post(t, "/api/agents/"+build.ID+"/kill", &got)
	killed(t, f.keeper)
	f.host.Sweep()
	f.host.Sweep()
	if s := f.state(t, build.ID); s != store.StateKilled {
		t.Fatalf("killed headless agent: %s", s)
	}
}

func TestBlockedDoneEndsRun(t *testing.T) {
	f := newFixture(t)
	run, build := f.startHeadless(t, headlessWorkflow)
	res := f.callDone(t, build.ID, map[string]any{"text": "Which repo, api or web?", "outcome": "blocked"})
	if res.IsError {
		t.Fatalf("blocked done: %+v", res)
	}
	waitFor(t, "run to block", func() bool { return f.run(t, run.ID).State == store.StateBlocked })
	r := f.run(t, run.ID)
	if r.DoneAt == nil || r.Steps[0].State != store.StateBlocked || r.Steps[1].State != store.StatePending {
		t.Fatalf("blocked run: %+v", r)
	}
	got, _ := f.store.Agent(build.ID)
	if got.State != store.StateDone || got.Handoff == nil || got.Handoff.Text != "Which repo, api or web?" {
		t.Fatalf("blocked agent: %+v", got)
	}
	killed(t, f.keeper)
	if n := f.keeper.startedCount(); n != 1 {
		t.Fatalf("%d agents started after a blocked step", n)
	}
}

func TestDoneOutcomes(t *testing.T) {
	f := newFixture(t)
	run, build := f.startHeadless(t, testWorkflow)
	if res := f.callDone(t, build.ID, map[string]any{"text": "x", "outcome": "maybe"}); !res.IsError || resultText(res) != `outcome "maybe" is neither done nor blocked` {
		t.Fatalf("bad outcome: %+v", res)
	}
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer", store.Worktree{Path: t.TempDir()})
	helper := f.helperOf(t, build.ID)
	if res := f.callDone(t, helper.ID, map[string]any{"text": "?", "outcome": "blocked"}); !res.IsError || !strings.HasPrefix(resultText(res), "only a step's agent can end blocked") {
		t.Fatalf("helper blocked: %+v", res)
	}
	f.done(t, helper.ID, "fine")
	<-call
	if res := f.callDone(t, build.ID, map[string]any{"text": "built", "outcome": "done"}); res.IsError {
		t.Fatalf("done outcome: %+v", res)
	}
	f.stepAgent(t, run.ID, "check")
}

func TestHeadlessParentsHelperIsHeadless(t *testing.T) {
	f := newFixture(t)
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	call := f.spawn(context.Background(), t, session, "reviewer")
	helper := f.helperOf(t, build.ID)
	if !helper.Headless || !runsHeadless(f.argvOf(t, helper)) {
		t.Fatalf("helper of a headless parent: %+v", helper)
	}
	f.done(t, helper.ID, "looks good")
	if c := <-call; c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), "looks good") {
		t.Fatalf("spawn: %v %+v", c.err, c.res)
	}
}

func TestHeadlessRoleUnderInteractiveStep(t *testing.T) {
	f := newFixture(t)
	src := strings.Replace(headlessWorkflow, "  - name: build\n    headless: true\n", "  - name: build\n", 1)
	src = strings.Replace(src, "helpers: [reviewer]", "helpers: [reviewer, tester]", 1)
	_, build := f.startHeadless(t, src)
	if build.Headless {
		t.Fatal("build is headless")
	}
	session := f.connect(t, build.ID)
	for _, c := range []struct {
		role string
		want bool
	}{{"reviewer", false}, {"tester", true}} {
		call := f.spawn(context.Background(), t, session, c.role)
		var helper store.Agent
		waitFor(t, c.role+" to start", func() bool {
			agents, _ := f.store.Agents()
			for _, a := range agents {
				if a.ParentID == build.ID && a.Role == c.role {
					helper = a
					f.keeper.mu.Lock()
					defer f.keeper.mu.Unlock()
					return slices.Contains(f.keeper.started, a.TmuxSession)
				}
			}
			return false
		})
		if helper.Headless != c.want || runsHeadless(f.argvOf(t, helper)) != c.want {
			t.Fatalf("%s helper: %+v", c.role, helper)
		}
		f.done(t, helper.ID, "ok")
		<-call
	}
}
