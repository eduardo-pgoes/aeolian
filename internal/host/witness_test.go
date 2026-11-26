package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const sentinel = "SENTINEL-producer-context-7f3a"

const witnessedWorkflow = `
steps:
  - name: write
    witness: true
    prompt: "Write {{.Goal}}."
  - name: publish
    prompt: "Publish. {{range .Parts}}[{{.Kind}} {{.Text}}]{{end}}"
`

var testPrecedents = filepath.Join("..", "precedent", "testdata", "precedents")

func witnessHost(f *fixture, c WitnessConfig) {
	if c.Precedents == "" {
		c.Precedents = testPrecedents
	}
	f.host.DoneGates = []DoneGate{c.Gate()}
}

// startWitnessed starts a witnessed run and has its first step's agent hand
// back claims, with notes.md in its folder for evidence.
func (f *fixture) startWitnessed(t *testing.T, claims []any) (store.Run, store.Agent) {
	t.Helper()
	var run store.Run
	json.NewDecoder(f.startRun(t, witnessedWorkflow, "the notes "+sentinel).Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	writeFile(t, filepath.Join(a.Dir, "notes.md"), "# notes\nthe cap is 2\nthe faxbridge client is exported\n")
	res := f.callDone(t, a.ID, map[string]any{"text": "written " + sentinel, "parts": []any{map[string]any{"kind": "claims", "body": map[string]any{"claims": claims}}}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	return run, a
}

var witnessClaims = []any{
	map[string]any{"id": "cap", "text": "the default cap is 2", "kind": "fact", "evidence": []string{"notes.md:2"}},
	map[string]any{"id": "fax", "text": "faxbridge is an active provider", "kind": "fact", "evidence": []string{"notes.md:3"}},
}

func (f *fixture) witnessOf(t *testing.T, producerID string) store.Agent {
	t.Helper()
	return f.helperWithRole(t, producerID, WitnessRole)
}

func (f *fixture) callTool(t *testing.T, agentID, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := f.connect(t, agentID).CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) toolNames(t *testing.T, agentID string) []string {
	t.Helper()
	res, err := f.connect(t, agentID).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestWitnessHoldsThenPasses(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)

	if !w.Headless || w.RunID != run.ID || w.Step != "write" {
		t.Fatalf("witness = %+v", w)
	}
	argv := f.argvOf(t, w)
	if !runsHeadless(argv) || argv[slices.Index(argv, "--model")+1] != DefaultWitnessModel {
		t.Fatalf("argv = %q", argv)
	}
	for _, want := range []string{"the default cap is 2", "faxbridge is an active provider", "notes.md:2", ">     2* the cap is 2"} {
		if !strings.Contains(w.Prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, w.Prompt)
		}
	}
	if strings.Contains(w.Prompt, sentinel) {
		t.Fatal("the witness saw the producer's context")
	}
	if names := f.toolNames(t, w.ID); !slices.Contains(names, "verdict") || slices.Contains(names, "spawn") || slices.Contains(names, "await") {
		t.Fatalf("witness tools = %v", names)
	}
	time.Sleep(30 * time.Millisecond)
	if r := f.run(t, run.ID); r.Steps[0].State != store.StateBusy || r.Steps[1].State != store.StatePending {
		t.Fatalf("run moved on before the verdicts: %+v", r.Steps)
	}

	res := f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "cap", "verdict": "holds", "note": "line 2 says so"})
	if res.IsError || !strings.Contains(resultText(res), "Still without a verdict: fax") {
		t.Fatalf("verdict = %q", resultText(res))
	}
	res = f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "fax", "verdict": "unverifiable", "note": "exported is not used"})
	if res.IsError || !strings.Contains(resultText(res), "call done") {
		t.Fatalf("verdict = %q", resultText(res))
	}
	if res := f.callDone(t, w.ID, map[string]any{"text": "1 holds, 1 unverifiable"}); res.IsError {
		t.Fatal(resultText(res))
	}
	next := f.stepAgent(t, run.ID, "publish")
	if !strings.Contains(next.Prompt, `"id":"cap"`) || !strings.Contains(next.Prompt, `[verdicts {"verdicts":[{"id":"cap","verdict":"holds","note":"line 2 says so"},{"id":"fax","verdict":"unverifiable"`) {
		t.Fatalf("next prompt = %q", next.Prompt)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	if cs[0].Verdict != store.VerdictHolds || cs[1].Verdict != store.VerdictUnverifiable || cs[0].WitnessID != w.ID {
		t.Fatalf("claims = %+v", cs)
	}
	if got := f.agent(t, w.ID); got.State != store.StateDone {
		t.Fatalf("witness state = %s", got.State)
	}
	evs := f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"witness.started", "witness.finished", "gate.passed"}})
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	noOwedWitness(t, f, a.ID)
	noTyping(t, f.keeper)
}

