package host

import (
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const trustDialog = `────────────────────────────────────────
 Accessing workspace:
 /home/u/dev/repo/.qa-worktrees/x
 Quick safety check: Is this a project you created or one you trust?
 ❯ No, exit
   Yes, I trust this folder
 Enter to confirm · Esc to cancel
`

func (f *fixture) launchSweep(t *testing.T, at time.Time) []store.Event {
	t.Helper()
	agents, err := f.store.LiveAgents()
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := f.keeper.List()
	live := map[string]bool{}
	for _, s := range sessions {
		live[s.Name] = true
	}
	f.host.launchWatch(at, agents, live)
	return f.eventsOf(t, store.KindAgentLaunchBlocked)
}

func TestLaunchWatchReportsTrustDialogOnce(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.keeper.setScreen(a.TmuxSession, trustDialog)
	if evs := f.launchSweep(t, a.CreatedAt.Add(DefaultLaunchStallAfter-time.Second)); len(evs) != 0 {
		t.Fatalf("reported before the grace ran out: %v", evs)
	}
	at := a.CreatedAt.Add(DefaultLaunchStallAfter + 2*time.Second)
	evs := f.launchSweep(t, at)
	if len(evs) != 1 || evs[0].AgentID != a.ID {
		t.Fatalf("events: %+v", evs)
	}
	got := decode[store.AgentLaunchBlocked](t, evs[0])
	if got.Match != "Accessing workspace:" || got.WaitedS != int(DefaultLaunchStallAfter.Seconds())+2 {
		t.Fatalf("event: %+v", got)
	}
	if evs := f.launchSweep(t, at.Add(time.Minute)); len(evs) != 1 {
		t.Fatalf("reported again: %d events", len(evs))
	}
	if cur, _ := f.store.Agent(a.ID); store.Finished(cur.State) {
		t.Fatalf("the watch ended the agent: %+v", cur)
	}
	select {
	case k := <-f.keeper.typed:
		t.Fatalf("the watch typed into %s: %q", k.name, k.text)
	default:
	}
}

func TestLaunchWatchQuietCases(t *testing.T) {
	f := newFixture(t)
	healthy := f.start(t)
	f.keeper.setScreen(healthy.TmuxSession, "❯ Try \"edit <filepath> to...\"\n")
	hooked := f.start(t)
	f.keeper.setScreen(hooked.TmuxSession, trustDialog)
	if err := f.store.Hook(hooked.ID, "SessionStart", time.Now()); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Hour)
	if evs := f.launchSweep(t, at); len(evs) != 0 {
		t.Fatalf("events: %+v", evs)
	}
	f.host.LaunchStallAfter = -1
	f.keeper.setScreen(healthy.TmuxSession, trustDialog)
	if evs := f.launchSweep(t, at); len(evs) != 0 {
		t.Fatalf("reported while off: %+v", evs)
	}
}

func TestBlockingPrompt(t *testing.T) {
	for screen, want := range map[string]string{
		trustDialog: "Accessing workspace:",
		"  Do you trust the files in this folder?\n": "Do you trust the files in this folder?",
		"Select login method:\n ❯ 1. Claude account": "Select login method:",
		"\n> working on it\n":                        "",
	} {
		if got := blockingPrompt(screen); got != want {
			t.Errorf("blockingPrompt(%.40q) = %q, want %q", screen, got, want)
		}
	}
	long := blockingPrompt(strings.Repeat("Press Enter to continue ", 40))
	if !strings.HasPrefix(long, "Press Enter to continue") || len(long) > launchMatchLimit+32 {
		t.Fatalf("long match not cut: %d bytes", len(long))
	}
}
