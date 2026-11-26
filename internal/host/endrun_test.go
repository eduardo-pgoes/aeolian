package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// endRun posts to the end endpoint and returns the status and the run.
func (f *fixture) endRun(t *testing.T, id string, force bool) (int, store.Run) {
	t.Helper()
	path := f.srv.URL + "/api/runs/" + id + "/end"
	if force {
		path += "?force=1"
	}
	resp, err := http.Post(path, "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var run store.Run
	if resp.StatusCode == http.StatusAccepted {
		json.NewDecoder(resp.Body).Decode(&run)
	}
	return resp.StatusCode, run
}

// transcriptOf writes a transcript for a whose last assistant message is
// last, where Transcripts finds it.
func (f *fixture) transcriptOf(t *testing.T, a store.Agent, last string) {
	t.Helper()
	if f.host.Transcripts == "" {
		f.host.Transcripts = t.TempDir()
	}
	dir := filepath.Join(f.host.Transcripts, "-some-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"an earlier thought"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":` + mustJSON(last) + `},{"type":"tool_use","name":"Bash"}]}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result"}]}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, a.SessionID+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func noTyping(t *testing.T, k *fakeKeeper) {
	t.Helper()
	select {
	case got := <-k.typed:
		t.Fatalf("typed into %s: %q", got.name, got.text)
	case <-time.After(20 * time.Millisecond):
	}
}

func nextTyped(t *testing.T, k *fakeKeeper) typed {
	t.Helper()
	select {
	case got := <-k.typed:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("nothing typed")
	}
	return typed{}
}

func TestEndRunGraceful(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")

	code, got := f.endRun(t, run.ID, false)
	if code != http.StatusAccepted || got.State != store.StateBusy {
		t.Fatalf("end: %d %+v", code, got)
	}
	if typed := nextTyped(t, f.keeper); typed.name != build.TmuxSession || typed.text != endMessage {
		t.Fatalf("typed %+v", typed)
	}
	if code, _ := f.endRun(t, run.ID, false); code != http.StatusConflict {
		t.Fatalf("second end: %d", code)
	}
	f.done(t, build.ID, "here is what was done")
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State != store.StateBusy })
	r := f.run(t, run.ID)
	wantEnd(t, r, store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	if r.Steps[1].State != store.StatePending {
		t.Fatalf("check step: %+v", r.Steps[1])
	}
	if a := f.agent(t, build.ID); a.State != store.StateDone || a.Reason != store.ReasonDelivered {
		t.Fatalf("lead: %s/%s", a.State, a.Reason)
	}
	waitFor(t, "ending to clear", func() bool { return !f.host.isEnding(run.ID) })
	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, run.ID); n != 1 {
		t.Fatalf("%d agents: the next step started", n)
	}
	entries := f.inbox(t, "")
	if len(entries) != 1 || entries[0].Handoff != "here is what was done" || entries[0].Reason != store.ReasonEnded || entries[0].Step != "build" {
		t.Fatalf("inbox: %+v", entries)
	}
}

