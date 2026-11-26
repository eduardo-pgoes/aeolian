package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) kinds(t *testing.T, q store.EventQuery) []store.Event {
	t.Helper()
	evs, err := f.store.Events(q)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func errText(res *mcp.CallToolResult) string {
	if !res.IsError {
		return ""
	}
	return resultText(res)
}

var twoClaims = map[string]any{"claims": []any{
	map[string]any{"id": "cap", "text": "the default cap is 2", "kind": "fact", "evidence": []string{"cap.go:12", " "}, "confidence": "High"},
	map[string]any{"text": "the cap is sensible", "kind": "judgment"},
}}

func TestDoneStoresParts(t *testing.T) {
	f := newFixture(t)
	resp := f.startRun(t, testWorkflow, "the parser")
	var run store.Run
	json.NewDecoder(resp.Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "build")
	writeFile(t, filepath.Join(a.Dir, "reports", "x.md"), "# report")

	res := f.callDone(t, a.ID, map[string]any{"text": "built", "parts": []any{
		map[string]any{"kind": "artifact", "body": map[string]any{"path": "reports/x.md", "title": "the report"}},
		map[string]any{"kind": "claims", "body": twoClaims},
	}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	got, _ := f.store.Agent(a.ID)
	if got.State != store.StateDone || len(got.Handoff.Parts) != 2 {
		t.Fatalf("agent = %+v", got)
	}
	var art artifactBody
	json.Unmarshal(got.Handoff.Parts[0].Body, &art)
	if art.Path != filepath.Join(a.Dir, "reports", "x.md") || art.Title != "the report" {
		t.Fatalf("artifact = %+v", art)
	}
	var cb ClaimsBody
	json.Unmarshal(got.Handoff.Parts[1].Body, &cb)
	if cb.Claims[1].ID != "c2" || cb.Claims[0].Confidence != "high" || len(cb.Claims[0].Evidence) != 1 {
		t.Fatalf("claims body = %+v", cb)
	}

	cs, _ := f.store.Claims(run.ID)
	if len(cs) != 2 || cs[0].ID != store.ClaimID(a.ID, 0) || cs[0].Key != "cap" || cs[0].RunID != run.ID || cs[1].Kind != "judgment" {
		t.Fatalf("claims = %+v", cs)
	}
	evs := f.kinds(t, store.EventQuery{AgentID: a.ID, Kinds: []string{"parts.recorded", "claims.recorded"}})
	if len(evs) != 2 || evs[0].Kind != "parts.recorded" || !strings.Contains(string(evs[0].Data), `"kinds":["artifact","claims"]`) ||
		!strings.Contains(string(evs[1].Data), `"count":2`) {
		t.Fatalf("events = %+v", evs)
	}

	// The next step's template sees the parts.
	next := f.stepAgent(t, run.ID, "check")
	if next.Prompt == "" {
		t.Fatal("no next step")
	}

	// REST: the claims of the run and the parts of the agent.
	var rc []store.Claim
	getJSON(t, f.srv.URL+"/api/runs/"+run.ID+"/claims", http.StatusOK, &rc)
	if len(rc) != 2 {
		t.Fatalf("GET claims = %+v", rc)
	}
	var rp []store.Part
	getJSON(t, f.srv.URL+"/api/agents/"+a.ID+"/parts", http.StatusOK, &rp)
	if len(rp) != 2 || rp[1].Kind != "claims" {
		t.Fatalf("GET parts = %+v", rp)
	}
	getJSON(t, f.srv.URL+"/api/agents/"+next.ID+"/parts", http.StatusOK, &rp)
	if rp == nil || len(rp) != 0 {
		t.Fatalf("GET parts of a live agent = %+v", rp)
	}
	getJSON(t, f.srv.URL+"/api/runs/nope/claims", http.StatusNotFound, nil)
}

func getJSON(t *testing.T, url string, code int, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != code {
		t.Fatalf("GET %s: %s", url, resp.Status)
	}
	if v != nil {
		if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBadPartsStoreNothing(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	outside := filepath.Join(t.TempDir(), "elsewhere.md")
	writeFile(t, outside, "x")
	inside := filepath.Join(a.Dir, "ok.md")
	writeFile(t, inside, "x")
	if err := os.Symlink(outside, filepath.Join(a.Dir, "sneaky.md")); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		part any
		want string
	}{
		"unknown kind":     {map[string]any{"kind": "vibes", "body": map[string]any{}}, "known kinds: artifact, claims"},
		"artifact outside": {map[string]any{"kind": "artifact", "body": map[string]any{"path": outside}}, "outside your folder"},
		"artifact symlink": {map[string]any{"kind": "artifact", "body": map[string]any{"path": "sneaky.md"}}, "outside your folder"},
		"artifact missing": {map[string]any{"kind": "artifact", "body": map[string]any{"path": "nope.md"}}, "does not exist"},
		"artifact extra":   {map[string]any{"kind": "artifact", "body": map[string]any{"path": inside, "colour": "red"}}, "unknown field"},
		"artifact escape":  {map[string]any{"kind": "artifact", "body": map[string]any{"path": "../../" + filepath.Base(outside)}}, ""},
		"no claims":        {map[string]any{"kind": "claims", "body": map[string]any{"claims": []any{}}}, "no claims"},
		"claim no text":    {map[string]any{"kind": "claims", "body": []any{map[string]any{"id": "a"}}}, "has no text"},
		"duplicate ids":    {map[string]any{"kind": "claims", "body": []any{map[string]any{"id": "a", "text": "x"}, map[string]any{"id": "a", "text": "y"}}}, `share id "a"`},
		"bad claim kind":   {map[string]any{"kind": "claims", "body": []any{map[string]any{"text": "x", "kind": "vibe"}}}, "has kind"},
		"bad confidence":   {map[string]any{"kind": "claims", "body": []any{map[string]any{"text": "x", "confidence": "sure"}}}, "has confidence"},
	} {
		res := f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{tc.part}})
		if !res.IsError || !strings.Contains(resultText(res), tc.want) {
			t.Errorf("%s: %v %q", name, res.IsError, resultText(res))
		}
	}
	got, _ := f.store.Agent(a.ID)
	if store.Finished(got.State) || got.Handoff != nil {
		t.Fatalf("a refused done stored something: %+v", got)
	}
	if _, closing := f.host.closing.Load(a.ID); closing {
		t.Fatal("a refused done closes the session")
	}

	// A lenient body: a path as a plain string, claims as quoted JSON.
	res := f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{
		map[string]any{"kind": "artifact", "body": "ok.md"},
		map[string]any{"kind": "claims", "body": `[{"text": "quoted"}]`},
	}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	cs, _ := f.store.AgentClaims(a.ID)
	if len(cs) != 1 || cs[0].Text != "quoted" || cs[0].RunID != "" {
		t.Fatalf("claims = %+v", cs)
	}
}

func TestTooManyParts(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	var parts []any
	for range MaxParts + 1 {
		parts = append(parts, map[string]any{"kind": "claims", "body": []any{map[string]any{"text": "x"}}})
	}
	if res := f.callDone(t, a.ID, map[string]any{"text": "x", "parts": parts}); !strings.Contains(errText(res), "at most") {
		t.Fatalf("res = %q", resultText(res))
	}
}

func TestGateCheckRefusesThenAllows(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	var seen atomic.Int32
	f.host.DoneGates = []DoneGate{{Name: "claims-required", Check: func(h *Host, c DoneCall) error {
		seen.Add(1)
		for _, p := range c.Handoff.Parts {
			if p.Kind == "claims" {
				return nil
			}
		}
		return errors.New("this role must hand back claims: add a claims part")
	}}}
	res := f.callDone(t, a.ID, map[string]any{"text": "x"})
	if !strings.Contains(errText(res), "add a claims part") {
		t.Fatalf("res = %q", resultText(res))
	}
	if got, _ := f.store.Agent(a.ID); store.Finished(got.State) {
		t.Fatal("refused done finished the agent")
	}
	res = f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	if got, _ := f.store.Agent(a.ID); got.State != store.StateDone || seen.Load() != 2 {
		t.Fatalf("agent = %+v, checks = %d", got, seen.Load())
	}
}

const twoStepPartsWorkflow = `
steps:
  - name: write
    prompt: "Write {{.Goal}}."
  - name: publish
    prompt: "Publish. {{range .Parts}}[{{.Kind}} {{.Text}}]{{end}}"
`

func TestHoldDelaysTheNextStep(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	held := make(chan DoneCall, 1)
	f.host.DoneGates = []DoneGate{{Name: "witness", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		held <- c
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "the notes").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	res := f.callDone(t, a.ID, map[string]any{"text": "written", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	c := <-held
	if c.Agent.ID != a.ID || len(c.Handoff.Parts) != 1 {
		t.Fatalf("held call = %+v", c)
	}
	time.Sleep(50 * time.Millisecond)
	if r := f.run(t, run.ID); r.Steps[0].State != store.StateBusy || r.Steps[1].State != store.StatePending {
		t.Fatalf("run moved on while held: %+v", r.Steps)
	}
	close(release)
	next := f.stepAgent(t, run.ID, "publish")
	if !strings.Contains(next.Prompt, `[claims {"claims":[{"id":"cap"`) {
		t.Fatalf("next prompt = %q", next.Prompt)
	}
	evs := f.kinds(t, store.EventQuery{AgentID: a.ID, Kinds: []string{"gate.held", "gate.passed"}})
	if len(evs) != 2 {
		t.Fatalf("events = %+v", evs)
	}
}

func TestHoldTimesOutAndRunFinishes(t *testing.T) {
	f := newFixture(t)
	f.host.DoneGates = []DoneGate{{Name: "witness", Timeout: 30 * time.Millisecond, Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		<-ctx.Done()
		_, err := h.MarkUnwitnessed(c.Agent.ID, "no witness in time")
		return errors.Join(ctx.Err(), err)
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, `
steps:
  - name: only
    prompt: "Do {{.Goal}}."
`, "it").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "only")
	f.callDone(t, a.ID, map[string]any{"text": "did", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})
	waitFor(t, "run to finish", func() bool { return f.run(t, run.ID).State == store.StateDone })
	cs, _ := f.store.Claims(run.ID)
	if len(cs) != 2 || cs[0].Verdict != store.VerdictUnwitnessed || cs[0].VerdictNote != "no witness in time" {
		t.Fatalf("claims = %+v", cs)
	}
	evs := f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"gate.timeout", "claim.verdict"}})
	if len(evs) != 3 || evs[2].Kind != "gate.timeout" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestKilledRunDuringHoldDoesNotMoveOn(t *testing.T) {
	f := newFixture(t)
	held := make(chan struct{})
	f.host.DoneGates = []DoneGate{{Name: "witness", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		close(held)
		<-ctx.Done()
		return nil
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	f.callDone(t, a.ID, map[string]any{"text": "did"})
	<-held
	if _, err := f.host.KillRun(run.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hold to be cancelled", func() bool {
		return len(f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"gate.cancelled"}})) == 1
	})
	time.Sleep(30 * time.Millisecond)
	r := f.run(t, run.ID)
	if r.State != store.StateKilled || r.Steps[1].State != store.StatePending {
		t.Fatalf("run = %+v", r)
	}
}

func TestHelpersAreNotHeld(t *testing.T) {
	f := newFixture(t)
	f.host.DoneGates = []DoneGate{{Name: "never", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		t.Error("held a helper")
		return nil
	}}}
	parent := f.start(t)
	session := f.connect(t, parent.ID)
	out := f.spawn(context.Background(), t, session, "reviewer")
	helper := f.helperOf(t, parent.ID)
	writeFile(t, filepath.Join(helper.Dir, "r.md"), "x")
	res := f.callDone(t, helper.ID, map[string]any{"text": "looks fine", "parts": []any{
		map[string]any{"kind": "claims", "body": twoClaims},
		map[string]any{"kind": "artifact", "body": "r.md"},
	}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	got := <-out
	if got.err != nil {
		t.Fatal(got.err)
	}
	text := resultText(got.res)
	if !strings.HasSuffix(text, "Parts: claims (2), artifact "+filepath.Join(helper.Dir, "r.md")) {
		t.Fatalf("spawn result = %q", text)
	}
}

func TestSpawnPassesParts(t *testing.T) {
	f := newFixture(t)
	parent := f.start(t)
	session := f.connect(t, parent.ID)
	call := func(parts any) (*mcp.CallToolResult, error) {
		wait := false
		return session.CallTool(context.Background(), &mcp.CallToolParams{Name: "spawn", Arguments: map[string]any{
			"role": "witness", "wait": wait,
			"handoff": map[string]any{"text": "check these", "worktrees": []any{}, "parts": parts},
		}})
	}
	res, err := call([]any{map[string]any{"kind": "nope", "body": 1}})
	if err != nil || !strings.Contains(errText(res), "known kinds") {
		t.Fatalf("bad part: %v %q", err, resultText(res))
	}
	res, err = call([]any{map[string]any{"kind": "claims", "body": twoClaims}})
	if err != nil || res.IsError {
		t.Fatalf("spawn: %v %q", err, resultText(res))
	}
	helper := f.helperOf(t, parent.ID)
	if !strings.Contains(helper.Prompt, "check these\n\nParts:\n- claims: {\"claims\":[{\"id\":\"cap\"") {
		t.Fatalf("helper prompt = %q", helper.Prompt)
	}
}

func TestSetVerdictEmits(t *testing.T) {
	f := newFixture(t)
	if err := f.store.AddClaims("a1", []store.Claim{{RunID: "r1", Text: "x"}, {RunID: "r1", Text: "y"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.host.SetVerdict("a1-0", store.VerdictHolds, "checked cap.go:12", "w1"); err != nil {
		t.Fatal(err)
	}
	if err := f.host.SetVerdict("a1-9", store.VerdictHolds, "", "w1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing claim: %v", err)
	}
	if n, err := f.host.MarkUnwitnessed("a1", "late"); n != 1 || err != nil {
		t.Fatalf("marked %d: %v", n, err)
	}
	evs := f.kinds(t, store.EventQuery{RunID: "r1", Kinds: []string{"claim.verdict"}})
	if len(evs) != 2 || !strings.Contains(string(evs[0].Data), `"verdict":"holds"`) || evs[0].AgentID != "a1" {
		t.Fatalf("events = %+v", evs)
	}
}

func TestDoneSchemaLeavesBodiesOpen(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	tools, err := f.connect(t, a.ID).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "done" && tool.Name != "spawn" {
			continue
		}
		b, _ := json.Marshal(tool.InputSchema)
		s := string(b)
		if !strings.Contains(s, `"body":{"description":"the part itself`) || strings.Contains(s, `"maximum":255`) {
			t.Errorf("%s schema = %s", tool.Name, s)
		}
	}
}

func TestExtraToolsAndRoutes(t *testing.T) {
	saved := extraTools
	t.Cleanup(func() { extraTools = saved })
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, id string, a store.Agent) {
		if a.Role != "witness" {
			return
		}
		mcp.AddTool(s, &mcp.Tool{Name: "verdict"}, func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, any, error) {
			return textResult("ok " + id), nil, nil
		})
	})
	f := newFixture(t)
	parent := f.start(t)
	session := f.connect(t, parent.ID)
	out := f.spawn(context.Background(), t, session, "witness")
	helper := f.helperOf(t, parent.ID)
	has := func(id string) bool {
		tools, err := f.connect(t, id).ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range tools.Tools {
			if tool.Name == "verdict" {
				return true
			}
		}
		return false
	}
	if has(parent.ID) || !has(helper.ID) {
		t.Error("verdict should reach the witness only")
	}
	f.callDone(t, helper.ID, map[string]any{"text": "done"})
	<-out
}

