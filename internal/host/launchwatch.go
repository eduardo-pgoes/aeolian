package host

import (
	"log"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const DefaultLaunchStallAfter = 30 * time.Second

var blockingPrompts = []string{
	"Accessing workspace",
	"Is this a project you",
	"Do you trust the files in this folder",
	"Yes, I trust this folder",
	"Enter to confirm",
	"Press Enter to continue",
	"Select login method",
	"Please run /login",
	"Invalid API key",
}

const launchMatchLimit = 300

func blockingPrompt(screen string) string {
	lines := strings.Split(screen, "\n")
	for _, p := range blockingPrompts {
		for _, line := range lines {
			if strings.Contains(line, p) {
				return store.CutString(strings.TrimSpace(line), launchMatchLimit)
			}
		}
	}
	return ""
}

func (h *Host) launchWatch(now time.Time, agents []store.Agent, live map[string]bool) {
	after := orDefault(h.LaunchStallAfter, DefaultLaunchStallAfter)
	if after < 0 {
		return
	}
	for _, a := range agents {
		if a.LastHookAt != nil || store.Finished(a.State) || !live[a.TmuxSession] {
			h.launchBlocked.Delete(a.ID)
			continue
		}
		if a.Headless || now.Sub(a.CreatedAt) < after {
			continue
		}
		if _, seen := h.launchBlocked.Load(a.ID); seen {
			continue
		}
		screen, err := h.Keeper.Capture(a.TmuxSession)
		if err != nil {
			log.Printf("launch watch: capture %s: %v", a.ID, err)
			continue
		}
		match := blockingPrompt(screen)
		if match == "" {
			continue
		}
		h.launchBlocked.Store(a.ID, true)
		waited := now.Sub(a.CreatedAt)
		log.Printf("launch watch: agent %s %s has had no hook for %s; its terminal waits on %q", a.ID, a.Role, waited.Round(time.Second), match)
		h.emit(a.RunID, a.ID, store.KindAgentLaunchBlocked, store.AgentLaunchBlocked{V: store.EventVersion, WaitedS: int(waited.Seconds()), Role: a.Role, Match: match})
	}
}