func TestEndRunForce(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	f.transcriptOf(t, build, "half the parser is rewritten")

	ended, err := f.host.EndRun(run.ID, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd(t, ended, store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	if name := killed(t, f.keeper); name != build.TmuxSession {
		t.Fatalf("killed %s", name)
	}
	noTyping(t, f.keeper)
	a := f.agent(t, build.ID)
	if a.State != store.StateKilled || a.Reason != store.ReasonEnded || a.Handoff == nil || !a.Handoff.Synthesized ||
		!strings.Contains(a.Handoff.Text, "half the parser is rewritten") || strings.Contains(a.Handoff.Text, "earlier thought") {
		t.Fatalf("lead: %+v %+v", a, a.Handoff)
	}
	if e := f.inbox(t, ""); len(e) != 1 || !strings.Contains(e[0].Handoff, "half the parser") {
		t.Fatalf("inbox: %+v", e)
	}
	if res := f.callDone(t, build.ID, map[string]any{"text": "too late"}); !res.IsError {
		t.Fatalf("done after the end: %+v", res)
	}
}

func TestEndRunTimeout(t *testing.T) {
	f := newFixture(t)
	f.host.EndGrace = 50 * time.Millisecond
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	ended, err := f.host.EndRun(run.ID, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	if nextTyped(t, f.keeper).text != endMessage {
		t.Fatal("lead not asked")
	}
	wantEnd(t, ended, store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonEnded || a.Handoff.Text != "(the run was ended before its lead called done and left no message)" {
		t.Fatalf("lead: %+v %+v", a, a.Handoff)
	}
	killed(t, f.keeper)
}

func TestEndRunHeadlessLeadIsNotAsked(t *testing.T) {
	f := newFixture(t)
	f.host.EndGrace = time.Hour
	run, build := f.startHeadless(t, headlessWorkflow)
	ended, err := f.host.EndRun(run.ID, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	wantEnd(t, ended, store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	noTyping(t, f.keeper)
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonEnded {
		t.Fatalf("lead: %s/%s", a.State, a.Reason)
	}
}

func TestEndRunKillsHelpersWithoutTyping(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	session := f.connect(t, build.ID)
	for range 2 {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
			"role": "reviewer", "handoff": map[string]any{"text": "look", "worktrees": []store.Worktree{}}, "wait": false,
		}})
		if err != nil || res.IsError {
			t.Fatalf("spawn: %v %+v", err, res)
		}
	}
	waitFor(t, "helpers to start", func() bool { return f.keeper.startedCount() == 3 })

	if code, _ := f.endRun(t, run.ID, false); code != http.StatusAccepted {
		t.Fatalf("end: %d", code)
	}
	for range 2 {
		killed(t, f.keeper)
	}
	if got := nextTyped(t, f.keeper); got.text != endMessage {
		t.Fatalf("typed %q", got.text)
	}
	noTyping(t, f.keeper)
	agents, _ := f.store.Agents()
	for _, a := range agents {
		if a.ParentID != "" && (a.State != store.StateKilled || a.Reason != store.ReasonEnded) {
			t.Fatalf("helper %s/%s", a.State, a.Reason)
		}
		if a.ParentID != "" {
			// Its handoff says the host wrote it.
			if got := f.agent(t, a.ID); got.Handoff == nil || !got.Handoff.Synthesized || got.Handoff.Text != endedHandoff.Text {
				t.Fatalf("helper handoff %+v", got.Handoff)
			}
		}
	}
	// No new helper can start while the run ends.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
		"role": "reviewer", "handoff": map[string]any{"text": "more", "worktrees": []store.Worktree{}},
	}})
	if err != nil || !res.IsError {
		t.Fatalf("spawn while ending: %v %+v", err, res)
	}
	f.done(t, build.ID, "wrapped up")
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State == store.StateDone })
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	noTyping(t, f.keeper)
}

func TestEndRunVsKillRun(t *testing.T) {
	for _, killFirst := range []bool{false, true} {
		f := newFixture(t)
		f.host.EndGrace = time.Hour
		var run store.Run
		json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
		f.stepAgent(t, run.ID, "build")
		if killFirst {
			if _, err := f.host.KillRun(run.ID); err != nil {
				t.Fatal(err)
			}
			if code, _ := f.endRun(t, run.ID, false); code != http.StatusConflict {
				t.Fatalf("end after kill: %d", code)
			}
			continue
		}
		ended := make(chan error, 1)
		go func() {
			_, err := f.host.EndRun(run.ID, "test", false)
			ended <- err
		}()
		nextTyped(t, f.keeper)
		if _, err := f.host.KillRun(run.ID); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-ended:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("EndRun still waiting after KillRun")
		}
		wantEnd(t, f.run(t, run.ID), store.StateKilled, store.ReasonKilled, 0, store.StateKilled, store.ReasonKilled)
		if f.host.isEnding(run.ID) {
			t.Fatal("still ending")
		}
	}
}

func TestEndRunQueued(t *testing.T) {
	f := newFixture(t)
	first := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "s", Ref: "1"})
	queued := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "s", Ref: "2"})
	if queued.State != store.StateQueued {
		t.Fatalf("second run: %s", queued.State)
	}
	code, got := f.endRun(t, queued.ID, false)
	if code != http.StatusAccepted || got.State != store.StateDone || got.Reason != store.ReasonEnded {
		t.Fatalf("end queued: %d %+v", code, got)
	}
	f.done(t, f.stepAgent(t, first.ID, "build").ID, "ok")
	waitFor(t, "first to finish", func() bool { return f.run(t, first.ID).State == store.StateDone })
	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, queued.ID); n != 0 {
		t.Fatalf("ended queued run started %d agents", n)
	}
}

