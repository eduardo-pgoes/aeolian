package host

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// WorktreeOutput is what the worktree tool returns.
type WorktreeOutput struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

func init() {
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, _ string, a store.Agent) { h.addWorktreeTool(s, a) })
}

// addWorktreeTool gives a lead, an agent that no other agent spawned, the
// worktree tool: it makes a worktree the lead can hand to several helpers.
// Helpers do not get it; they ask their lead, or get one through spawn.
func (h *Host) addWorktreeTool(s *mcp.Server, a store.Agent) {
	if a.ParentID != "" {
		return
	}
	id := a.ID
	mcp.AddTool(s, &mcp.Tool{
		Name: "worktree",
		Description: "Make a new git worktree on a new branch, owned by the host, and return its path. " +
			"Hand the path to helpers in spawn's handoff worktrees, or pass spawn a worktree request to make one per helper. " +
			"The host removes it after the run is acked and old enough, unless it has uncommitted changes; the branch always stays.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in WorktreeRequest) (*mcp.CallToolResult, any, error) {
		return h.worktreeTool(id, in)
	})
}

func (h *Host) worktreeTool(id string, in WorktreeRequest) (*mcp.CallToolResult, any, error) {
	a, err := h.Store.Agent(id)
	if err != nil {
		return nil, nil, err
	}
	if store.Finished(a.State) {
		return nil, nil, store.ErrFinished
	}
	p, err := h.planWorktree(a, "lead-"+short(agent.NewSessionID()), in)
	if err != nil {
		return nil, nil, err
	}
	w, err := h.makeWorktree(a, p)
	if err != nil {
		return nil, nil, err
	}
	out := WorktreeOutput{Repo: w.Repo, Path: w.Path, Branch: w.Branch}
	b, _ := json.Marshal(out)
	return textResult(string(b)), out, nil
}