// noOwedWitness fails unless every witness handoff of producerID is marked
// collected, so an await of a reopened producer never returns one.
func noOwedWitness(t *testing.T, f *fixture, producerID string) {
	t.Helper()
	waitFor(t, "witness handoff to be collected", func() bool {
		owed, err := f.store.Uncollected(producerID)
		return err == nil && len(owed) == 0
	})
}

func TestWitnessCheckPolicy(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{MaxClaims: 2})
	var run store.Run
	json.NewDecoder(f.startRun(t, witnessedWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	writeFile(t, filepath.Join(a.Dir, "notes.md"), "one\ntwo\n")
	claims := func(cs ...map[string]any) map[string]any {
		return map[string]any{"text": "x", "parts": []any{map[string]any{"kind": "claims", "body": map[string]any{"claims": cs}}}}
	}
	ok := map[string]any{"text": "fine", "evidence": []string{"notes.md:2"}}
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"text": "no claims"}, "this step is witnessed: add a claims part"},
		{claims(ok, ok, ok), "3 claims; a witnessed step hands back at most 2"},
		{claims(map[string]any{"id": "bare", "text": "no evidence"}), "claim bare has no evidence"},
		{claims(map[string]any{"text": "far", "evidence": []string{"notes.md:99999"}}), "points past the end"},
		{claims(map[string]any{"text": "abs", "evidence": []string{"/no/such/file.go:3"}}), "does not exist"},
	} {
		res := f.callDone(t, a.ID, tc.args)
		if !strings.Contains(errText(res), tc.want) {
			t.Fatalf("want %q, got %q", tc.want, resultText(res))
		}
	}
	if got := f.agent(t, a.ID); store.Finished(got.State) {
		t.Fatal("a refused done finished the agent")
	}
	res := f.callDone(t, a.ID, claims(
		map[string]any{"text": "url", "evidence": []string{"https://example.com/x"}},
		map[string]any{"text": "elsewhere", "evidence": []string{"cmd: git log -1", "other/repo/file.go:12"}},
	))
	if res.IsError {
		t.Fatal(resultText(res))
	}
	f.witnessOf(t, a.ID)
}

func TestBlockedWitnessedStepIsNotHeld(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	var run store.Run
	json.NewDecoder(f.startRun(t, witnessedWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	if res := f.callDone(t, a.ID, map[string]any{"text": "which repo?", "outcome": "blocked"}); res.IsError {
		t.Fatal(resultText(res))
	}
	waitFor(t, "run to end blocked", func() bool { return f.run(t, run.ID).State == store.StateBlocked })
}

func TestUnwitnessedStepIsNotHeld(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	if res := f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}}); res.IsError {
		t.Fatal(resultText(res))
	}
	f.stepAgent(t, run.ID, "publish")
	if agents, _ := f.store.Agents(); slices.ContainsFunc(agents, isWitness) {
		t.Fatal("a witness started on an unwitnessed step")
	}
}

func TestWitnessAllWitnessesStepsWithClaims(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{All: true, Model: "haiku", EvidenceOptional: true})
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	if res := f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}}); res.IsError {
		t.Fatal(resultText(res))
	}
	w := f.witnessOf(t, a.ID)
	if argv := f.argvOf(t, w); argv[slices.Index(argv, "--model")+1] != "haiku" {
		t.Fatalf("argv = %q", argv)
	}
}