func TestEndRunErrors(t *testing.T) {
	f := newFixture(t)
	if code, _ := f.endRun(t, "nope", false); code != http.StatusNotFound {
		t.Fatalf("unknown: %d", code)
	}
	var run store.Run
	json.NewDecoder(f.startRun(t, oneStepWorkflow, "g").Body).Decode(&run)
	f.done(t, f.stepAgent(t, run.ID, "build").ID, "ok")
	waitFor(t, "run to finish", func() bool { return f.run(t, run.ID).State == store.StateDone })
	if code, _ := f.endRun(t, run.ID, false); code != http.StatusConflict {
		t.Fatalf("finished: %d", code)
	}
	if _, err := f.host.EndRun(run.ID, "test", false); !errors.Is(err, store.ErrFinished) {
		t.Fatalf("finished: %v", err)
	}
}

func TestDoneAfterKill(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	if _, err := f.host.Kill(a.ID); err != nil {
		t.Fatal(err)
	}
	if res := f.callDone(t, a.ID, map[string]any{"text": "late"}); !res.IsError {
		t.Fatalf("done after kill: %+v", res)
	}
	if got := f.agent(t, a.ID); got.State != store.StateKilled || got.Handoff != nil {
		t.Fatalf("agent: %+v", got)
	}
}

func TestEndRunTool(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	call := f.spawn(context.Background(), t, f.connect(t, build.ID), "reviewer")
	helper := f.helperOf(t, build.ID)

	res, err := f.connect(t, helper.ID).CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{"text": "x", "worktrees": []store.Worktree{}}})
	if err == nil && !res.IsError {
		t.Fatalf("helper got end_run: %+v", res)
	}
	f.done(t, helper.ID, "reviewed")
	<-call

	res, err = f.connect(t, build.ID).CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{
		"text": "final word", "worktrees": []store.Worktree{},
	}})
	if err != nil || res.IsError {
		t.Fatalf("end_run: %v %+v", err, res)
	}
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State != store.StateBusy })
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	if a := f.agent(t, build.ID); a.State != store.StateDone || a.Reason != store.ReasonDelivered || a.Handoff.Text != "final word" {
		t.Fatalf("lead: %+v", a)
	}
	waitFor(t, "ending to clear", func() bool { return !f.host.isEnding(run.ID) })
	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, run.ID); n != 2 {
		t.Fatalf("%d agents: the next step started", n)
	}
	noTyping(t, f.keeper)
}

func TestEndRunToolWithHoldingGate(t *testing.T) {
	f := newFixture(t)
	f.host.DoneGates = []DoneGate{{Name: "witness", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		<-ctx.Done()
		return ctx.Err()
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	res, err := f.connect(t, build.ID).CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{
		"text": "final word", "worktrees": []store.Worktree{},
	}})
	if err != nil || res.IsError {
		t.Fatalf("end_run: %v %+v", err, res)
	}
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State != store.StateBusy })
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
	waitFor(t, "ending to clear", func() bool { return !f.host.isEnding(run.ID) })
	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, run.ID); n != 1 {
		t.Fatalf("%d agents: the next step started", n)
	}
}

