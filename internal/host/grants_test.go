package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/github"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const secretToken = "ghs_TOPSECRETtoken0123456789"

const grantsWorkflow = `
steps:
  - name: plan
    headless: true
    prompt: "Plan {{.Goal}}."
  - name: publish
    headless: true
    grants: [github, linear]
    helpers: [skeptic, scribe]
    prompt: "Publish {{.Goal}}. {{.Handoff}}"
roles:
  skeptic:
    prompt: "Refute {{.Handoff}}."
  scribe:
    grants: [linear]
    prompt: "Write {{.Handoff}}."
`

var testBot = github.Identity{Name: "acme-agent[bot]", Email: "55501+acme-agent[bot]@users.noreply.github.com"}

type stubToken struct {
	token    string
	err      error
	calls    atomic.Int32
	botErr   error
	botCalls atomic.Int32

	mu    sync.Mutex
	asked []map[string]string
}

func (s *stubToken) Token(_ context.Context, permissions map[string]string) (string, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.asked = append(s.asked, permissions)
	s.mu.Unlock()
	return s.token, s.err
}

func (s *stubToken) Identity(context.Context) (github.Identity, error) {
	s.botCalls.Add(1)
	if s.botErr != nil {
		return github.Identity{}, s.botErr
	}
	return testBot, nil
}

func (s *stubToken) permissions() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.asked)
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return buf
}

const linearEntry = `{"type":"http","url":"https://mcp.linear.app/mcp","headers":{"Authorization":"Bearer lin_api_x"}}`

func newGrantsFixture(t *testing.T, tok *stubToken) *fixture {
	t.Helper()
	f := newFixture(t)
	if tok != nil {
		f.host.GitHub = tok
	}
	if err := os.WriteFile(f.host.Layout.LinearEntry(), []byte(linearEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

type agentFiles struct {
	settings, mcp string
	env           map[string]string
	servers       map[string]json.RawMessage
}

func (f *fixture) files(t *testing.T, id string) agentFiles {
	t.Helper()
	var af agentFiles
	for _, path := range []string{f.host.Layout.Settings(id), f.host.Layout.MCPConfig(id)} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", path, fi.Mode().Perm())
		}
	}
	b, _ := os.ReadFile(f.host.Layout.Settings(id))
	af.settings = string(b)
	var settings struct{ Env map[string]string }
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	af.env = settings.Env
	b, _ = os.ReadFile(f.host.Layout.MCPConfig(id))
	af.mcp = string(b)
	var mcp struct{ MCPServers map[string]json.RawMessage }
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	af.servers = mcp.MCPServers
	return af
}

func (af agentFiles) assertBare(t *testing.T, who string) {
	t.Helper()
	for _, s := range []string{af.settings, af.mcp} {
		for _, bad := range []string{secretToken, agent.BotHelper, "linear", "lin_api_x"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s files contain %q:\n%s", who, bad, s)
			}
		}
	}
	if af.servers["aeolian"] == nil {
		t.Errorf("%s lost the conductor: %s", who, af.mcp)
	}
}

func (af agentFiles) assertInteractive(t *testing.T, who string) {
	t.Helper()
	bad := []string{"SSH_AUTH_SOCK", "GIT_SSH_COMMAND", "GH_CONFIG_DIR", "GITHUB_TOKEN"}
	if af.env["GH_TOKEN"] == "" {
		bad = append(bad, "GH_TOKEN", "GIT_CONFIG")
	}
	for _, b := range bad {
		if strings.Contains(af.settings, b) {
			t.Errorf("%s settings contain %q:\n%s", who, b, af.settings)
		}
	}
}

