package host

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) usageCall(t *testing.T, dir, agentID string, in, out, read, write int64) {
	t.Helper()
	path := filepath.Join(dir, agentID+".jsonl")
	before := len(f.events(t, agentID, store.KindUsage))
	appendLine(t, path, requestLine(fmt.Sprintf("r%d", before), in, out, read, write))
	f.hook(t, agentID, "PostToolUse", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "transcript_path": path})
	waitUsages(t, f, agentID, before+1)
	tl := f.host.hooks.tail(agentID, "")
	tl.mu.Lock()
	tl.mu.Unlock()
}

func (f *fixture) runView(t *testing.T, id string) RunView {
	t.Helper()
	for _, r := range f.runs(t) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("run %s not listed", id)
	return RunView{}
}

func TestTokenCapStored(t *testing.T) {
	f := newFixture(t)
	resp := f.postRun(t, RunRequest{Workflow: testWorkflow, Goal: "g", TokenCap: -1})
	msg, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || strings.TrimSpace(string(msg)) != "token_cap: must not be negative" {
		t.Fatalf("negative token_cap: %s %q", resp.Status, msg)
	}
	run := f.postRunOK(t, RunRequest{Workflow: testWorkflow, Goal: "g", TokenCap: 50000})
	if stored := f.run(t, run.ID); stored.TokenCap != 50000 || stored.ContextCap != 0 || stored.CapKills != 0 {
		t.Fatalf("run row: %+v", stored)
	}
	events, err := f.store.Events(store.EventQuery{RunID: run.ID, Kinds: []string{store.KindRunCreated}})
	if err != nil || len(events) != 1 || !strings.Contains(string(events[0].Data), `"token_cap":50000`) {
		t.Fatalf("run.created: %v %+v", err, events)
	}
	if folded := store.Fold(f.allEvents(t)).Runs; len(folded) != 1 || folded[0].TokenCap != 50000 {
		t.Fatalf("folded runs: %+v", folded)
	}
	if v := f.runView(t, run.ID); v.TokenCap != 50000 || v.Tokens != 0 {
		t.Fatalf("run view: %d of %d", v.Tokens, v.TokenCap)
	}
}

func TestTokenCapEndsRun(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", TokenCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)
	f.waitStarted(t, helper.ID)

	f.usageCall(t, dir, build.ID, 100, 100, 50000, 100)
	f.usageCall(t, dir, helper.ID, 300, 200, 90000, 0)
	f.usageCall(t, dir, build.ID, 10, 50, 0, 40)
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("run %s/%s under the cap", r.State, r.Reason)
	}
	if v := f.runView(t, run.ID); v.Tokens != 900 || v.TokenCap != 1000 {
		t.Fatalf("run view: %d of %d", v.Tokens, v.TokenCap)
	}

	f.usageCall(t, dir, helper.ID, 50, 60, 0, 0)
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
	entries := f.inbox(t, "")
	if len(entries) != 1 || entries[0].Reason != store.ReasonBudget || entries[0].Tokens != 1010 || entries[0].TokenCap != 1000 {
		t.Fatalf("inbox: %+v", entries)
	}
}

func TestNoTokenCapNeverStops(t *testing.T) {
	f := newFixture(t)
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g"})
	build := f.stepAgent(t, run.ID, "build")
	f.usageCall(t, t.TempDir(), build.ID, 900000, 900000, 0, 900000)
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("uncapped run %s/%s", r.State, r.Reason)
	}
	if v := f.runView(t, run.ID); v.Tokens != 2700000 || v.TokenCap != 0 {
		t.Fatalf("run view: %d of %d", v.Tokens, v.TokenCap)
	}
}

func TestTokenCapSkipsReopenedAgent(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	dir := t.TempDir()
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", TokenCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	helper := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "x")
	f.waitStarted(t, helper)
	f.usageCall(t, dir, helper, 100, 100, 0, 0)
	f.done(t, helper, "delivered")
	killed(t, f.keeper)
	f.keeper.vanish(f.agent(t, helper).TmuxSession)
	if _, err := f.host.Resume(helper); err != nil {
		t.Fatal(err)
	}
	kills := len(f.keeper.killed)

	f.usageCall(t, dir, helper, 5000, 5000, 0, 0)
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("run %s/%s after the reopened helper's tokens", r.State, r.Reason)
	}
	if v := f.runView(t, run.ID); v.Tokens != 10200 {
		t.Fatalf("run view tokens %d", v.Tokens)
	}
	f.usageCall(t, dir, build.ID, 400, 300, 0, 0)
	if r := f.run(t, run.ID); r.State != store.StateBusy {
		t.Fatalf("run %s/%s under the cap", r.State, r.Reason)
	}
	if len(f.keeper.killed) != kills {
		t.Fatal("reopened helper's session killed")
	}

	f.usageCall(t, dir, build.ID, 0, 101, 0, 0)
	f.waitRun(t, run.ID, store.StateFailed, store.ReasonBudget)
}

func TestTokenCapSparesDeliveredRun(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	release := make(chan struct{})
	f.host.DoneGates = []DoneGate{{Name: "hold", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		if c.Agent.Step == "check" {
			<-release
		}
		return nil
	}}}
	run := f.postRunOK(t, RunRequest{Workflow: headlessWorkflow, Goal: "g", TokenCap: 1000})
	build := f.stepAgent(t, run.ID, "build")
	f.usageCall(t, dir, build.ID, 300, 300, 0, 0)
	f.done(t, build.ID, "built")
	check := f.stepAgent(t, run.ID, "check")
	f.usageCall(t, dir, check.ID, 100, 100, 0, 0)

	appendLine(t, filepath.Join(dir, check.ID+".jsonl"), requestLine("last", 500, 500, 0, 0))
	f.done(t, check.ID, "checked")
	waitUsages(t, f, check.ID, 2)
	tl := f.host.hooks.tail(check.ID, "")
	tl.mu.Lock()
	tl.mu.Unlock()
	close(release)
	r := f.waitRun(t, run.ID, store.StateDone, store.ReasonDelivered)
	if st := r.Steps[1]; st.State != store.StateDone || st.Reason != store.ReasonDelivered {
		t.Fatalf("step: %+v", st)
	}
	if v := f.runView(t, run.ID); v.Tokens != 1800 {
		t.Fatalf("run view tokens %d", v.Tokens)
	}
}
