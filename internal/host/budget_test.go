package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) toolCall(t *testing.T, dir, agentID string, level int64) {
	t.Helper()
	path := filepath.Join(dir, agentID+".jsonl")
	before := len(f.events(t, agentID, store.KindUsage))
	appendLine(t, path, requestLine(fmt.Sprintf("r%d", before), level, 1, 0, 0))
	f.hook(t, agentID, "PostToolUse", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "transcript_path": path})
	waitUsages(t, f, agentID, before+1)
	tl := f.host.hooks.tail(agentID, "")
	tl.mu.Lock()
	tl.mu.Unlock()
}

func (f *fixture) waitEnded(t *testing.T, id, state, reason string) {
	t.Helper()
	waitFor(t, "agent "+id+" to end "+state+"/"+reason, func() bool {
		a := f.agent(t, id)
		return a.State == state && a.Reason == reason
	})
}

func (f *fixture) waitRun(t *testing.T, id, state, reason string) store.Run {
	t.Helper()
	var run store.Run
	waitFor(t, "run "+id+" to end "+state+"/"+reason, func() bool {
		run = f.run(t, id)
		return run.State == state && run.Reason == reason
	})
	return run
}

func helperCapText(level, limit int64) string {
	return fmt.Sprintf("(the helper reached its context cap (%d of %d tokens) and the host stopped it before it called done; spawn a fresh helper with this handoff to pick up the work", level, limit)
}

func TestRunRefusesBadCap(t *testing.T) {
	f := newFixture(t)
	for _, c := range []struct {
		req  RunRequest
		want string
	}{
		{RunRequest{ContextCap: -1}, "context_cap: must not be negative"},
		{RunRequest{ContextCap: 100, CapKills: -1}, "cap_kills: must not be negative"},
		{RunRequest{CapKills: -2}, "cap_kills: must not be negative"},
		{RunRequest{CapKills: 2}, "cap_kills: needs a context_cap"},
	} {
		c.req.Workflow, c.req.Goal = testWorkflow, "g"
		resp := f.postRun(t, c.req)
		msg, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(msg)) != c.want {
			t.Errorf("%d/%d: %s %q", c.req.ContextCap, c.req.CapKills, resp.Status, msg)
		}
	}
	if runs, _ := f.store.Runs(store.RunFilter{}); len(runs) != 0 {
		t.Fatalf("refused run stored: %+v", runs)
	}

	run := f.postRunOK(t, RunRequest{Workflow: testWorkflow, Goal: "g", ContextCap: 120000})
	if run.ContextCap != 120000 || run.CapKills != DefaultCapKills {
		t.Fatalf("default cap_kills: %+v", run)
	}
	events, err := f.store.Events(store.EventQuery{RunID: run.ID, Kinds: []string{store.KindRunCreated}})
	if err != nil || len(events) != 1 || !strings.Contains(string(events[0].Data), `"context_cap":120000,"cap_kills":3`) {
		t.Fatalf("run.created: %v %+v", err, events)
	}
	run = f.postRunOK(t, RunRequest{Workflow: testWorkflow, Goal: "h", ContextCap: 5, CapKills: 1})
	if stored := f.run(t, run.ID); stored.ContextCap != 5 || stored.CapKills != 1 {
		t.Fatalf("run row: %+v", stored)
	}
	folded := store.Fold(f.allEvents(t)).Runs
	if len(folded) != 2 || folded[1].ContextCap != 5 || folded[1].CapKills != 1 || folded[0].CapKills != DefaultCapKills {
		t.Fatalf("folded runs: %+v", folded)
	}
}

func (f *fixture) allEvents(t *testing.T) []store.Event {
	t.Helper()
	evs, err := f.store.Events(store.EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func TestCapStopsHeadlessHelper(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)
	f.waitStarted(t, helper.ID)
	if !helper.Headless {
		t.Fatal("helper is not headless")
	}

	f.toolCall(t, dir, helper.ID, 1000)
	if a := f.agent(t, helper.ID); a.State != store.StateBusy {
		t.Fatalf("stopped at the cap, not over it: %s/%s", a.State, a.Reason)
	}
	f.toolCall(t, dir, helper.ID, 1500)
	f.waitEnded(t, helper.ID, store.StateFailed, store.ReasonBudget)
	if name := killed(t, f.keeper); name != helper.TmuxSession {
		t.Fatalf("killed %s", name)
	}
	c := <-call
	if c.err != nil || c.res.IsError || !strings.Contains(resultText(c.res), helperCapText(1500, 1000)) {
		t.Fatalf("lead got %v %q", c.err, resultText(c.res))
	}
	if a := f.agent(t, build.ID); a.State != store.StateBusy {
		t.Fatalf("lead %s", a.State)
	}
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("run %s/%s after one budget kill", r.State, r.Reason)
	}
}

func TestCapFailsHeadlessStepAgent(t *testing.T) {
	f := newFixture(t)
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	f.toolCall(t, t.TempDir(), build.ID, 4000)
	f.waitEnded(t, build.ID, store.StateFailed, store.ReasonBudget)
	r := f.waitRun(t, run.ID, store.StateFailed, store.ReasonBudget)
	if st := r.Steps[0]; st.State != store.StateFailed || st.Reason != store.ReasonBudget || r.Steps[1].State != store.StatePending {
		t.Fatalf("steps: %+v", r.Steps)
	}
	a := f.agent(t, build.ID)
	if a.Handoff == nil || !strings.Contains(a.Handoff.Text, "(the agent reached its context cap (4000 of 1000 tokens) and the host stopped it before it called done") {
		t.Fatalf("handoff: %+v", a.Handoff)
	}
	entries := f.inbox(t, "")
	if len(entries) != 1 || entries[0].RunID != run.ID || entries[0].State != store.StateFailed || entries[0].Reason != store.ReasonBudget {
		t.Fatalf("inbox: %+v", entries)
	}
}

func TestCapKillsEndRun(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 4
	dir := t.TempDir()
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	session := f.connect(t, build.ID)
	idle := f.spawnLater(t, session, "reviewer", "stay")
	f.waitStarted(t, idle)
	for i := range DefaultCapKills {
		helper := f.spawnLater(t, session, "reviewer", fmt.Sprintf("try %d", i))
		f.waitStarted(t, helper)
		f.toolCall(t, dir, helper, 2000)
		f.waitEnded(t, helper, store.StateFailed, store.ReasonBudget)
		if i < DefaultCapKills-1 {
			if r := f.run(t, run.ID); r.State != store.StateBusy {
				t.Fatalf("run %s/%s after %d budget kills", r.State, r.Reason, i+1)
			}
		}
	}
	r := f.waitRun(t, run.ID, store.StateFailed, store.ReasonBudget)
	if st := r.Steps[0]; st.State != store.StateFailed || st.Reason != store.ReasonBudget {
		t.Fatalf("step: %+v", st)
	}
	waitFor(t, "every agent of the run to stop", func() bool {
		agents, _ := f.store.AgentsOfRuns([]string{run.ID})
		for _, a := range agents {
			if !store.Finished(a.State) {
				return false
			}
		}
		return true
	})
	if a := f.agent(t, idle); a.State != store.StateKilled {
		t.Fatalf("bystander helper %s/%s", a.State, a.Reason)
	}
	if a := f.agent(t, build.ID); a.State != store.StateKilled {
		t.Fatalf("lead %s/%s", a.State, a.Reason)
	}
	if entries := f.inbox(t, ""); len(entries) != 1 || entries[0].Reason != store.ReasonBudget {
		t.Fatalf("inbox: %+v", entries)
	}
}

func TestCapStartingStepFailsWithRun(t *testing.T) {
	f := newFixture(t)
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	f.stepAgent(t, run.ID, "build")
	if err := f.host.stopRun(run.ID, store.End{State: store.StateFailed, Reason: store.ReasonBudget}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StartStep(run.ID, 1, "late", f.run(t, run.ID).CreatedAt); err != nil {
		t.Fatal(err)
	}
	f.host.closeStepIfRunClosed(run.ID, 1)
	if st := f.run(t, run.ID).Steps[1]; st.State != store.StateFailed || st.Reason != store.ReasonBudget {
		t.Fatalf("starting step: %+v", st)
	}
}

func TestCapSkipsWitnesses(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run := f.postRunOK(t, RunRequest{Workflow: witnessedWorkflow, Goal: "the notes", ContextCap: 1000, CapKills: 1})
	a := f.stepAgent(t, run.ID, "write")
	writeFile(t, filepath.Join(a.Dir, "notes.md"), "# notes\nthe cap is 2\nthe faxbridge client is exported\n")
	if res := f.callDone(t, a.ID, map[string]any{"text": "written", "parts": []any{map[string]any{"kind": "claims", "body": map[string]any{"claims": witnessClaims}}}}); res.IsError {
		t.Fatal(resultText(res))
	}
	w := f.witnessOf(t, a.ID)
	f.waitStarted(t, w.ID)
	f.toolCall(t, t.TempDir(), w.ID, 5000)
	f.waitEnded(t, w.ID, store.StateFailed, store.ReasonBudget)
	waitFor(t, "run to move on", func() bool { return f.run(t, run.ID).Steps[1].State == store.StateBusy })
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("a witness counted toward cap_kills: run %s/%s", r.State, r.Reason)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	for _, c := range cs {
		if c.Verdict != store.VerdictUnwitnessed || !strings.Contains(c.VerdictNote, "the witness was stopped (budget)") {
			t.Fatalf("claim = %+v", c)
		}
	}
}

func TestCapSparesInteractiveAndCaplessRuns(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	interactive := f.postRunOK(t, RunRequest{Workflow: testWorkflow, Goal: "g", ContextCap: 1000})
	build := f.stepAgent(t, interactive.ID, "build")
	if build.Headless {
		t.Fatal("step agent is headless")
	}
	f.toolCall(t, dir, build.ID, 50000)

	capless := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "h"})
	hb := f.stepAgent(t, capless.ID, "build")
	f.toolCall(t, dir, hb.ID, 900000)

	for _, id := range []string{build.ID, hb.ID} {
		if a := f.agent(t, id); a.State != store.StateBusy {
			t.Fatalf("agent %s stopped: %s/%s", id, a.State, a.Reason)
		}
	}
	for _, id := range []string{interactive.ID, capless.ID} {
		if r := f.run(t, id); r.State != store.StateBusy {
			t.Fatalf("run %s: %s/%s", id, r.State, r.Reason)
		}
	}
}

func TestCapSparesResumedHeadlessHelper(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	dir := t.TempDir()
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000, CapKills: 1})
	build := f.stepAgent(t, run.ID, "build")
	session := f.connect(t, build.ID)
	helper := f.spawnLater(t, session, "reviewer", "x")
	f.waitStarted(t, helper)
	f.done(t, helper, "delivered")
	killed(t, f.keeper)
	f.keeper.vanish(f.agent(t, helper).TmuxSession)
	if _, err := f.host.Resume(helper); err != nil {
		t.Fatal(err)
	}
	f.hook(t, helper, "UserPromptSubmit", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": "one more pass"})
	waitFor(t, "resumed helper to be busy", func() bool { return f.agent(t, helper).State == store.StateBusy })
	kills := len(f.keeper.killed)

	f.toolCall(t, dir, helper, 5000)
	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	restarted.enforceCap(helper, 9000)

	if a := f.agent(t, helper); a.State != store.StateBusy {
		t.Fatalf("resumed helper capped: %s/%s", a.State, a.Reason)
	}
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("run %s/%s", r.State, r.Reason)
	}
	if n, err := f.host.capKills(run.ID); err != nil || n != 0 {
		t.Fatalf("cap kills %d %v", n, err)
	}
	if len(f.keeper.killed) != kills {
		t.Fatal("resumed helper's session killed")
	}
}

func TestCapSkipsAgentThatCalledDone(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	helper := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "x")
	f.waitStarted(t, helper)
	f.done(t, helper, "delivered")
	if name := killed(t, f.keeper); name != f.agent(t, helper).TmuxSession {
		t.Fatalf("done killed %s", name)
	}
	kills := len(f.keeper.killed)
	f.host.enforceCap(helper, 5000)
	if a := f.agent(t, helper); a.State != store.StateDone || a.Handoff == nil || a.Handoff.Text != "delivered" {
		t.Fatalf("helper %s/%s %+v", a.State, a.Reason, a.Handoff)
	}
	if len(f.keeper.killed) != kills {
		t.Fatal("finished helper's session killed")
	}
}

func TestRestartKeepsStoppedRunsContext(t *testing.T) {
	f := newFixture(t)
	budget := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", ContextCap: 1000})
	f.stepAgent(t, budget.ID, "build")
	if err := f.host.stopRun(budget.ID, store.End{State: store.StateFailed, Reason: store.ReasonBudget, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stalled := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "h", ContextCap: 1000})
	f.stepAgent(t, stalled.ID, "build")
	if err := f.host.stopRun(stalled.ID, store.End{State: store.StateFailed, Reason: store.ReasonLost, At: time.Now()}); err != nil {
		t.Fatal(err)
	}

	restarted := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	if restarted.runContext(budget.ID).Err() == nil {
		t.Fatal("run failed at its cap starts again after a restart")
	}
	if err := restarted.runContext(stalled.ID).Err(); err != nil {
		t.Fatalf("run failed lost: %v", err)
	}
}