type adapterInboxEntry struct {
	RunID      string           `json:"run_id"`
	Source     string           `json:"source"`
	Ref        string           `json:"ref"`
	State      string           `json:"state"`
	Goal       string           `json:"goal"`
	FinishedAt *time.Time       `json:"finished_at"`
	Step       string           `json:"step"`
	Handoff    string           `json:"handoff"`
	Worktrees  []store.Worktree `json:"worktrees"`
	LogTail    []string         `json:"log_tail"`
}

func TestInboxEntryCarriesParts(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, `
steps:
  - name: only
    prompt: "Do {{.Goal}}."
`, "it").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "only")
	f.callDone(t, a.ID, map[string]any{"text": "did", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})
	waitFor(t, "run to finish", func() bool { return f.run(t, run.ID).State == store.StateDone })
	e := f.host.inboxEntry(f.run(t, run.ID))
	if len(e.Parts) != 1 || e.Parts[0].Kind != "claims" {
		t.Fatalf("entry = %+v", e)
	}
	b, _ := json.Marshal(e)
	dec := json.NewDecoder(strings.NewReader(string(b)))
	var old adapterInboxEntry
	if err := dec.Decode(&old); err != nil || old.Handoff != "did" || old.RunID != run.ID {
		t.Fatalf("adapter decode: %v %+v", err, old)
	}
}

func TestResumeDoneAfterRestart(t *testing.T) {
	f := newFixture(t)
	stop := make(chan struct{})
	f.host.DoneGates = []DoneGate{{Name: "stuck", Hold: func(ctx context.Context, h *Host, c DoneCall) error {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		return nil
	}}}
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	f.callDone(t, a.ID, map[string]any{"text": "did", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})

	// A new host on the same store, as after a restart; the old one's hold
	// never returns.
	h2 := &Host{Store: f.store, Layout: f.host.Layout, Keeper: f.keeper, KillGrace: time.Millisecond, URL: f.srv.URL}
	h2.ResumeDone()
	next := f.stepAgent(t, run.ID, "publish")
	if !strings.Contains(next.Prompt, "[claims ") {
		t.Fatalf("resumed step prompt = %q", next.Prompt)
	}
	close(stop)
	// The old host's hold now returns; its advance finds the step finished.
	time.Sleep(30 * time.Millisecond)
	if r := f.run(t, run.ID); r.Steps[1].AgentID != next.ID {
		t.Fatalf("second step restarted: %+v", r.Steps)
	}
}
