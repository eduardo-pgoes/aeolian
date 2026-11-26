package host

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

var ErrUnknownAccount = errors.New("unknown account")

func (h *Host) AccountNames() []string {
	return slices.Sorted(maps.Keys(h.Accounts))
}

func (h *Host) unknownAccount(name string) error {
	names := h.AccountNames()
	if len(names) == 0 {
		return fmt.Errorf("account %q: config.json has no accounts: %w", name, ErrUnknownAccount)
	}
	return fmt.Errorf("account %q is not one of %s: %w", name, strings.Join(names, ", "), ErrUnknownAccount)
}

func (h *Host) pickAccount(name string) (string, error) {
	if name == "" {
		name = h.DefaultAccount
	}
	if name == "" {
		return "", nil
	}
	if _, ok := h.Accounts[name]; !ok {
		return "", h.unknownAccount(name)
	}
	return name, nil
}

func (h *Host) runAccount(runID string) (string, error) {
	if runID == "" {
		return "", nil
	}
	if v, ok := h.runAccounts.Load(runID); ok {
		return v.(string), nil
	}
	run, err := h.Store.Run(runID)
	if err != nil {
		return "", err
	}
	h.runAccounts.Store(runID, run.Account)
	return run.Account, nil
}

func (h *Host) accountLayout(name string) (agent.Layout, error) {
	if name == "" {
		return h.Layout, nil
	}
	dir, ok := h.Accounts[name]
	if !ok {
		return h.Layout, h.unknownAccount(name)
	}
	return h.Layout.ForAccount(dir), nil
}

func (h *Host) runLayout(runID string) (agent.Layout, error) {
	name, err := h.runAccount(runID)
	if err != nil {
		return h.Layout, fmt.Errorf("account of run %s: %w", runID, err)
	}
	l, err := h.accountLayout(name)
	if err != nil {
		return l, fmt.Errorf("run %s: %w", runID, err)
	}
	return l, nil
}

func (h *Host) agentLayout(a store.Agent) (agent.Layout, error) {
	return h.runLayout(a.RunID)
}

func (h *Host) transcriptsOf(a store.Agent) string {
	l, err := h.agentLayout(a)
	if err != nil || l.ClaudeDir == "" {
		return h.Transcripts
	}
	return agent.TranscriptsIn(l.ClaudeDir)
}

type AccountsView struct {
	Accounts []string `json:"accounts"`
	Default  string   `json:"default,omitempty"`
}

func (h *Host) listAccounts(w http.ResponseWriter, r *http.Request) {
	names := h.AccountNames()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, AccountsView{Accounts: names, Default: h.DefaultAccount})
}
