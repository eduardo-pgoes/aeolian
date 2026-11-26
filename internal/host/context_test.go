package host

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func requestLine(req string, in, out, read, write int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","name":"Bash"}],"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n",
		req, in, out, read, write)
}

func appendLine(t *testing.T, path, s string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func waitUsages(t *testing.T, f *fixture, agentID string, n int) []store.Event {
	t.Helper()
	for range 300 {
		if evs := f.events(t, agentID, store.KindUsage); len(evs) >= n {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fewer than %d usage events for %s", n, agentID)
	return nil
}

func (f *fixture) contextOf(t *testing.T, runID, agentID string) int64 {
	t.Helper()
	for _, r := range f.runs(t) {
		if r.ID != runID {
			continue
		}
		for _, a := range r.Agents {
			if a.ID == agentID {
				return a.Context
			}
		}
	}
	t.Fatalf("agent %s not in run %s", agentID, runID)
	return 0
}

func TestContextLevelFollowsToolCalls(t *testing.T) {
	f := newFixture(t)
	run := f.postRunOK(t, RunRequest{Workflow: testWorkflow, Goal: "g"})
	build := f.stepAgent(t, run.ID, "build")
	if c := f.contextOf(t, run.ID, build.ID); c != 0 {
		t.Fatalf("context before any request: %d", c)
	}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	toolUsed := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Bash", "transcript_path": path}

	steps := []struct {
		lines string
		level int64
	}{
		{requestLine("r1", 10, 500, 1000, 100) + requestLine("r1", 10, 700, 1000, 100), 1110},
		{requestLine("r2", 5, 20, 2000, 50), 2055},
		{`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary"}}` + "\n" + requestLine("r3", 3, 9000, 0, 600), 603},
	}
	for i, s := range steps {
		appendLine(t, path, s.lines)
		f.hook(t, build.ID, "PostToolUse", toolUsed)
		evs := waitUsages(t, f, build.ID, i+1)
		if len(evs) != i+1 {
			t.Fatalf("step %d: %d usage events", i, len(evs))
		}
		if u := decode[store.Usage](t, evs[i]); u.Context != s.level {
			t.Fatalf("step %d: usage event context %d, want %d", i, u.Context, s.level)
		}
		if c := f.contextOf(t, run.ID, build.ID); c != s.level {
			t.Fatalf("step %d: agent view context %d, want %d", i, c, s.level)
		}
	}

	st, err := store.Open(filepath.Join(f.host.Layout.Home, "aeolian.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	restarted := &Host{Store: st, Layout: f.host.Layout, Keeper: f.keeper, URL: f.host.URL, KillGrace: time.Millisecond}
	f.srv.Config.Handler = restarted.Handler()
	if c := f.contextOf(t, run.ID, build.ID); c != 603 {
		t.Fatalf("context after a restart: %d, want 603", c)
	}

	appendLine(t, path, requestLine("r4", 1, 1, 900, 0))
	f.hook(t, build.ID, "PostToolUse", toolUsed)
	evs := waitUsages(t, f, build.ID, 4)
	if u := decode[store.Usage](t, evs[3]); u.Context != 901 || u.Turns != 1 {
		t.Fatalf("usage after a restart: %+v", u)
	}
	if c := f.contextOf(t, run.ID, build.ID); c != 901 {
		t.Fatalf("context after a restart and a tool call: %d", c)
	}
}
