package host

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const docsMCPList = `claude.ai Claude Docs: https://api.anthropic.com/v1/pages/mcp - ✔ Connected
claude.ai Slack: https://mcp.slack.com/mcp - ✔ Connected
claude.ai Gmail: https://gmailmcp.googleapis.com/mcp/v1 - ✔ Connected
`

const defaultGrantsWorkflow = `
steps:
  - name: lead
    headless: true
    grants: [claudeai:slack]
    helpers: [plain, scribe]
    prompt: "Lead {{.Goal}}."
roles:
  scribe:
    grants: [claudeai:claude_docs]
    prompt: "Write {{.Handoff}}."
  plain:
    prompt: "Do {{.Handoff}}."
`

func stubMCPList(t *testing.T, out string) {
	t.Helper()
	old := agent.ListMCP
	agent.ListMCP = func(context.Context, string, string) (string, error) { return out, nil }
	t.Cleanup(func() { agent.ListMCP = old })
}

func denied(argv []string) []string {
	i := slices.Index(argv, "--disallowedTools")
	return strings.Split(argv[i+1], ",")
}

func TestDefaultGrantsReachEveryAgent(t *testing.T) {
	stubMCPList(t, docsMCPList)
	f := newFixture(t)
	f.host.DefaultGrants = DefaultGrants
	var run store.Run
	if err := json.NewDecoder(f.startRun(t, defaultGrantsWorkflow, "the parser").Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	lead := f.stepAgent(t, run.ID, "lead")
	argv := f.argvOf(t, lead)
	if slices.Contains(argv, "--strict-mcp-config") || slices.Contains(denied(argv), "mcp__claude_ai_Claude_Docs") || slices.Contains(denied(argv), "mcp__claude_ai_Slack") || !slices.Contains(denied(argv), "mcp__claude_ai_Gmail") {
		t.Fatalf("lead argv %q", argv)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := f.connect(t, lead.ID)
	for _, role := range []string{"plain", "scribe"} {
		f.spawn(ctx, t, session, role)
		h := f.helperWithRole(t, lead.ID, role)
		argv := f.argvOf(t, h)
		if slices.Contains(argv, "--strict-mcp-config") || slices.Contains(denied(argv), "mcp__claude_ai_Claude_Docs") || !slices.Contains(denied(argv), "mcp__claude_ai_Slack") {
			t.Errorf("%s argv %q", role, argv)
		}
		f.done(t, h.ID, "ok")
	}
}

func TestMissingDefaultConnectorLaunchesWithoutIt(t *testing.T) {
	stubMCPList(t, "claude.ai Gmail: https://gmailmcp.googleapis.com/mcp/v1 - ✔ Connected\n")
	logs := captureLog(t)
	f := newFixture(t)
	f.host.DefaultGrants = DefaultGrants
	a := f.start(t)
	argv := f.argvOf(t, a)
	if !slices.Contains(argv, "--strict-mcp-config") || slices.Contains(denied(argv), "mcp__claude_ai_Gmail") {
		t.Fatalf("argv %q", argv)
	}
	if !strings.Contains(logs.String(), "agent "+a.ID+": default connector claude_docs left out") {
		t.Fatalf("no warning:\n%s", logs.String())
	}
}

func TestNoDefaultGrantsKeepsStrictMode(t *testing.T) {
	stubMCPList(t, docsMCPList)
	f := newFixture(t)
	a := f.start(t)
	if !slices.Contains(f.argvOf(t, a), "--strict-mcp-config") {
		t.Fatal("no default grants dropped strict mode")
	}
}
