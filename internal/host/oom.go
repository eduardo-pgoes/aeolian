package host

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type scopeSeen struct {
	pid  int
	unit string
}

func cgroupPath(pid int) (string, error) {
	if pid <= 0 {
		return "", errors.New("no pid")
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", err
	}
	var p string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "0::"); ok {
			p = v
		}
	}
	if p == "" {
		return "", errors.New("no cgroup v2 entry")
	}
	return p, nil
}

func cgroupUnit(pid int) (string, error) {
	p, err := cgroupPath(pid)
	if err != nil {
		return "", err
	}
	unit := path.Base(p)
	if !strings.HasSuffix(unit, ".scope") {
		return "", fmt.Errorf("pid %d is in %s, not a scope", pid, p)
	}
	return unit, nil
}

func systemdResult(unit string) (string, error) {
	out, err := exec.Command("systemctl", "--user", "show", unit, "--property", "Result", "--value").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (h *Host) noteScopes(agents []store.Agent, pids map[string]int, ended []store.Agent) {
	of := h.ScopeOf
	if of == nil {
		of = cgroupUnit
	}
	gone := map[string]bool{}
	for _, a := range ended {
		gone[a.ID] = true
	}
	h.mu.Lock()
	prev := h.scopes
	h.mu.Unlock()
	next := map[string]scopeSeen{}
	for _, a := range agents {
		if store.Finished(a.State) || gone[a.ID] {
			continue
		}
		old, known := prev[a.ID]
		pid, live := pids[a.TmuxSession]
		if !live {
			if known {
				next[a.ID] = old
			}
			continue
		}
		if known && old.pid == pid {
			next[a.ID] = old
			continue
		}
		unit, err := of(pid)
		if err != nil {
			continue
		}
		next[a.ID] = scopeSeen{pid: pid, unit: unit}
	}
	h.mu.Lock()
	h.scopes = next
	h.mu.Unlock()
}

func (h *Host) oomKilled(id string) (string, bool) {
	h.mu.Lock()
	seen, ok := h.scopes[id]
	h.mu.Unlock()
	if !ok {
		return "", false
	}
	result := h.ScopeResult
	if result == nil {
		result = systemdResult
	}
	r, err := result(seen.unit)
	if err != nil || r != "oom-kill" {
		return "", false
	}
	return seen.unit, true
}

func (h *Host) oomEnd(a store.Agent, unit string) (store.End, string) {
	state := store.StateKilled
	if a.Headless {
		state = store.StateFailed
	}
	_, limit, err := agent.MemoryLimits(h.memory(a))
	if err != nil {
		limit = agent.MemoryMax
	}
	why := fmt.Sprintf("the out-of-memory killer ended the session at its %s memory limit (%s), before it called done; give its step or role a larger memory:", limit, unit)
	return store.End{State: state, Reason: store.ReasonOOM}, why
}