func TestEndRunWithHoldingGateClosesWithoutGrace(t *testing.T) {
	f := newFixture(t)
	f.host.EndGrace = time.Hour
	f.host.DoneGates = []DoneGate{{Name: "witness", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		<-ctx.Done()
		return ctx.Err()
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	ended := make(chan struct{})
	go func() {
		f.host.EndRun(run.ID, "person", false)
		close(ended)
	}()
	waitFor(t, "run to be ending", func() bool { return f.host.isEnding(run.ID) })
	f.done(t, build.ID, "wrapped up")
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("EndRun waited out its grace after the lead called done")
	}
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
}

func TestSweepClosedIdleLead(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	f.transcriptOf(t, build, "the retro command is on feat/retro")
	f.hook(t, build.ID, "Stop", struct{}{})
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()

	r := f.run(t, run.ID)
	wantEnd(t, r, store.StateDone, store.ReasonClosed, 0, store.StateDone, store.ReasonClosed)
	if r.Steps[1].State != store.StatePending {
		t.Fatalf("check: %+v", r.Steps[1])
	}
	a := f.agent(t, build.ID)
	if a.State != store.StateKilled || a.Reason != store.ReasonClosed || !a.Handoff.Synthesized {
		t.Fatalf("lead: %+v", a)
	}
	e := f.inbox(t, "")
	if len(e) != 1 || e[0].Reason != store.ReasonClosed || e[0].State != store.StateDone ||
		e[0].Handoff != "(the session was closed while idle, without done; this is its last message)\n\nthe retro command is on feat/retro" {
		t.Fatalf("inbox: %+v", e)
	}
	time.Sleep(20 * time.Millisecond)
	if n := f.runAgents(t, run.ID); n != 1 {
		t.Fatalf("%d agents: closing started the next step", n)
	}
}

func TestSweepLostBusyLead(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	f.hook(t, build.ID, "Stop", struct{}{})
	f.hook(t, build.ID, "UserPromptSubmit", struct{}{})
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	wantEnd(t, f.run(t, run.ID), store.StateFailed, store.ReasonLost, 0, store.StateFailed, store.ReasonLost)
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonLost {
		t.Fatalf("lead: %s/%s", a.State, a.Reason)
	}
}

func TestSweepKeepsResumedAgentsHandoff(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.done(t, a.ID, "the real result")
	killed(t, f.keeper)
	f.keeper.vanish(a.TmuxSession)
	if _, err := f.host.Resume(a.ID); err != nil {
		t.Fatal(err)
	}
	f.hook(t, a.ID, "Stop", struct{}{})
	f.keeper.vanish(a.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	got := f.agent(t, a.ID)
	if got.State != store.StateKilled || got.Reason != store.ReasonClosed || got.Handoff.Text != "the real result" || got.Handoff.Synthesized {
		t.Fatalf("resumed agent: %+v %+v", got, got.Handoff)
	}
}

// TestEndRunToolWhileEnding: the person ended the run and the lead was
// asked to hand back; it answering with end_run instead of done succeeds.
func TestEndRunToolWhileEnding(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	if code, _ := f.endRun(t, run.ID, false); code != http.StatusAccepted {
		t.Fatalf("end: %d", code)
	}
	nextTyped(t, f.keeper)
	res, err := f.connect(t, build.ID).CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{
		"text": "wrapped up", "worktrees": []store.Worktree{},
	}})
	if err != nil || res.IsError {
		t.Fatalf("end_run while ending: %v %+v", err, res)
	}
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State != store.StateBusy })
	wantEnd(t, f.run(t, run.ID), store.StateDone, store.ReasonEnded, 0, store.StateDone, store.ReasonEnded)
}

// TestEndRunToolBadInputKeepsHelpers: an end_run call done would refuse
// is refused before any helper is killed or the run starts ending.
func TestEndRunToolBadInputKeepsHelpers(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 3
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	session := f.connect(t, build.ID)
	helper := f.spawnLater(t, session, "reviewer", "x")
	f.waitStarted(t, helper)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{
		"text": "x", "worktrees": []store.Worktree{}, "outcome": "bogus",
	}})
	if err == nil && !res.IsError {
		t.Fatalf("bad end_run accepted: %+v", res)
	}
	noKill(t, f.keeper)
	if f.state(t, helper) != store.StateBusy || f.host.isEnding(run.ID) || f.run(t, run.ID).State != store.StateBusy {
		t.Fatal("a refused end_run ended work")
	}
}

// TestSweepLostAfterNotification: a Notification mid-turn leaves the lead
// busy, so its session dying is lost work, not a person closing it.
func TestSweepLostAfterNotification(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	f.hook(t, build.ID, "UserPromptSubmit", struct{}{})
	f.hook(t, build.ID, "Notification", struct{}{})
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	if a := f.agent(t, build.ID); a.State != store.StateKilled || a.Reason != store.ReasonLost {
		t.Fatalf("lead %s/%s", a.State, a.Reason)
	}
	if r := f.run(t, run.ID); r.State != store.StateFailed || r.Reason != store.ReasonLost {
		t.Fatalf("run %s/%s", r.State, r.Reason)
	}
}