func (af agentFiles) assertScrubbed(t *testing.T, who string) {
	t.Helper()
	for k, want := range map[string]string{
		"SSH_AUTH_SOCK":       "",
		"GIT_SSH_COMMAND":     agent.NoSSH,
		"GIT_TERMINAL_PROMPT": "0",
		"GITHUB_TOKEN":        "",
		"GIT_CONFIG_KEY_0":    "credential.helper",
		"GIT_CONFIG_VALUE_0":  "",
	} {
		if got, ok := af.env[k]; !ok || got != want {
			t.Errorf("%s env %s = %q (set %v), want %q", who, k, got, ok, want)
		}
	}
	if !strings.HasSuffix(af.env["GH_CONFIG_DIR"], "/gh") {
		t.Errorf("%s GH_CONFIG_DIR = %q", who, af.env["GH_CONFIG_DIR"])
	}
}

func TestGrantsReachOnlyTheGrantedAgent(t *testing.T) {
	logs := captureLog(t)
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	plan := f.stepAgent(t, run.ID, "plan")
	pf := f.files(t, plan.ID)
	pf.assertBare(t, "plan")
	pf.assertScrubbed(t, "plan")
	if pf.env["GH_TOKEN"] != "" || pf.env["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("plan env: %v", pf.env)
	}
	if tok.calls.Load() != 0 {
		t.Fatal("minted for an ungranted step")
	}

	f.callDone(t, plan.ID, map[string]any{"text": "planned"})
	publish := f.stepAgent(t, run.ID, "publish")
	got := f.files(t, publish.ID)
	got.assertScrubbed(t, "publish")
	if got.env["GH_TOKEN"] != secretToken || got.env["GIT_CONFIG_COUNT"] != "3" || got.env["GIT_CONFIG_VALUE_2"] != agent.BotHelper || got.env["MCP_TOOL_TIMEOUT"] == "" {
		t.Fatalf("publish env: %v", got.env)
	}
	if p := tok.permissions(); len(p) != 1 || !maps.Equal(p[0], github.WritePermissions) {
		t.Fatalf("publish asked for %v", p)
	}
	var compact bytes.Buffer
	json.Compact(&compact, got.servers["linear"])
	if compact.String() != linearEntry || got.servers["aeolian"] == nil {
		t.Fatalf("publish servers: %s", got.mcp)
	}
	if tok.calls.Load() != 1 {
		t.Fatalf("minted %d times", tok.calls.Load())
	}

	session := f.connect(t, publish.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.spawn(ctx, t, session, "skeptic")
	skeptic := f.helperOf(t, publish.ID)
	sk := f.files(t, skeptic.ID)
	sk.assertBare(t, "skeptic")
	sk.assertScrubbed(t, "skeptic")
	f.done(t, skeptic.ID, "refuted nothing")

	f.spawn(ctx, t, session, "scribe")
	var scribe store.Agent
	waitFor(t, "scribe to start", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.Role == "scribe" {
				scribe = a
				return true
			}
		}
		return false
	})
	waitFor(t, "scribe files", func() bool {
		_, err := os.Stat(f.host.Layout.Settings(scribe.ID))
		return err == nil
	})
	sc := f.files(t, scribe.ID)
	sc.assertScrubbed(t, "scribe")
	if sc.servers["linear"] == nil || strings.Contains(sc.settings, secretToken) || sc.env["GH_TOKEN"] != "" || strings.Contains(sc.settings, agent.BotHelper) {
		t.Fatalf("scribe: %s %s", sc.settings, sc.mcp)
	}
	if tok.calls.Load() != 1 {
		t.Fatalf("helpers minted: %d", tok.calls.Load())
	}

	agents, _ := f.store.Agents()
	runs, _ := f.host.Runs(store.RunFilter{})
	for what, v := range map[string]any{"agents": agents, "runs": runs} {
		b, _ := json.Marshal(v)
		if strings.Contains(string(b), secretToken) {
			t.Errorf("%s rows hold the token", what)
		}
	}
	if strings.Contains(logs.String(), secretToken) {
		t.Errorf("log holds the token:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "agent "+publish.ID+": minted a github token") {
		t.Errorf("no mint line:\n%s", logs.String())
	}
}

