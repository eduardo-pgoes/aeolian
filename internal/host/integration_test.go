package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// These tests cover where the honest-ends, event-log and parts work meet.

// A headless lead that awaits a helper reads the helper's parts summary,
// as a parent handed a spawn result does.
func TestAwaitShowsParts(t *testing.T) {
	f := newFixture(t)
	_, build := f.startHeadless(t, headlessWorkflow)
	session := f.connect(t, build.ID)
	helper := f.spawnLater(t, session, "reviewer", "check it")
	call := f.await(session, map[string]any{})
	f.waitStarted(t, helper)
	res := f.callDone(t, helper, map[string]any{"text": "checked", "parts": []any{
		map[string]any{"kind": "claims", "body": twoClaims},
	}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	c := <-call
	if c.isErr || !strings.Contains(c.text, "checked") || !strings.Contains(c.text, "Parts: claims (2)") {
		t.Fatalf("await: %+v", c)
	}
}

// end_run takes done's parts: they are stored, recorded and logged.
func TestEndRunToolTakesParts(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, testWorkflow, "g").Body).Decode(&run)
	build := f.stepAgent(t, run.ID, "build")
	res, err := f.connect(t, build.ID).CallTool(context.Background(), &mcp.CallToolParams{Name: "end_run", Arguments: map[string]any{
		"text": "final word", "worktrees": []store.Worktree{},
		"parts": []any{map[string]any{"kind": "claims", "body": twoClaims}},
	}})
	if err != nil || res.IsError {
		t.Fatalf("end_run: %v %+v", err, res)
	}
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State != store.StateBusy })
	a := f.agent(t, build.ID)
	if len(a.Handoff.Parts) != 1 || a.Handoff.Parts[0].Kind != "claims" {
		t.Fatalf("lead: %+v", a.Handoff)
	}
	if cs, _ := f.store.Claims(run.ID); len(cs) != 2 {
		t.Fatalf("claims: %+v", cs)
	}
	evs := f.kinds(t, store.EventQuery{AgentID: build.ID, Kinds: []string{store.KindAgentDone}})
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), `"kind":"claims"`) {
		t.Fatalf("agent.done carries no parts: %+v", evs)
	}
}
