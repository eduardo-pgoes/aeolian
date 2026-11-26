package host

import (
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// extraTools add MCP tools to an agent's server after done and spawn. A
// feature appends to it from an init in its own file, so it never edits
// server; a func decides from a which agents get its tool. It runs once
// per agent, when the agent's server is first built.
var extraTools []func(h *Host, s *mcp.Server, id string, a store.Agent)

// extraRoutes add HTTP routes to the host's mux, the same way.
var extraRoutes []func(h *Host, mux *http.ServeMux)

func init() {
	extraRoutes = append(extraRoutes, func(h *Host, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/agents/{agent}/parts", h.getParts)
		mux.HandleFunc("GET /api/runs/{run}/claims", h.listClaims)
	})
}

func (h *Host) getParts(w http.ResponseWriter, r *http.Request) {
	a, err := h.Store.Agent(r.PathValue("agent"))
	if err != nil {
		httpError(w, err)
		return
	}
	parts := []store.Part{}
	if a.Handoff != nil && a.Handoff.Parts != nil {
		parts = a.Handoff.Parts
	}
	writeJSON(w, http.StatusOK, parts)
}

func (h *Host) listClaims(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	if _, err := h.Store.Run(id); err != nil {
		httpError(w, err)
		return
	}
	cs, err := h.Store.Claims(id)
	if err != nil {
		httpError(w, err)
		return
	}
	if cs == nil {
		cs = []store.Claim{}
	}
	writeJSON(w, http.StatusOK, cs)
}
