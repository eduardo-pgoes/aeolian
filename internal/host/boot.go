package host

import (
	"fmt"
	"log"
	"slices"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// DefaultBootRoles are the helper roles that boot lean unless config.json
// says otherwise: they check or propose from what they are handed, so the
// user's own setup is noise to them, and a cost on every start.
var DefaultBootRoles = map[string]string{
	WitnessRole: agent.BootLean,
	"judge":     agent.BootLean,
	"proposer":  agent.BootLean,
	"evolver":   agent.BootLean,
}

// DefaultConfined are the roles whose file reads stay inside their own
// folders when config.json names none: they judge another agent's work,
// so what other agents left under $AEOLIAN_HOME must not reach them.
var DefaultConfined = []string{WitnessRole, "judge"}

// BootConfig is the boot section of config.json.
type BootConfig struct {
	// Default is the profile of an agent nothing else decides for. Empty
	// means agent.BootFull.
	Default string `json:"default,omitempty"`
	// Roles sets a profile per helper role, over DefaultBootRoles.
	Roles map[string]string `json:"roles,omitempty"`
	// Confine lists the roles whose reads are confined to their own
	// folders. Nil means DefaultConfined; an empty list confines none.
	Confine []string `json:"confine,omitempty"`
}

func (c BootConfig) Validate() error {
	if err := agent.CheckBoot(c.Default); err != nil {
		return fmt.Errorf("boot.default: %w", err)
	}
	for role, p := range c.Roles {
		if err := agent.CheckBoot(p); err != nil {
			return fmt.Errorf("boot.roles.%s: %w", role, err)
		}
	}
	return nil
}

// helperRole is the helper role of a, or empty for a step or a lone agent.
func helperRole(a store.Agent) string {
	if a.ParentID == "" {
		return ""
	}
	return a.Role
}

// boot decides a's profile: the workflow's step or role boot first, then
// config.json's boot.roles, DefaultBootRoles, boot.default, and full.
func (h *Host) boot(a store.Agent) string {
	role := helperRole(a)
	if a.RunID != "" && !isWitness(a) {
		wf, err := h.wf(a.RunID)
		if err != nil {
			log.Printf("boot for agent %s: %v", a.ID, err)
		} else if p := wf.Boot(a.Step, role); p != "" {
			return p
		}
	}
	if role != "" {
		if p := h.Boot.Roles[role]; p != "" {
			return p
		}
		if p := DefaultBootRoles[role]; p != "" {
			return p
		}
	}
	if h.Boot.Default != "" {
		return h.Boot.Default
	}
	return agent.BootFull
}

// confined reports whether a's reads stay inside its own folders.
func (h *Host) confined(a store.Agent) bool {
	role := helperRole(a)
	if role == "" {
		return false
	}
	confine := h.Boot.Confine
	if confine == nil {
		confine = DefaultConfined
	}
	return slices.Contains(confine, role)
}

// readRoots are the folders a confined agent may read beside its working
// directory: its scratch, where the host leaves its input, and extra.
func (h *Host) readRoots(a store.Agent, extra ...string) []string {
	return append([]string{h.Layout.ScratchDir(a.ID), a.Dir}, extra...)
}