func TestMintFailureFailsTheStep(t *testing.T) {
	for name, c := range map[string]struct {
		tok  *stubToken
		want string
	}{
		"mint error": {&stubToken{err: errors.New("github: installation for acme/api: GET /repos/acme/api/installation: 404 Not Found")}, "404 Not Found"},
		"no github":  {nil, "config.json has no github section"},
		"no linear":  {&stubToken{token: secretToken}, "linear.json"},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLog(t)
			f := newGrantsFixture(t, c.tok)
			if name == "no linear" {
				os.Remove(f.host.Layout.LinearEntry())
			}
			var run store.Run
			if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
				t.Fatal(err)
			}
			plan := f.stepAgent(t, run.ID, "plan")
			f.callDone(t, plan.ID, map[string]any{"text": "planned"})
			waitFor(t, "run to fail", func() bool { return f.run(t, run.ID).State == store.StateFailed })
			r := f.run(t, run.ID)
			if r.Steps[1].State != store.StateFailed || r.Steps[1].AgentID != "" {
				t.Fatalf("publish step: %+v", r.Steps[1])
			}
			if f.keeper.startedCount() != 1 {
				t.Fatalf("started %d agents", f.keeper.startedCount())
			}
			if !strings.Contains(logs.String(), "step publish") || !strings.Contains(logs.String(), c.want) {
				t.Fatalf("log lacks the reason:\n%s", logs.String())
			}
			if strings.Contains(logs.String(), secretToken) {
				t.Fatal("log holds the token")
			}
		})
	}
}

func TestResumeKeepsGrants(t *testing.T) {
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	plan := f.stepAgent(t, run.ID, "plan")
	f.callDone(t, plan.ID, map[string]any{"text": "planned"})
	publish := f.stepAgent(t, run.ID, "publish")
	f.callDone(t, publish.ID, map[string]any{"text": "published"})
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State == store.StateDone })

	if _, err := f.host.Resume(plan.ID); err != nil {
		t.Fatal(err)
	}
	rp := f.files(t, plan.ID)
	rp.assertBare(t, "resumed plan")
	rp.assertInteractive(t, "resumed plan")
	if _, err := f.host.Resume(publish.ID); err != nil {
		t.Fatal(err)
	}
	got := f.files(t, publish.ID)
	if got.env["GH_TOKEN"] != secretToken || got.env["MCP_TOOL_TIMEOUT"] != "" || got.servers["linear"] == nil {
		t.Fatalf("resumed publish: %s %s", got.settings, got.mcp)
	}
	if _, ok := got.env["GIT_SSH_COMMAND"]; ok || got.env["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("resumed publish is scrubbed: %v", got.env)
	}
	if tok.calls.Load() != 2 {
		t.Fatalf("minted %d times", tok.calls.Load())
	}
}

func TestAgentOutsideRunHasNoGrants(t *testing.T) {
	f := newGrantsFixture(t, &stubToken{token: secretToken})
	a := f.start(t)
	af := f.files(t, a.ID)
	af.assertBare(t, "debug agent")
	af.assertInteractive(t, "debug agent")
}

const readWorkflow = `
steps:
  - name: look
    headless: true
    grants: [github-read]
    prompt: "Look at {{.Goal}}."
  - name: both
    headless: true
    grants: [github-read, github]
    prompt: "Ship {{.Goal}}."
`

func TestGitHubReadGrant(t *testing.T) {
	logs := captureLog(t)
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, readWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	look := f.stepAgent(t, run.ID, "look")
	got := f.files(t, look.ID)
	got.assertScrubbed(t, "look")
	if got.env["GH_TOKEN"] != secretToken || got.env["GIT_CONFIG_VALUE_2"] != agent.BotHelper || got.servers["linear"] != nil {
		t.Fatalf("look: %v %s", got.env, got.mcp)
	}
	if p := tok.permissions(); len(p) != 1 || !maps.Equal(p[0], github.ReadPermissions) {
		t.Fatalf("look asked for %v", p)
	}
	if !strings.Contains(logs.String(), "agent "+look.ID+": minted a github-read token") {
		t.Errorf("no mint line:\n%s", logs.String())
	}

	f.callDone(t, look.ID, map[string]any{"text": "looked"})
	both := f.stepAgent(t, run.ID, "both")
	if f.files(t, both.ID).env["GH_TOKEN"] != secretToken {
		t.Fatal("both has no token")
	}
	if p := tok.permissions(); len(p) != 2 || !maps.Equal(p[1], github.WritePermissions) {
		t.Fatalf("both asked for %v", p)
	}
}

