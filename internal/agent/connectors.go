package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

const ConnectorPrefix = "claude.ai "

type MCPServer struct {
	Name   string
	Status string
}

func (s MCPServer) Connected() bool { return strings.Contains(s.Status, "Connected") }

var toolUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

func ServerTool(name string) string { return "mcp__" + toolUnsafe.ReplaceAllString(name, "_") }

func connectorKey(name string) string {
	return strings.ToLower(strings.ReplaceAll(toolUnsafe.ReplaceAllString(name, "_"), "-", "_"))
}

var mcpListLine = regexp.MustCompile(`^(.+?): .* - (\S+ .+)$`)

func ParseMCPList(out string) []MCPServer {
	var servers []MCPServer
	for line := range strings.Lines(out) {
		m := mcpListLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		servers = append(servers, MCPServer{Name: m[1], Status: m[2]})
	}
	return servers
}

func connectorReady(servers []MCPServer, key string) error {
	for _, s := range servers {
		if name, ok := strings.CutPrefix(s.Name, ConnectorPrefix); ok && connectorKey(name) == key {
			if !s.Connected() {
				return fmt.Errorf("connector %q is %s: connect it on claude.ai or run claude mcp login", s.Name, s.Status)
			}
			return nil
		}
	}
	return fmt.Errorf("account has no claude.ai connector matching %q", key)
}

func ConnectorDeny(servers []MCPServer, granted, kept []string) ([]string, error) {
	for _, g := range granted {
		if err := connectorReady(servers, g); err != nil {
			return nil, err
		}
	}
	var deny []string
	for _, s := range servers {
		tool := ServerTool(s.Name)
		if slices.Contains(kept, tool) {
			continue
		}
		if name, ok := strings.CutPrefix(s.Name, ConnectorPrefix); ok && slices.Contains(granted, connectorKey(name)) {
			continue
		}
		deny = append(deny, tool)
	}
	slices.Sort(deny)
	return slices.Compact(deny), nil
}

const MCPListTimeout = time.Minute

var ListMCP = func(ctx context.Context, claudeDir, dir string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, MCPListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "mcp", "list")
	cmd.Dir = dir
	cmd.Env = os.Environ()
	if claudeDir != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CONFIG_DIR="+claudeDir)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude mcp list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

type ConnectorAccess struct {
	Deny    []string
	On      bool
	Skipped []error
}

func (l Layout) ConnectorDeny(ctx context.Context, dir string, s Setup) (ConnectorAccess, error) {
	var acc ConnectorAccess
	if len(s.Connectors) == 0 && len(s.DefaultConnectors) == 0 {
		return acc, nil
	}
	out, err := ListMCP(ctx, l.ClaudeDir, dir)
	if err != nil {
		if len(s.Connectors) > 0 {
			return acc, err
		}
		acc.Skipped = append(acc.Skipped, fmt.Errorf("default connectors %s left out: %w", strings.Join(s.DefaultConnectors, ", "), err))
		return acc, nil
	}
	servers := ParseMCPList(out)
	var granted []string
	for _, c := range s.Connectors {
		granted = append(granted, connectorKey(c))
	}
	for _, c := range s.DefaultConnectors {
		key := connectorKey(c)
		if slices.Contains(granted, key) {
			continue
		}
		if err := connectorReady(servers, key); err != nil {
			acc.Skipped = append(acc.Skipped, fmt.Errorf("default connector %s left out: %w", c, err))
			continue
		}
		granted = append(granted, key)
	}
	if len(granted) == 0 {
		return acc, nil
	}
	kept := []string{ServerTool("aeolian")}
	if s.Linear {
		kept = append(kept, ServerTool("linear"))
	}
	for _, name := range s.MCP {
		kept = append(kept, ServerTool(name))
	}
	deny, err := ConnectorDeny(servers, granted, kept)
	if err != nil {
		return acc, fmt.Errorf("claudeai grant: %w", err)
	}
	acc.Deny, acc.On = deny, true
	return acc, nil
}
