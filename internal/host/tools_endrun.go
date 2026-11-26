package host

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func init() {
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, _ string, a store.Agent) { h.addEndRunTool(s, a) })
}

// addEndRunTool offers a run's step agent end_run: done that also ends the
// run, so no later step starts. Helpers and agents outside a run do not get
// it.
func (h *Host) addEndRunTool(s *mcp.Server, a store.Agent) {
	if a.ParentID != "" || a.RunID == "" {
		return
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "end_run",
		Description: "Hand back your result, as done does, and end the whole run: no later step starts and any helper still running is stopped. " +
			"Use it when the person says the run is over. List every git worktree that holds work; your session closes right after the call returns.",
		InputSchema: inputSchema[DoneInput](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in DoneInput) (*mcp.CallToolResult, any, error) {
		// Checked before any helper is killed, so a bad call costs nothing.
		call, err := h.checkDoneInput(a.ID, in)
		if err != nil {
			return nil, nil, err
		}
		e, err := h.beginEnd(a.RunID, "lead")
		if errors.Is(err, ErrEnding) {
			// The person is ending the run already and asked this lead to
			// hand back: end_run is then just done.
			return h.finishDone(call)
		}
		if err != nil {
			return nil, nil, err
		}
		res, out, err := h.finishDone(call)
		if err != nil && e != nil {
			h.dropEnding(a.RunID, e)
		}
		return res, out, err
	})
}
