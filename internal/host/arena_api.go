package host

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func init() {
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, id string, _ store.Agent) { h.addArenaTools(s, id) })
	extraRoutes = append(extraRoutes, func(h *Host, mux *http.ServeMux) { h.arenaRoutes(mux) })
}

// addArenaTools gives an arena's judges the judgment tool. Nobody else gets
// it: a judge is booked before its launch, so its first MCP request finds
// it here.

func (h *Host) addArenaTools(s *mcp.Server, agentID string) {
	ar := h.arenas().judgeOf(agentID)
	if ar == nil {
		return
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "judgment",
		Description: "Record your verdict on one pair of entries. Call it once per listed pair; " +
			"a pair that is not listed, or one you already judged, is refused.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in JudgmentInput) (*mcp.CallToolResult, any, error) {
		msg, err := ar.recordJudgment(agentID, in)
		if err != nil {
			return nil, nil, err
		}
		h.emit(ar.a.ID, agentID, store.KindArenaJudgment, store.Versioned{V: store.EventVersion})
		return textResult(msg), nil, nil
	})
}

func (h *Host) arenaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/arenas", h.startArena)
	mux.HandleFunc("GET /api/arenas", h.listArenas)
	mux.HandleFunc("GET /api/arenas/{arena}", h.getArena)
}

func (h *Host) startArena(w http.ResponseWriter, r *http.Request) {
	var req ArenaRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	a, err := h.StartArena(req)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrNoRunSlot) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (h *Host) listArenas(w http.ResponseWriter, r *http.Request) {
	out, err := h.Arenas()
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Host) getArena(w http.ResponseWriter, r *http.Request) {
	a, err := h.Arena(r.PathValue("arena"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}