func TestWitnessTimeoutMarksUnwitnessed(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{Timeout: "100ms"})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	waitFor(t, "run to move on", func() bool { return f.run(t, run.ID).Steps[1].State == store.StateBusy })
	cs, _ := f.store.AgentClaims(a.ID)
	for _, c := range cs {
		if c.Verdict != store.VerdictUnwitnessed || c.VerdictNote != "no witness verdict within 100ms" {
			t.Fatalf("claim = %+v", c)
		}
	}
	if got := f.agent(t, w.ID); got.State != store.StateKilled {
		t.Fatalf("witness state = %s", got.State)
	}
	if evs := f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"gate.timeout"}}); len(evs) != 1 {
		t.Fatalf("events = %+v", evs)
	}
	noTyping(t, f.keeper)
}

func TestKilledRunKillsWitness(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	if _, err := f.host.KillRun(run.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hold to be cancelled", func() bool {
		return len(f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"gate.cancelled"}})) == 1
	})
	if got := f.agent(t, w.ID); got.State != store.StateKilled {
		t.Fatalf("witness state = %s", got.State)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateKilled || r.Steps[1].State != store.StatePending {
		t.Fatalf("run = %+v", r)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	if cs[0].Verdict != store.VerdictUnwitnessed {
		t.Fatalf("claims = %+v", cs)
	}
}

func TestWitnessIsIdempotent(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	c := WitnessConfig{Precedents: testPrecedents}
	_, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)

	// A second Hold of the same done, as after a restart, picks up the
	// running witness rather than starting another.
	second := make(chan error, 1)
	go func() { second <- f.host.RunWitness(context.Background(), c, a, nil) }()
	time.Sleep(50 * time.Millisecond)
	f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "cap", "verdict": "holds", "note": "seen"})
	f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "fax", "verdict": "contradicted", "note": "seen"})
	f.callDone(t, w.ID, map[string]any{"text": "done"})
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if err := f.host.RunWitness(context.Background(), c, a, nil); err != nil {
		t.Fatal(err)
	}
	agents, _ := f.store.Agents()
	if n := len(slices.DeleteFunc(agents, func(x store.Agent) bool { return !isWitness(x) })); n != 1 {
		t.Fatalf("%d witnesses", n)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	if cs[1].Verdict != store.VerdictContradicted {
		t.Fatalf("claims = %+v", cs)
	}
}

func TestWitnessSkipsJudgedClaims(t *testing.T) {
	f := newFixture(t)
	var run store.Run
	json.NewDecoder(f.startRun(t, twoStepPartsWorkflow, "x").Body).Decode(&run)
	a := f.stepAgent(t, run.ID, "write")
	f.callDone(t, a.ID, map[string]any{"text": "x", "parts": []any{map[string]any{"kind": "claims", "body": twoClaims}}})
	cs, _ := f.store.AgentClaims(a.ID)
	if err := f.host.SetVerdict(cs[0].ID, store.VerdictHolds, "judged before", "w0"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.host.RunWitness(ctx, WitnessConfig{Precedents: NoPrecedents}, a, nil) }()
	w := f.witnessOf(t, a.ID)
	var in WitnessInput
	b, _ := os.ReadFile(filepath.Join(f.host.Layout.ScratchDir(w.ID), "witness-input.json"))
	if err := json.Unmarshal(b, &in); err != nil || len(in.Claims) != 1 || in.Claims[0].ID != "c2" {
		t.Fatalf("input = %s (%v)", b, err)
	}
	cancel()
	<-done
	cs, _ = f.store.AgentClaims(a.ID)
	if cs[0].Verdict != store.VerdictHolds || cs[0].VerdictNote != "judged before" || cs[1].Verdict != store.VerdictUnwitnessed {
		t.Fatalf("claims = %+v", cs)
	}
}