const freeWorkflow = `
steps:
  - name: lead
    grants: [linear, github-read]
    helpers: ["*"]
    prompt: "Lead {{.Goal}}."
roles:
  reviewer:
    prompt: "Review {{.Handoff}}."
  scribe:
    grants: [linear]
    prompt: "Write {{.Handoff}}."
`

func (f *fixture) helperWithRole(t *testing.T, parentID, role string) store.Agent {
	t.Helper()
	var found store.Agent
	waitFor(t, role+" to start", func() bool {
		agents, _ := f.store.Agents()
		for _, a := range agents {
			if a.ParentID == parentID && a.Role == role {
				found = a
				_, err := os.Stat(f.host.Layout.Settings(a.ID))
				return err == nil
			}
		}
		return false
	})
	return found
}

func TestAnyRoleHelperInheritsParentGrants(t *testing.T) {
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, freeWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	lead := f.stepAgent(t, run.ID, "lead")
	if f.files(t, lead.ID).servers["linear"] == nil {
		t.Fatal("lead has no linear")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := f.connect(t, lead.ID)

	f.spawn(ctx, t, session, "researcher")
	researcher := f.helperWithRole(t, lead.ID, "researcher")
	got := f.files(t, researcher.ID)
	if got.servers["linear"] == nil || got.env["GH_TOKEN"] != secretToken {
		t.Fatalf("free helper did not inherit: %s %s", got.settings, got.mcp)
	}
	got.assertInteractive(t, "researcher")

	f.spawn(ctx, t, f.connect(t, researcher.ID), "digger")
	digger := f.helperWithRole(t, researcher.ID, "digger")
	if got := f.files(t, digger.ID); got.servers["linear"] == nil || got.env["GH_TOKEN"] != secretToken {
		t.Fatalf("free helper of a free helper did not inherit: %s %s", got.settings, got.mcp)
	}
	f.done(t, digger.ID, "dug")
	f.done(t, researcher.ID, "researched")

	f.spawn(ctx, t, session, "reviewer")
	reviewer := f.helperWithRole(t, lead.ID, "reviewer")
	rf := f.files(t, reviewer.ID)
	rf.assertBare(t, "reviewer")
	rf.assertInteractive(t, "reviewer")
	f.done(t, reviewer.ID, "reviewed")

	f.spawn(ctx, t, session, "scribe")
	scribe := f.helperWithRole(t, lead.ID, "scribe")
	if got := f.files(t, scribe.ID); got.servers["linear"] == nil || strings.Contains(got.settings, secretToken) {
		t.Fatalf("named role got its parent's grants: %s %s", got.settings, got.mcp)
	}
	if tok.calls.Load() != 3 {
		t.Fatalf("minted %d times, want lead, researcher and digger", tok.calls.Load())
	}
	for _, p := range tok.permissions() {
		if !maps.Equal(p, github.ReadPermissions) {
			t.Fatalf("asked for %v", p)
		}
	}
}

var botIdent = []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"}

func (af agentFiles) assertBot(t *testing.T, who string) {
	t.Helper()
	for _, k := range botIdent {
		want := testBot.Name
		if strings.HasSuffix(k, "_EMAIL") {
			want = testBot.Email
		}
		if af.env[k] != want {
			t.Errorf("%s env %s = %q, want %q", who, k, af.env[k], want)
		}
	}
}

func (af agentFiles) assertEngineerCommits(t *testing.T, who string) {
	t.Helper()
	for _, k := range botIdent {
		if strings.Contains(af.settings, k) {
			t.Errorf("%s settings set %s:\n%s", who, k, af.settings)
		}
	}
}

func TestHeadlessAgentsCommitAsTheBot(t *testing.T) {
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	plan := f.stepAgent(t, run.ID, "plan")
	pf := f.files(t, plan.ID)
	pf.assertBot(t, "ungranted plan")
	pf.assertScrubbed(t, "ungranted plan")
	if tok.calls.Load() != 0 {
		t.Fatal("minted for the bot identity")
	}

	f.callDone(t, plan.ID, map[string]any{"text": "planned"})
	publish := f.stepAgent(t, run.ID, "publish")
	got := f.files(t, publish.ID)
	got.assertBot(t, "publish")
	if got.env["GH_TOKEN"] != secretToken || got.env["GIT_CONFIG_VALUE_2"] != agent.BotHelper || got.env["MCP_TOOL_TIMEOUT"] == "" {
		t.Fatalf("publish env lost its grant: %v", got.env)
	}

	session := f.connect(t, publish.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.spawn(ctx, t, session, "skeptic")
	skeptic := f.helperOf(t, publish.ID)
	f.files(t, skeptic.ID).assertBot(t, "headless skeptic")
	f.done(t, skeptic.ID, "refuted nothing")
	f.callDone(t, publish.ID, map[string]any{"text": "published"})
	waitFor(t, "run to end", func() bool { return f.run(t, run.ID).State == store.StateDone })

	if _, err := f.host.Resume(plan.ID); err != nil {
		t.Fatal(err)
	}
	f.files(t, plan.ID).assertEngineerCommits(t, "resumed plan")
	debug := f.start(t)
	f.files(t, debug.ID).assertEngineerCommits(t, "debug agent")
	if n := tok.botCalls.Load(); n != 3 {
		t.Fatalf("asked for the bot identity %d times, want plan, publish and skeptic", n)
	}
}

func TestInteractiveGrantedAgentCommitsAsTheEngineer(t *testing.T) {
	tok := &stubToken{token: secretToken}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, freeWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	lead := f.stepAgent(t, run.ID, "lead")
	got := f.files(t, lead.ID)
	if got.env["GH_TOKEN"] != secretToken {
		t.Fatalf("lead env: %v", got.env)
	}
	got.assertEngineerCommits(t, "interactive lead")
	if tok.botCalls.Load() != 0 {
		t.Fatal("an interactive agent asked for the bot identity")
	}
}

func TestNoGitHubSectionCommitsAsTheEngineer(t *testing.T) {
	f := newGrantsFixture(t, nil)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	pf := f.files(t, f.stepAgent(t, run.ID, "plan").ID)
	pf.assertScrubbed(t, "plan")
	pf.assertEngineerCommits(t, "plan without a github section")
}

func TestUnknownBotIdentityFailsTheLaunch(t *testing.T) {
	logs := captureLog(t)
	tok := &stubToken{token: secretToken, botErr: errors.New("github: user acme-agent[bot]: GET /users/acme-agent[bot]: 403 Forbidden: \"API rate limit exceeded\"")}
	f := newGrantsFixture(t, tok)
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, grantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "run to fail", func() bool { return f.run(t, run.ID).State == store.StateFailed })
	r := f.run(t, run.ID)
	if r.Steps[0].State != store.StateFailed || r.Steps[0].AgentID != "" {
		t.Fatalf("plan step: %+v", r.Steps[0])
	}
	if f.keeper.startedCount() != 0 {
		t.Fatalf("started %d agents", f.keeper.startedCount())
	}
	if tok.calls.Load() != 0 {
		t.Fatal("minted a token for a launch that failed")
	}
	if !strings.Contains(logs.String(), "step plan") || !strings.Contains(logs.String(), "identity is unknown") || !strings.Contains(logs.String(), "API rate limit exceeded") {
		t.Fatalf("log lacks the reason:\n%s", logs.String())
	}
	entries, _ := os.ReadDir(f.host.Layout.Home + "/agents")
	if len(entries) != 0 {
		t.Fatalf("settings written for a failed launch: %v", entries)
	}
}
