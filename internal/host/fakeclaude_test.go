package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/digest"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const fakeClaudeEnv = "AEOLIAN_FAKE_CLAUDE"

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeEnv) == "1" {
		os.Exit(fakeClaude(os.Args[1:]))
	}
	keeper.UsePrivateSocket()
	code := m.Run()
	if err := keeper.KillPrivateServer(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func fakeClaude(args []string) int {
	var prompt, settingsPath, mcpPath string
	for i := 0; i+1 < len(args); i++ {
		switch args[i] {
		case "-p":
			prompt = args[i+1]
		case "--settings":
			settingsPath = args[i+1]
		case "--mcp-config":
			mcpPath = args[i+1]
		}
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "fake claude only runs headless")
		return 2
	}
	fmt.Printf("prompt: %s\n", prompt)
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	var mcpConfig struct {
		MCPServers map[string]struct{ URL string } `json:"mcpServers"`
	}
	for path, v := range map[string]any{settingsPath: &settings, mcpPath: &mcpConfig} {
		b, err := os.ReadFile(path)
		if err == nil {
			err = json.Unmarshal(b, v)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	hook := func(event string) {
		for _, h := range settings.Hooks[event] {
			for _, c := range h.Hooks {
				cmd := exec.Command("sh", "-c", c.Command)
				cmd.Stdin = strings.NewReader(`{"hook_event_name":"` + event + `"}`)
				cmd.Run()
			}
		}
	}
	hook("SessionStart")
	hook("UserPromptSubmit")
	defer hook("Stop")

	verb, rest, _ := strings.Cut(prompt, " ")
	if verb == "exit" {
		fmt.Fprintln(os.Stderr, rest)
		return 1
	}
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-claude"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpConfig.MCPServers["aeolian"].URL}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer session.Close()
	call := func(name string, args map[string]any) (string, bool) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return err.Error(), true
		}
		text := resultText(res)
		fmt.Printf("%s: %s\n", name, text)
		return text, res.IsError
	}
	none := []store.Worktree{}
	if strings.HasPrefix(prompt, "You are a witness") {
		return fakeWitness(prompt, call)
	}
	switch verb {
	case "done":
		call("done", map[string]any{"text": rest, "worktrees": none})
	case "blocked":
		call("done", map[string]any{"text": rest, "worktrees": none, "outcome": "blocked"})
	case "spawn":
		got, isErr := call("spawn", map[string]any{"role": rest, "handoff": map[string]any{"text": "review it", "worktrees": none}})
		if isErr {
			return 1
		}
		call("done", map[string]any{"text": got, "worktrees": none})
	case "fanout":
		// fanout <n> <role>: start n helpers without waiting, await them
		// all, hand back what came.
		n, role, _ := strings.Cut(rest, " ")
		count, _ := strconv.Atoi(n)
		for i := range count {
			if _, isErr := call("spawn", map[string]any{"role": role, "handoff": map[string]any{"text": "part " + strconv.Itoa(i), "worktrees": none}, "wait": false}); isErr {
				return 1
			}
		}
		got, isErr := call("await", map[string]any{"mode": "all", "timeout_s": 60})
		if isErr {
			return 1
		}
		call("done", map[string]any{"text": got, "worktrees": none})
	case "claims":
		// claims <text>|<text>...: write notes.md with one line per claim
		// and hand back a claims part, each claim citing its line.
		var claims []any
		var notes strings.Builder
		for i, text := range strings.Split(rest, "|") {
			notes.WriteString(text + "\n")
			claims = append(claims, map[string]any{"id": fmt.Sprintf("c%d", i+1), "text": text, "evidence": []string{fmt.Sprintf("notes.md:%d", i+1)}})
		}
		if err := os.WriteFile("notes.md", []byte(notes.String()), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		parts := []any{map[string]any{"kind": "claims", "body": map[string]any{"claims": claims}}}
		if _, isErr := call("done", map[string]any{"text": "claimed", "worktrees": none, "parts": parts}); isErr {
			return 1
		}
	case "hang":
		// A session alive but making no progress, as a hung claude.
		select {}
	}
	return 0
}

// fakeWitness reads the witness's JSON input and finds every claim
// unverifiable, so a replay through it scores only the plumbing, except a
// claim whose text says FAKE-WRONG or FAKE-HOLDS.
func fakeWitness(prompt string, call func(string, map[string]any) (string, bool)) int {
	_, rest, ok := strings.Cut(prompt, "The same input as JSON: ")
	if !ok {
		fmt.Fprintln(os.Stderr, "no input path in the witness prompt")
		return 1
	}
	path, _, _ := strings.Cut(rest, "\n")
	b, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var in WitnessInput
	if err := json.Unmarshal(b, &in); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for _, c := range in.Claims {
		verdict := "unverifiable"
		switch {
		case strings.Contains(c.Text, "FAKE-WRONG"):
			verdict = "contradicted"
		case strings.Contains(c.Text, "FAKE-HOLDS"):
			verdict = "holds"
		}
		if _, isErr := call("verdict", map[string]any{"claim_id": c.ID, "verdict": verdict, "note": "fake witness"}); isErr {
			return 1
		}
	}
	call("done", map[string]any{"text": fmt.Sprintf("%d unverifiable", len(in.Claims)), "worktrees": []store.Worktree{}})
	return 0
}

type fakeClaudeKeeper struct {
	Tmux
	exe string
}

func (k fakeClaudeKeeper) Start(name, dir string, argv []string) error {
	i := slices.Index(argv, "claude")
	argv = append(append(slices.Clone(argv[:i]), "env", fakeClaudeEnv+"=1", k.exe), argv[i+1:]...)
	return k.Tmux.Start(name, dir, argv)
}

func newTmuxHost(t *testing.T) *Host {
	t.Helper()
	for _, bin := range []string{"tmux", "curl", "systemd-run"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	if err := exec.Command("systemd-run", "--user", "--scope", "--quiet", "true").Run(); err != nil {
		t.Skip("no systemd user scope: ", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	st, err := store.Open(filepath.Join(home, "aeolian.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	t.Cleanup(func() { assertFolds(t, st) })
	h := &Host{Store: st, Layout: agent.Layout{Home: home}, Keeper: fakeClaudeKeeper{exe: exe}, KillGrace: time.Millisecond}
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)
	h.URL = srv.URL
	t.Cleanup(func() {
		agents, _ := st.Agents()
		for _, a := range agents {
			if err := keeper.Kill(a.TmuxSession); err != nil && !errors.Is(err, keeper.ErrNoSession) {
				t.Logf("kill %s: %v", a.TmuxSession, err)
			}
		}
	})
	return h
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); !cond(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func runState(t *testing.T, h *Host, id string) store.Run {
	t.Helper()
	run, err := h.Store.Run(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestHeadlessRunInTmux(t *testing.T) {
	h := newTmuxHost(t)
	goal := `the "parser" $(touch pwned) it's`
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: build
    headless: true
    helpers: [reviewer]
    prompt: "spawn reviewer"
  - name: check
    headless: true
    prompt: "done checked {{.Goal}} after {{.Handoff}}"
roles:
  reviewer:
    prompt: "done reviewed {{.Handoff}}"
`, Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	waitLong(t, "run to finish", func() bool { return runState(t, h, run.ID).State != store.StateBusy })
	r := runState(t, h, run.ID)
	if r.State != store.StateDone || r.Steps[0].State != store.StateDone || r.Steps[1].State != store.StateDone {
		t.Fatalf("run: %+v", r)
	}
	agents, _ := h.Store.Agents()
	if len(agents) != 3 {
		t.Fatalf("%d agents", len(agents))
	}
	for _, a := range agents {
		if a.State != store.StateDone || !a.Headless {
			t.Fatalf("agent %s %s: %+v", a.Step, a.Role, a)
		}
		if b, err := os.ReadFile(h.Layout.Log(a.ID)); err != nil || !strings.Contains(string(b), "prompt: "+a.Prompt+"\n") {
			t.Fatalf("log of %s %s: %q %v", a.Step, a.Role, b, err)
		}
		if _, err := os.Stat(filepath.Join(a.Dir, "pwned")); !os.IsNotExist(err) {
			t.Fatal("goal ran as shell")
		}
	}
	check, _ := h.Store.Agent(r.Steps[1].AgentID)
	for _, want := range []string{"checked " + goal + " after Helper reviewer finished:\n\nreviewed review it"} {
		if !strings.Contains(check.Handoff.Text, want) {
			t.Fatalf("check handoff lacks %q: %q", want, check.Handoff.Text)
		}
	}
	if check.LastHook == "" {
		t.Fatal("no hook reached the host")
	}
}

func TestHeadlessExitFailsRun(t *testing.T) {
	h := newTmuxHost(t)
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: build
    headless: true
    prompt: "exit Credit balance is too low"
  - name: check
    headless: true
    prompt: "done"
`, Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	var build store.Agent
	waitLong(t, "build to start", func() bool {
		build, err = h.Store.Agent(runState(t, h, run.ID).Steps[0].AgentID)
		return err == nil
	})
	waitLong(t, "build to exit", func() bool {
		live, err := keeper.Has(build.TmuxSession)
		return err == nil && !live
	})
	h.Sweep()
	h.Sweep()
	r := runState(t, h, run.ID)
	got, _ := h.Store.Agent(build.ID)
	if got.State != store.StateFailed || r.State != store.StateFailed || r.Steps[0].State != store.StateFailed || r.Steps[1].State != store.StatePending {
		t.Fatalf("agent %s, run %+v", got.State, r)
	}
	if b, _ := os.ReadFile(h.Layout.Log(build.ID)); !strings.HasSuffix(string(b), "Credit balance is too low\n") {
		t.Fatalf("log: %q", b)
	}
}

func TestHeadlessBlockedInTmux(t *testing.T) {
	h := newTmuxHost(t)
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: build
    headless: true
    prompt: "blocked Which repo, api or web?"
  - name: check
    headless: true
    prompt: "done"
`, Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	waitLong(t, "run to finish", func() bool { return runState(t, h, run.ID).State != store.StateBusy })
	r := runState(t, h, run.ID)
	build, _ := h.Store.Agent(r.Steps[0].AgentID)
	if r.State != store.StateBlocked || r.Steps[0].State != store.StateBlocked || r.Steps[1].State != store.StatePending || build.Handoff.Text != "Which repo, api or web?" {
		t.Fatalf("run %+v, build %+v", r, build)
	}
	time.Sleep(100 * time.Millisecond)
	if agents, _ := h.Store.Agents(); len(agents) != 1 {
		t.Fatalf("%d agents after a blocked step", len(agents))
	}
}

func TestHeadlessFanOutInTmux(t *testing.T) {
	h := newTmuxHost(t)
	h.MaxAgents = 4
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: explore
    headless: true
    helpers: [researcher]
    prompt: "fanout 3 researcher"
roles:
  researcher:
    prompt: "done found {{.Handoff}}"
`, Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	waitLong(t, "run to finish", func() bool { return runState(t, h, run.ID).State != store.StateBusy })
	r := runState(t, h, run.ID)
	if r.State != store.StateDone {
		t.Fatalf("run: %+v", r)
	}
	lead, _ := h.Store.Agent(r.Steps[0].AgentID)
	for _, want := range []string{"found part 0", "found part 1", "found part 2"} {
		if !strings.Contains(lead.Handoff.Text, want) {
			t.Fatalf("lead handoff lacks %q: %q", want, lead.Handoff.Text)
		}
	}
	if n := strings.Count(lead.Handoff.Text, "Helper researcher ("); n != 3 {
		t.Fatalf("%d handoffs: %q", n, lead.Handoff.Text)
	}
	// The three ran side by side: each started before any finished.
	agents, _ := h.Store.Agents()
	var firstDone time.Time
	var starts []time.Time
	for _, a := range agents {
		if a.ParentID == "" {
			continue
		}
		starts = append(starts, a.CreatedAt)
		if firstDone.IsZero() || a.DoneAt.Before(firstDone) {
			firstDone = *a.DoneAt
		}
	}
	for _, s := range starts {
		if s.After(firstDone) {
			t.Fatalf("a helper started after another finished: starts %v, first done %v", starts, firstDone)
		}
	}
}

// TestHungHelperInTmux is run 060c55f7: a headless lead blocked in spawn on
// a helper whose claude hung with its session up. The watchdog ends the
// helper and the run goes on.
func TestHungHelperInTmux(t *testing.T) {
	h := newTmuxHost(t)
	h.StallAfter = 300 * time.Millisecond
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: explore
    headless: true
    helpers: [researcher]
    prompt: "spawn researcher"
roles:
  researcher:
    prompt: "hang"
`, Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	var helper store.Agent
	waitLong(t, "helper to start", func() bool {
		agents, _ := h.Store.Agents()
		for _, a := range agents {
			if a.ParentID != "" && a.LastHook == "UserPromptSubmit" {
				helper = a
				return true
			}
		}
		return false
	})
	time.Sleep(400 * time.Millisecond)
	// The first quiet sweep samples the helper's scope CPU; the next sees
	// it hung, using none.
	h.Sweep()
	time.Sleep(200 * time.Millisecond)
	h.Sweep()
	waitLong(t, "run to finish", func() bool { return runState(t, h, run.ID).State != store.StateBusy })
	r := runState(t, h, run.ID)
	lead, _ := h.Store.Agent(r.Steps[0].AgentID)
	if r.State != store.StateDone || !strings.Contains(lead.Handoff.Text, "Helper researcher ended without calling done:") || !strings.Contains(lead.Handoff.Text, "stalled") {
		t.Fatalf("run %+v, lead handoff %q", r, lead.Handoff.Text)
	}
	if got, _ := h.Store.Agent(helper.ID); got.State != store.StateKilled || got.Reason != store.ReasonLost {
		t.Fatalf("helper %s/%s", got.State, got.Reason)
	}
	if live, _ := keeper.Has(helper.TmuxSession); live {
		t.Fatal("hung helper's session still up")
	}
}

// TestWitnessedRunInTmux runs a witnessed step end to end on real tmux: the
// step hands back claims, the witness contradicts one, the next step sees
// the verdicts part, the run delivers, and the digest lists the
// contradicted claim.
func TestWitnessedRunInTmux(t *testing.T) {
	h := newTmuxHost(t)
	h.DoneGates = []DoneGate{WitnessConfig{Precedents: NoPrecedents}.Gate()}
	run, err := h.StartRun(RunRequest{Workflow: `
steps:
  - name: map
    headless: true
    witness: true
    prompt: "claims the cap is 2 FAKE-HOLDS|faxbridge is live FAKE-WRONG"
  - name: write
    headless: true
    prompt: "done saw{{range .Parts}} {{.Kind}}{{end}}: {{range .Parts}}{{if eq .Kind \"verdicts\"}}{{.Text}}{{end}}{{end}}"
`, Goal: "map the providers"})
	if err != nil {
		t.Fatal(err)
	}
	waitLong(t, "run to finish", func() bool { return runState(t, h, run.ID).State != store.StateBusy })
	r := runState(t, h, run.ID)
	if r.State != store.StateDone || r.Reason != store.ReasonDelivered {
		t.Fatalf("run: %+v", r)
	}

	producer, _ := h.Store.Agent(r.Steps[0].AgentID)
	claims, err := h.Store.AgentClaims(producer.ID)
	if err != nil || len(claims) != 2 {
		t.Fatalf("claims %+v %v", claims, err)
	}
	if claims[0].Verdict != store.VerdictHolds || claims[1].Verdict != store.VerdictContradicted {
		t.Fatalf("verdicts: %+v", claims)
	}
	var witness store.Agent
	agents, _ := h.Store.Agents()
	for _, a := range agents {
		if a.Role == WitnessRole {
			witness = a
		}
	}
	if witness.ParentID != producer.ID || witness.State != store.StateDone || !witness.Headless {
		t.Fatalf("witness: %+v", witness)
	}

	write, _ := h.Store.Agent(r.Steps[1].AgentID)
	for _, want := range []string{"saw claims verdicts:", `"id":"c2","verdict":"contradicted"`, `"id":"c1","verdict":"holds"`} {
		if !strings.Contains(write.Handoff.Text, want) {
			t.Fatalf("next step's handoff lacks %q: %q", want, write.Handoff.Text)
		}
	}

	evs, _ := h.Store.Events(store.EventQuery{RunID: run.ID, Kinds: []string{store.KindWitnessStarted, store.KindWitnessFinished, store.KindGatePassed}})
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	// The second gate.passed is the write step's: its done passes the
	// gate at once, as it is not witnessed.
	if !slices.Equal(kinds, []string{store.KindWitnessStarted, store.KindWitnessFinished, store.KindGatePassed, store.KindGatePassed}) {
		t.Fatalf("events %v", kinds)
	}

	resp, err := http.Get(h.URL + "/api/digest?run=" + run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v digest.View
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("digest: %s %v", resp.Status, err)
	}
	var found []digest.Item
	for _, g := range v.Groups {
		for _, it := range g.Items {
			if it.Kind == digest.KindContradicted {
				found = append(found, it)
			}
		}
	}
	if len(found) != 1 || !strings.Contains(found[0].Text, "faxbridge is live") || found[0].Ref != claims[1].ID || found[0].Evidence[0] != "notes.md:2" {
		t.Fatalf("digest contradicted items: %+v (view %+v)", found, v)
	}
}