func TestVerdictToolIsForWitnessesOnly(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	if names := f.toolNames(t, a.ID); slices.Contains(names, "verdict") {
		t.Fatalf("producer tools = %v", names)
	}
	_ = run

	// A helper a lead names witness gets the tool, but only on its parent's
	// claims, and a lead still at work has none.
	lead := f.start(t)
	out := f.spawn(context.Background(), t, f.connect(t, lead.ID), WitnessRole)
	fake := f.helperOf(t, lead.ID)
	res := f.callTool(t, fake.ID, "verdict", map[string]any{"claim_id": "cap", "verdict": "contradicted", "note": "x"})
	if !strings.Contains(errText(res), `no claim "cap"`) {
		t.Fatalf("res = %q", resultText(res))
	}
	cs, _ := f.store.AgentClaims(a.ID)
	res = f.callTool(t, fake.ID, "verdict", map[string]any{"claim_id": cs[0].ID, "verdict": "contradicted", "note": "x"})
	if !res.IsError {
		t.Fatal("a stranger judged another agent's claim")
	}
	f.callDone(t, fake.ID, map[string]any{"text": "x"})
	<-out

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"claim_id": "cap", "verdict": "unwitnessed", "note": "x"}, "is not one of"},
		{map[string]any{"claim_id": "cap", "verdict": "holds", "note": " "}, "no note"},
		{map[string]any{"claim_id": "nope", "verdict": "holds", "note": "x"}, `no claim "nope"`},
	} {
		if res := f.callTool(t, w.ID, "verdict", tc.args); !strings.Contains(errText(res), tc.want) {
			t.Fatalf("want %q, got %q", tc.want, resultText(res))
		}
	}
}

func TestStalePrecedentIsFlaggedNotApplied(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, []any{
		map[string]any{"id": "fax", "text": "faxbridge is an active provider", "evidence": []string{"notes.md:3"}},
		map[string]any{"id": "branch", "text": "the dev-ledger branch is abandoned", "evidence": []string{"cmd: git branch"}},
	})
	w := f.witnessOf(t, a.ID)
	input, err := os.ReadFile(filepath.Join(f.host.Layout.ScratchDir(w.ID), "witness-input.md"))
	if err != nil {
		t.Fatal(err)
	}
	p01, _ := os.ReadFile(filepath.Join(testPrecedents, "p01-dev-ledger-project-branch.md"))
	p03, _ := os.ReadFile(filepath.Join(testPrecedents, "p03-faxbridge-not-used.md"))
	body := func(b []byte) string {
		_, after, _ := strings.Cut(string(b), "\n---\n")
		return strings.TrimSpace(after)
	}
	in := string(input)
	if !strings.Contains(in, body(p01)[:60]) {
		t.Fatalf("fresh precedent missing:\n%s", in)
	}
	if strings.Contains(in, body(p03)[:40]) || strings.Contains(w.Prompt, body(p03)[:40]) {
		t.Fatalf("stale precedent's text reached the witness:\n%s", in)
	}
	if !strings.Contains(in, "## Stale precedents: flagged, do not apply") || !strings.Contains(in, "- p03-faxbridge-not-used (review_by 2026-11-01)") {
		t.Fatalf("stale precedent not flagged:\n%s", in)
	}
	res := f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "fax", "verdict": "contradicted", "note": "p03", "precedent_ids": []string{"p03-faxbridge-not-used"}})
	if !strings.Contains(errText(res), "is stale: do not apply it") {
		t.Fatalf("res = %q", resultText(res))
	}
	res = f.callTool(t, w.ID, "verdict", map[string]any{"claim_id": "branch", "verdict": "contradicted", "note": "project branch", "precedent_ids": []string{"p01-dev-ledger-project-branch"}})
	if res.IsError {
		t.Fatal(resultText(res))
	}
	cs, _ := f.store.AgentClaims(a.ID)
	if cs[1].VerdictNote != "project branch [precedents: p01-dev-ledger-project-branch]" {
		t.Fatalf("note = %q", cs[1].VerdictNote)
	}
	evs := f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{"precedent.stale"}})
	if len(evs) != 1 || !strings.Contains(string(evs[0].Data), "p03-faxbridge-not-used") {
		t.Fatalf("events = %+v", evs)
	}
}

