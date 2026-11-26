package host

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// onFinishScript writes an executable shell script with body and returns
// its path and the file it may write stdin to.
func onFinishScript(t *testing.T, body string) (script, out string) {
	t.Helper()
	dir := t.TempDir()
	script, out = filepath.Join(dir, "on-finish"), filepath.Join(dir, "entry.json")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, out
}

func readEntry(t *testing.T, path string) InboxEntry {
	t.Helper()
	var e InboxEntry
	waitFor(t, "on_finish to write its entry", func() bool {
		b, err := os.ReadFile(path)
		return err == nil && json.Unmarshal(b, &e) == nil
	})
	return e
}

func TestOnFinishGetsTheInboxEntry(t *testing.T) {
	f := newFixture(t)
	script, out := onFinishScript(t, `cat > "$1.tmp" && mv "$1.tmp" "$1"`)
	f.host.OnFinish = []string{script, out}
	run := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "slack"})
	build := f.stepAgent(t, run.ID, "build")
	f.done(t, build.ID, "built it")

	e := readEntry(t, out)
	if e.RunID != run.ID || e.State != store.StateDone || e.Step != "build" || e.Handoff != "built it" || e.Source != "slack" {
		t.Fatalf("entry %+v", e)
	}
	if entries := f.inbox(t, ""); len(entries) != 1 || entries[0].RunID != run.ID {
		t.Fatalf("inbox after on_finish: %+v", entries)
	}
}

func TestOnFinishOnKilledRunHasItsStep(t *testing.T) {
	f := newFixture(t)
	script, out := onFinishScript(t, `cat > "$1.tmp" && mv "$1.tmp" "$1"`)
	f.host.OnFinish = []string{script, out}
	run := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a"})
	f.stepAgent(t, run.ID, "build")
	if _, err := f.host.KillRun(run.ID); err != nil {
		t.Fatal(err)
	}
	if e := readEntry(t, out); e.RunID != run.ID || e.State != store.StateKilled || e.Step != "build" {
		t.Fatalf("entry %+v", e)
	}
}

func TestOnFinishFailureIsLogged(t *testing.T) {
	logs := captureLog(t)
	f := newFixture(t)
	script, _ := onFinishScript(t, "cat >/dev/null; echo first >&2; echo token ghp_abcdef >&2; exit 3")
	f.host.OnFinish = []string{script}
	run := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a"})
	f.done(t, f.stepAgent(t, run.ID, "build").ID, "ok")

	want := "on_finish run " + run.ID + ": exit status 3"
	waitFor(t, "the failure in the log", func() bool { return strings.Contains(logs.String(), want) })
	got := logs.String()
	if !strings.Contains(got, "first") || !strings.Contains(got, "ghp_[redacted]") || strings.Contains(got, "abcdef") {
		t.Fatalf("log %q", got)
	}
	if r := f.run(t, run.ID); r.State != store.StateDone || r.AckedAt != nil {
		t.Fatalf("run after a failed on_finish: %+v", r)
	}
}

func TestOnFinishHangDoesNotHoldTheQueue(t *testing.T) {
	logs := captureLog(t)
	f := newFixture(t)
	f.host.MaxRuns = 1
	f.host.OnFinishTimeout = 200 * time.Millisecond
	script, _ := onFinishScript(t, "echo stuck >&2; exec sleep 30")
	f.host.OnFinish = []string{script}
	first := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "a", Source: "slack"})
	build := f.stepAgent(t, first.ID, "build")
	queued := f.postRunOK(t, RunRequest{Workflow: oneStepWorkflow, Goal: "b", Source: "slack"})

	f.done(t, build.ID, "ok")
	f.stepAgent(t, queued.ID, "build")
	if strings.Contains(logs.String(), "on_finish run "+first.ID) {
		t.Fatal("queued run waited for on_finish")
	}

	want := "on_finish run " + first.ID + ": killed after 200ms"
	waitFor(t, "the timeout in the log", func() bool { return strings.Contains(logs.String(), want) })
	if !strings.Contains(logs.String(), "stuck") {
		t.Fatalf("log %q", logs.String())
	}
	if entries := f.inbox(t, ""); len(entries) != 1 || entries[0].RunID != first.ID {
		t.Fatalf("inbox after a hung on_finish: %+v", entries)
	}
}
