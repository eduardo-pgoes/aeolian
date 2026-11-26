package host

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type fakeScopes struct {
	mu      sync.Mutex
	results map[string]string
	asked   []string
}

func (s *fakeScopes) install(f *fixture) {
	f.host.ScopeOf = func(pid int) (string, error) { return "run-test.scope", nil }
	f.host.ScopeResult = func(unit string) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.asked = append(s.asked, unit)
		r, ok := s.results[unit]
		if !ok {
			return "", errors.New("unit not found")
		}
		return r, nil
	}
}

func TestSweepReportsOOM(t *testing.T) {
	for _, c := range []struct {
		name, src, result  string
		idle               bool
		agentState, reason string
		runState           string
	}{
		{"busy interactive", testWorkflow, "oom-kill", false, store.StateKilled, store.ReasonOOM, store.StateFailed},
		{"idle interactive", testWorkflow, "oom-kill", true, store.StateKilled, store.ReasonOOM, store.StateFailed},
		{"headless", headlessWorkflow, "oom-kill", false, store.StateFailed, store.ReasonOOM, store.StateFailed},
		{"clean exit stays lost", testWorkflow, "success", false, store.StateKilled, store.ReasonLost, store.StateFailed},
		{"clean idle exit stays closed", testWorkflow, "success", true, store.StateKilled, store.ReasonClosed, store.StateDone},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			scopes := &fakeScopes{results: map[string]string{"run-test.scope": c.result}}
			scopes.install(f)
			run, build := f.startHeadless(t, c.src)
			f.host.Sweep()
			if c.idle {
				if err := f.store.Hook(build.ID, "Stop", time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			f.keeper.vanish(build.TmuxSession)
			f.host.Sweep()
			f.host.Sweep()
			a := f.agent(t, build.ID)
			if a.State != c.agentState || a.Reason != c.reason {
				t.Fatalf("agent: %s/%s", a.State, a.Reason)
			}
			if r := f.run(t, run.ID); r.State != c.runState || r.Reason != c.reason {
				t.Fatalf("run: %s/%s", r.State, r.Reason)
			}
			if c.reason == store.ReasonOOM && (a.Handoff == nil || !strings.Contains(a.Handoff.Text, "out-of-memory") || !strings.Contains(a.Handoff.Text, "1G")) {
				t.Fatalf("handoff: %+v", a.Handoff)
			}
		})
	}
}

func TestSweepWithoutKnownScopeStaysLost(t *testing.T) {
	f := newFixture(t)
	scopes := &fakeScopes{results: map[string]string{"run-test.scope": "oom-kill"}}
	scopes.install(f)
	f.host.ScopeOf = func(int) (string, error) { return "", errors.New("no scope") }
	run, build := f.startHeadless(t, testWorkflow)
	f.host.Sweep()
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	if a := f.agent(t, build.ID); a.Reason != store.ReasonLost {
		t.Fatalf("agent reason %s", a.Reason)
	}
	if r := f.run(t, run.ID); r.Reason != store.ReasonLost {
		t.Fatalf("run reason %s", r.Reason)
	}
	if len(scopes.asked) != 0 {
		t.Fatalf("asked systemd about %q", scopes.asked)
	}
}

func TestSweepForgetsScopesOfEndedAgents(t *testing.T) {
	f := newFixture(t)
	(&fakeScopes{results: map[string]string{"run-test.scope": "oom-kill"}}).install(f)
	_, build := f.startHeadless(t, testWorkflow)
	f.host.Sweep()
	f.keeper.vanish(build.TmuxSession)
	f.host.Sweep()
	f.host.Sweep()
	f.host.mu.Lock()
	defer f.host.mu.Unlock()
	if _, ok := f.host.scopes[build.ID]; ok {
		t.Fatal("ended agent's scope kept")
	}
}

func TestCgroupUnitNamesOwnScope(t *testing.T) {
	p, err := cgroupPath(os.Getpid())
	if err != nil {
		t.Skip("no cgroup v2: ", err)
	}
	unit, err := cgroupUnit(os.Getpid())
	if !strings.HasSuffix(p, ".scope") {
		if err == nil {
			t.Fatalf("%s named as scope %s", p, unit)
		}
		return
	}
	if err != nil || !strings.HasSuffix(p, "/"+unit) {
		t.Fatalf("unit %q of %s: %v", unit, p, err)
	}
	if _, err := cgroupUnit(0); err == nil {
		t.Fatal("pid 0 read")
	}
}
