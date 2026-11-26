package host

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
)

// The MCP handler is stateless: a notifications/cancelled arrives as its own
// POST on a throwaway session, and the SDK does not end a tool handler when
// the client drops the request either. The host tracks each tools/call
// itself and hands the tool its call through callHeader.
const callHeader = "Aeolian-Call"

type call struct {
	agent, id string
	ctx       context.Context
	cancel    context.CancelFunc
}

type rpcMessage struct {
	Method string          `json:"method"`
	ID     json.RawMessage `json:"id"`
	Params struct {
		RequestID json.RawMessage `json:"requestId"`
	} `json:"params"`
}

// openCall registers a tools/call that lasts as long as ctx and returns the
// token that names it, and a func that unregisters it.
func (h *Host) openCall(ctx context.Context, agentID string, id json.RawMessage) (string, func()) {
	var b [12]byte
	rand.Read(b[:])
	token := hex.EncodeToString(b[:])
	ctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	if h.calls == nil {
		h.calls = map[string]*call{}
	}
	h.calls[token] = &call{agent: agentID, id: rpcID(id), ctx: ctx, cancel: cancel}
	h.mu.Unlock()
	return token, func() {
		cancel()
		h.mu.Lock()
		delete(h.calls, token)
		h.mu.Unlock()
	}
}

func (h *Host) cancelCall(agentID string, id json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.agent == agentID && c.id == rpcID(id) {
			c.cancel()
		}
	}
}

// callDone is closed when the call named by token is cancelled or its
// request ends; nil, which never fires, for an unknown token.
func (h *Host) callDone(token string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.calls[token]; ok {
		return c.ctx.Done()
	}
	return nil
}

func rpcID(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }
