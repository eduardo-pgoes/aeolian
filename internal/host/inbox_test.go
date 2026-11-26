package host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// finishedRun stores a two-step run whose second step ends in state on an
// agent with handoff h, and the run with it.
func (f *fixture) finishedRun(t *testing.T, id, source, state string, at time.Time, h *store.Handoff) store.Agent {
	t.Helper()
	run := store.Run{ID: id, Goal: "goal of " + id, Source: source, Ref: "ref-" + id, CreatedAt: at,
		Steps: []store.Step{{Name: "build"}, {Name: "check"}}}
	if err := f.store.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	a := store.Agent{ID: "agent-" + id, RunID: id, Step: "check", State: store.StateBusy}
	if err := f.store.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	if h != nil {
		if err := f.store.Done(a.ID, *h, store.ReasonDelivered, at); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{
		f.store.StartStep(id, 0, "agent-build-"+id, at),
		f.store.FinishStep(id, 0, store.End{State: store.StateDone, At: at}),
		f.store.StartStep(id, 1, a.ID, at),
		f.store.FinishStep(id, 1, store.End{State: state, At: at}),
		f.store.FinishRun(id, store.End{State: state, At: at}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (f *fixture) inbox(t *testing.T, query string) []InboxEntry {
	t.Helper()
	resp, err := http.Get(f.srv.URL + "/api/inbox" + query)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("inbox: %s", resp.Status)
	}
	var entries []InboxEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func (f *fixture) ack(t *testing.T, id string) (int, store.Run) {
	t.Helper()
	resp, err := http.Post(f.srv.URL+"/api/runs/"+id+"/ack", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var run store.Run
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, run
}

func TestInboxEntries(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	wt := store.Worktree{Repo: "/src/api", Path: "/wt/api", Branch: "fix"}
	f.finishedRun(t, "done", "slack", store.StateDone, now, &store.Handoff{Text: "shipped", Worktrees: []store.Worktree{wt}})
	failed := f.finishedRun(t, "failed", "", store.StateFailed, now.Add(time.Second), nil)
	var lines []string
	for i := range 40 {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	lines[39] = "token ghp_abcdefghijklmnop leaked"
	writeLog(t, f.host, failed.ID, lines)

	entries := f.inbox(t, "")
	if len(entries) != 2 || entries[0].RunID != "done" || entries[1].RunID != "failed" {
		t.Fatalf("inbox %+v", entries)
	}
	d := entries[0]
	if d.Source != "slack" || d.Ref != "ref-done" || d.State != store.StateDone || d.Goal != "goal of done" || d.FinishedAt == nil ||
		d.Step != "check" || d.Handoff != "shipped" || len(d.Worktrees) != 1 || d.Worktrees[0] != wt || len(d.LogTail) != 0 {
		t.Fatalf("done entry %+v", d)
	}
	e := entries[1]
	if e.Step != "check" || e.Handoff != "" || e.Worktrees == nil || len(e.LogTail) != logTailLines {
		t.Fatalf("failed entry %+v", e)
	}
	if e.LogTail[0] != "line 10" || e.LogTail[29] != "token ghp_[redacted] leaked" {
		t.Fatalf("log tail %q", e.LogTail)
	}
	if got := f.inbox(t, "?source=slack"); len(got) != 1 || got[0].RunID != "done" {
		t.Fatalf("slack inbox %+v", got)
	}
	if got := f.inbox(t, "?source=nobody"); got == nil || len(got) != 0 {
		t.Fatalf("empty inbox %+v", got)
	}
}

func TestAck(t *testing.T) {
	f := newFixture(t)
	f.finishedRun(t, "r", "", store.StateBlocked, time.Now(), &store.Handoff{Text: "which repo?"})
	if err := f.store.CreateRun(store.Run{ID: "live", Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	code, run := f.ack(t, "r")
	if code != http.StatusOK || run.ID != "r" || run.AckedAt == nil {
		t.Fatalf("ack: %d %+v", code, run)
	}
	if code, again := f.ack(t, "r"); code != http.StatusOK || !again.AckedAt.Equal(*run.AckedAt) {
		t.Fatalf("second ack: %d %+v", code, again)
	}
	if got := f.inbox(t, ""); len(got) != 0 {
		t.Fatalf("acked run still in the inbox: %+v", got)
	}
	if code, _ := f.ack(t, "live"); code != http.StatusConflict {
		t.Fatalf("ack busy run: %d", code)
	}
	if code, _ := f.ack(t, "nope"); code != http.StatusNotFound {
		t.Fatalf("ack unknown run: %d", code)
	}
	resp, err := http.Get(f.srv.URL + "/api/runs/r")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil || !strings.HasPrefix(fmt.Sprint(raw["acked_at"]), "20") {
		t.Fatalf("run JSON acked_at %v (%v)", raw["acked_at"], err)
	}
}

func writeLog(t *testing.T, h *Host, agentID string, lines []string) {
	t.Helper()
	path := h.Layout.Log(agentID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
