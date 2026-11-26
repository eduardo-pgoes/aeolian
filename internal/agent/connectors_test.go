package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

const mcpListOut = `Checking MCP server health…

claude.ai Slack: https://mcp.slack.com/mcp - ✔ Connected
claude.ai Gmail: https://gmailmcp.googleapis.com/mcp/v1 - ✔ Connected
claude.ai Google Drive: https://drivemcp.googleapis.com/mcp/v1 - ✔ Connected
claude.ai monday.com: https://mcp.monday.com/mcp - ! Needs authentication
claude.ai Linear: https://mcp.linear.app/mcp - ✔ Connected
plugin:figma:figma: https://mcp.figma.com/mcp (HTTP) - ✔ Connected
linear: https://mcp.linear.app/mcp (HTTP) - ✔ Connected
local-tool: npx -y some-server - ✗ Failed to connect
`

func TestParseMCPList(t *testing.T) {
	got := ParseMCPList(mcpListOut)
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	want := []string{"claude.ai Slack", "claude.ai Gmail", "claude.ai Google Drive", "claude.ai monday.com", "claude.ai Linear", "plugin:figma:figma", "linear", "local-tool"}
	if !slices.Equal(names, want) {
		t.Fatalf("names %q", names)
	}
	if !got[0].Connected() || got[3].Connected() || got[7].Connected() {
		t.Fatalf("status %+v", got)
	}
}

func TestConnectorDeny(t *testing.T) {
	deny, err := ConnectorDeny(ParseMCPList(mcpListOut), []string{"slack", "google_drive"}, []string{"mcp__linear"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"mcp__claude_ai_Gmail", "mcp__claude_ai_Linear", "mcp__claude_ai_monday_com", "mcp__local-tool", "mcp__plugin_figma_figma"}
	if !slices.Equal(deny, want) {
		t.Fatalf("deny %q", deny)
	}
}

func TestConnectorDenyRefuses(t *testing.T) {
	servers := ParseMCPList(mcpListOut)
	if _, err := ConnectorDeny(servers, []string{"notion"}, nil); err == nil || !strings.Contains(err.Error(), `"notion"`) {
		t.Errorf("missing connector: %v", err)
	}
	if _, err := ConnectorDeny(servers, []string{"monday_com"}, nil); err == nil || !strings.Contains(err.Error(), "Needs authentication") {
		t.Errorf("unauthenticated connector: %v", err)
	}
}

func TestLayoutConnectorDeny(t *testing.T) {
	old := ListMCP
	defer func() { ListMCP = old }()
	var gotDir, gotClaude string
	ListMCP = func(_ context.Context, claudeDir, dir string) (string, error) {
		gotClaude, gotDir = claudeDir, dir
		return mcpListOut, nil
	}
	l := Layout{Home: "/h", ClaudeDir: "/c"}
	if acc, err := l.ConnectorDeny(context.Background(), "/w", Setup{}); acc.On || acc.Deny != nil || err != nil || gotDir != "" {
		t.Fatalf("no connectors ran mcp list: %+v %v", acc, err)
	}
	acc, err := l.ConnectorDeny(context.Background(), "/w", Setup{Connectors: []string{"google-drive"}, Linear: true})
	if err != nil || !acc.On {
		t.Fatal(acc, err)
	}
	deny := acc.Deny
	if gotDir != "/w" || gotClaude != "/c" {
		t.Errorf("ran in %q with %q", gotDir, gotClaude)
	}
	if slices.Contains(deny, "mcp__linear") || slices.Contains(deny, "mcp__claude_ai_Google_Drive") || !slices.Contains(deny, "mcp__claude_ai_Slack") {
		t.Errorf("deny %q", deny)
	}
}

func TestDefaultConnectorsFailSoft(t *testing.T) {
	old := ListMCP
	defer func() { ListMCP = old }()
	listErr := error(nil)
	ListMCP = func(context.Context, string, string) (string, error) {
		return mcpListOut + "claude.ai Claude Docs: https://api.anthropic.com/v1/pages/mcp - ✔ Connected\n", listErr
	}
	l := Layout{Home: "/h"}
	ctx := context.Background()
	docs := "mcp__claude_ai_Claude_Docs"

	acc, err := l.ConnectorDeny(ctx, "/w", Setup{DefaultConnectors: []string{"claude_docs"}})
	if err != nil || !acc.On || len(acc.Skipped) != 0 || slices.Contains(acc.Deny, docs) || !slices.Contains(acc.Deny, "mcp__claude_ai_Slack") || slices.Contains(acc.Deny, "mcp__aeolian") {
		t.Fatalf("docs default: %+v %v", acc, err)
	}

	for _, missing := range []string{"notion", "monday_com"} {
		acc, err := l.ConnectorDeny(ctx, "/w", Setup{DefaultConnectors: []string{missing}})
		if err != nil || acc.On || acc.Deny != nil || len(acc.Skipped) != 1 || !strings.Contains(acc.Skipped[0].Error(), missing) {
			t.Errorf("soft %s: %+v %v", missing, acc, err)
		}
		if _, err := l.ConnectorDeny(ctx, "/w", Setup{Connectors: []string{missing}}); err == nil {
			t.Errorf("explicit %s launched", missing)
		}
	}

	acc, err = l.ConnectorDeny(ctx, "/w", Setup{Connectors: []string{"slack"}, DefaultConnectors: []string{"notion", "claude_docs"}})
	if err != nil || !acc.On || len(acc.Skipped) != 1 || slices.Contains(acc.Deny, docs) || slices.Contains(acc.Deny, "mcp__claude_ai_Slack") || !slices.Contains(acc.Deny, "mcp__claude_ai_Gmail") {
		t.Fatalf("mixed: %+v %v", acc, err)
	}

	listErr = errors.New("timed out")
	acc, err = l.ConnectorDeny(ctx, "/w", Setup{DefaultConnectors: []string{"claude_docs"}})
	if err != nil || acc.On || len(acc.Skipped) != 1 {
		t.Fatalf("list failed, default only: %+v %v", acc, err)
	}
	if _, err := l.ConnectorDeny(ctx, "/w", Setup{Connectors: []string{"slack"}, DefaultConnectors: []string{"claude_docs"}}); err == nil {
		t.Fatal("list failed, explicit grant launched")
	}
}

func TestArgvConnectors(t *testing.T) {
	l := Layout{Home: "/h"}
	argv := l.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p", Connectors: true, Deny: []string{"mcp__claude_ai_Gmail"}})
	if slices.Contains(argv, "--strict-mcp-config") {
		t.Errorf("connectors kept strict mode: %q", argv)
	}
	i := slices.Index(argv, "--disallowedTools")
	if i < 0 || argv[i+1] != "Agent,Task,mcp__claude_ai_Gmail" {
		t.Errorf("deny list %q", argv)
	}
	if !slices.Contains(l.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p"}), "--strict-mcp-config") {
		t.Error("no connectors dropped strict mode")
	}
}