func TestWitnessPromptStaysSmall(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	var claims []any
	long := strings.Repeat("a long and winding claim about the system ", 20)
	for i := range DefaultWitnessMaxClaims {
		claims = append(claims, map[string]any{"text": fmt.Sprintf("%d %s", i, long), "evidence": []string{"notes.md:1-3"}})
	}
	_, a := f.startWitnessed(t, claims)
	w := f.witnessOf(t, a.ID)
	if len(w.Prompt) > 4096 {
		t.Fatalf("prompt is %d bytes", len(w.Prompt))
	}
	md := filepath.Join(f.host.Layout.ScratchDir(w.ID), "witness-input.md")
	if !strings.Contains(w.Prompt, md) {
		t.Fatalf("prompt does not point at the input:\n%s", w.Prompt)
	}
	var in WitnessInput
	b, _ := os.ReadFile(filepath.Join(f.host.Layout.ScratchDir(w.ID), "witness-input.json"))
	if err := json.Unmarshal(b, &in); err != nil || len(in.Claims) != DefaultWitnessMaxClaims {
		t.Fatalf("json input: %v %d", err, len(in.Claims))
	}
}

var replayDir = filepath.Join("testdata", "witness-replay")

// TestReplayCasesMatchTheirPrecedents shows what the matcher alone finds
// for each case; replays inject the case's precedent_ids instead.
func TestReplayCasesMatchTheirPrecedents(t *testing.T) {
	f := newFixture(t)
	cases, err := LoadReplayCases(filepath.Join(replayDir, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	c := WitnessConfig{Precedents: filepath.Join(replayDir, "precedents")}
	for _, rc := range cases {
		cl := store.Claim{ID: "x-0", Key: "k", Text: rc.ClaimText}
		var xs []string
		for _, e := range rc.Evidence {
			cl.Evidence = append(cl.Evidence, e.evidenceRef())
			xs = append(xs, e.Excerpt)
		}
		in := f.host.witnessInput(c, store.Agent{ID: "x"}, []store.Claim{cl}, &WitnessGiven{Excerpts: map[string][]string{"x-0": xs}}, time.Now())
		var got []string
		for _, p := range append(in.Precedents, in.StalePrecedents...) {
			got = append(got, p.ID)
		}
		for _, want := range rc.PrecedentIDs {
			if !slices.Contains(got, want) {
				t.Logf("%s: the matcher misses %s", rc.ID, want)
			}
		}
		for _, p := range in.Precedents {
			if slices.Contains(rc.ExpectStaleFlag, p.ID) {
				t.Errorf("%s: stale %s injected", rc.ID, p.ID)
			}
		}
		t.Logf("%s: fresh %v stale %v", rc.ID, in.Precedents, in.StalePrecedents)
	}
}

// TestReplayPlumbing runs the replay through real tmux and fake claude,
// whose witness finds everything unverifiable: it checks the plumbing, not
// the witness, so only the three unverifiable cases pass.
func TestReplayPlumbing(t *testing.T) {
	h := newTmuxHost(t)
	h.MaxAgents = 4
	cases, err := LoadReplayCases(filepath.Join(replayDir, "cases"))
	if err != nil {
		t.Fatal(err)
	}
	results := h.Replay(context.Background(), cases, WitnessConfig{Precedents: filepath.Join(replayDir, "precedents")}, ReplayOptions{Parallel: 4, Timeout: 30 * time.Second})
	r := Score(results, 8, []string{"w02", "w04", "w09"})
	if len(r.Results) != 10 || r.Passed != 3 || r.OK {
		t.Fatalf("report: passed %d of %d, ok %v: %+v", r.Passed, len(r.Results), r.OK, r.Results)
	}
	for _, res := range r.Results {
		if res.Verdict != store.VerdictUnverifiable || res.Note != "fake witness" {
			t.Fatalf("%s: %s %q %v", res.Case.ID, res.Verdict, res.Note, res.Problems)
		}
		if res.Case.ID == "w04-faxbridge-active-stale-precedent" && (!res.Pass || !slices.Contains(res.Stale, "p03-faxbridge-not-used") || len(res.Injected) != 0) {
			t.Fatalf("w04: %+v", res)
		}
		var want []string
		for _, p := range res.Case.PrecedentIDs {
			if !slices.Contains(res.Case.ExpectStaleFlag, p) {
				want = append(want, p)
			}
		}
		if !slices.Equal(res.Injected, want) {
			t.Fatalf("%s: injected %v, want %v", res.Case.ID, res.Injected, want)
		}
	}
	agents, _ := h.Store.Agents()
	for _, a := range agents {
		if isWitness(a) && a.State != store.StateDone {
			t.Fatalf("witness %s is %s", a.ID, a.State)
		}
	}
}

func TestEndedRunCancelsWitness(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	if _, err := f.host.EndRun(run.ID, "test", false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "hold to be cancelled", func() bool {
		return len(f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{store.KindGateCancelled}})) == 1
	})
	if got := f.agent(t, w.ID); got.State != store.StateKilled {
		t.Fatalf("witness = %s/%s", got.State, got.Reason)
	}
	r := f.run(t, run.ID)
	if r.State != store.StateDone || r.Reason != store.ReasonEnded || r.Steps[1].State != store.StatePending {
		t.Fatalf("run = %+v", r)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	for _, c := range cs {
		if c.Verdict != store.VerdictUnwitnessed || c.VerdictNote != "the run ended before the witness finished" {
			t.Fatalf("claim = %+v", c)
		}
	}
	// The hold stopped waiting first, so the killed witness's handoff is
	// dropped: never typed into its finished producer.
	noTyping(t, f.keeper)
	if evs := f.kinds(t, store.EventQuery{AgentID: w.ID, Kinds: []string{store.KindHandoffTyped, store.KindHandoffDelivered}}); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}
	noOwedWitness(t, f, a.ID)
}

func TestStalledWitnessLeavesClaimsUnwitnessed(t *testing.T) {
	f := newFixture(t)
	f.host.StallAfter = 50 * time.Millisecond
	witnessHost(f, WitnessConfig{})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	f.waitStarted(t, w.ID)
	f.hook(t, w.ID, "UserPromptSubmit", struct{}{})
	time.Sleep(80 * time.Millisecond)
	f.host.Sweep()
	waitFor(t, "run to move on", func() bool { return f.run(t, run.ID).Steps[1].State == store.StateBusy })
	if got := f.agent(t, w.ID); got.State != store.StateKilled || got.Reason != store.ReasonLost {
		t.Fatalf("witness = %s/%s", got.State, got.Reason)
	}
	cs, _ := f.store.AgentClaims(a.ID)
	for _, c := range cs {
		if c.Verdict != store.VerdictUnwitnessed || !strings.Contains(c.VerdictNote, "the witness was stopped (lost)") {
			t.Fatalf("claim = %+v", c)
		}
	}
	noTyping(t, f.keeper)
}

// A quiet witness whose Hold times out about now is the Hold's to stop: the
// watchdog does not race it, and the claims say the timeout ran out.
func TestWatchdogLeavesAWitnessToItsTimeout(t *testing.T) {
	f := newFixture(t)
	f.host.StallAfter = 50 * time.Millisecond
	witnessHost(f, WitnessConfig{Timeout: "1s"})
	run, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	f.waitStarted(t, w.ID)
	f.hook(t, w.ID, "UserPromptSubmit", struct{}{})
	waitFor(t, "the hold to register", func() bool { _, ok := witnessHolds.Load(w.ID); return ok })
	time.Sleep(80 * time.Millisecond)
	f.host.Sweep()
	if got := f.agent(t, w.ID); store.Finished(got.State) {
		t.Fatalf("the watchdog stopped the witness: %s/%s", got.State, got.Reason)
	}
	waitFor(t, "run to move on", func() bool { return f.run(t, run.ID).Steps[1].State == store.StateBusy })
	cs, _ := f.store.AgentClaims(a.ID)
	for _, c := range cs {
		if c.Verdict != store.VerdictUnwitnessed || c.VerdictNote != "no witness verdict within 1s" {
			t.Fatalf("claim = %+v", c)
		}
	}
	if evs := f.kinds(t, store.EventQuery{RunID: run.ID, Kinds: []string{store.KindAgentStalled}}); len(evs) != 0 {
		t.Fatalf("stalled events = %+v", evs)
	}
}
