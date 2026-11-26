package host

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// ackedRun stores a one-step run whose agent worked in its scratch folder,
// left a file there and a log, finished in state and was acked at acked
// (never, when nil).
func (f *fixture) ackedRun(t *testing.T, id, state string, acked *time.Time) store.Agent {
	t.Helper()
	at := time.Now().Add(-30 * 24 * time.Hour)
	if err := f.store.CreateRun(store.Run{ID: id, Goal: "g", CreatedAt: at, Steps: []store.Step{{Name: "s"}}}); err != nil {
		t.Fatal(err)
	}
	l := f.host.Layout
	a := store.Agent{ID: "agent-" + id, RunID: id, Step: "s", Dir: l.ScratchDir("agent-" + id), State: store.StateBusy}
	if err := f.store.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(a.Dir, "notes.md"), l.Log(a.ID)} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{
		f.store.StartStep(id, 0, a.ID, at),
		f.store.EndAgent(a.ID, store.End{State: store.StateKilled, Reason: store.ReasonKilled, At: at}),
		f.store.FinishStep(id, 0, store.End{State: state, At: at}),
		f.store.FinishRun(id, store.End{State: state, At: at}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if acked != nil {
		if err := f.store.Ack(id, *acked); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func ago(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

func TestGCRespectsAckAndAge(t *testing.T) {
	f := newFixture(t)
	old := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	fresh := f.ackedRun(t, "fresh", store.StateDone, ago(time.Hour))
	unacked := f.ackedRun(t, "unacked", store.StateDone, nil)
	l := f.host.Layout

	rep, err := f.host.GC(time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Runs) != 1 || rep.Runs[0] != "old" || rep.Bytes == 0 {
		t.Fatalf("report: %+v", rep)
	}
	if exists(l.ScratchDir(old.ID)) || exists(l.ConfigDir(old.ID)) {
		t.Fatal("old run's folders kept")
	}
	if exists(l.KeptLog(old.ID)) {
		t.Fatal("a done run's log kept")
	}
	for _, a := range []store.Agent{fresh, unacked} {
		if !exists(l.ScratchDir(a.ID)) || !exists(l.ConfigDir(a.ID)) {
			t.Fatalf("%s's folders removed", a.ID)
		}
	}
	if a, _ := f.store.Agent(old.ID); a.GCAt == nil {
		t.Fatal("gc_at not set")
	}
	if a, _ := f.store.Agent(fresh.ID); a.GCAt != nil {
		t.Fatal("fresh agent marked collected")
	}
	if evs := f.eventsOf(t, "gc.swept"); len(evs) != 1 || evs[0].RunID != "old" {
		t.Fatalf("events: %+v", evs)
	}
	rep, _ = f.host.GC(time.Now(), false)
	if len(rep.Runs) != 0 {
		t.Fatalf("second pass collected again: %+v", rep)
	}

	f.host.GCAfter = 30 * time.Minute
	if rep, _ := f.host.GC(time.Now(), false); len(rep.Runs) != 1 || rep.Runs[0] != "fresh" {
		t.Fatalf("gc_after not honoured: %+v", rep)
	}
}

func TestGCDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	a := f.ackedRun(t, "old", store.StateFailed, ago(8*24*time.Hour))
	var rep GCReport
	if code := f.post(t, "/api/gc?dry_run=1", &rep); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !rep.DryRun || len(rep.Agents) != 1 || len(rep.Logs) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	l := f.host.Layout
	if !exists(l.ScratchDir(a.ID)) || !exists(l.Log(a.ID)) || exists(l.KeptLog(a.ID)) {
		t.Fatal("dry run changed files")
	}
	if got, _ := f.store.Agent(a.ID); got.GCAt != nil {
		t.Fatal("dry run marked the agent")
	}
	if evs := f.eventsOf(t, "gc.swept"); len(evs) != 0 {
		t.Fatalf("dry run emitted %+v", evs)
	}
}

func TestGCKeepsFailedRunLog(t *testing.T) {
	f := newFixture(t)
	a := f.ackedRun(t, "bad", store.StateFailed, ago(8*24*time.Hour))
	if _, err := f.host.GC(time.Now(), false); err != nil {
		t.Fatal(err)
	}
	l := f.host.Layout
	if exists(l.ConfigDir(a.ID)) || !exists(l.KeptLog(a.ID)) {
		t.Fatal("failed run's log not kept")
	}
}

func TestGCSkipsRunWithLiveAgent(t *testing.T) {
	f := newFixture(t)
	a := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	if err := f.store.Reopen(a.ID); err != nil {
		t.Fatal(err)
	}
	if rep, _ := f.host.GC(time.Now(), false); len(rep.Runs) != 0 || !exists(f.host.Layout.ScratchDir(a.ID)) {
		t.Fatalf("collected a run with a resumed agent: %+v", rep)
	}
}

func TestGCNeverLeavesHome(t *testing.T) {
	f := newFixture(t)
	a := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	outside := t.TempDir()
	keep := filepath.Join(outside, "precious")
	os.WriteFile(keep, []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(f.host.Layout.ScratchDir(a.ID), "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.host.Layout.ConfigDir(a.ID), "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.GC(time.Now(), false); err != nil {
		t.Fatal(err)
	}
	if !exists(keep) {
		t.Fatal("GC followed a symlink out of home")
	}
	if exists(f.host.Layout.ScratchDir(a.ID)) {
		t.Fatal("scratch kept")
	}
	for _, p := range []string{outside, f.host.Layout.Home, filepath.Join(f.host.Layout.Home, "scratch"), filepath.Join(f.host.Layout.Home, "scratch", "..", "x")} {
		if err := removeUnder(filepath.Join(f.host.Layout.Home, "scratch"), p); err == nil {
			t.Errorf("removeUnder accepted %s", p)
		}
	}
	if !exists(outside) || !exists(f.host.Layout.Home) {
		t.Fatal("removed outside scratch")
	}
}

func TestGCUntrustsOnlyOwnDirs(t *testing.T) {
	f := newFixture(t)
	cj := f.withClaudeJSON(t)
	a := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	other := t.TempDir()
	for _, d := range []string{f.host.Layout.ScratchDir(a.ID), other} {
		if err := agent.Trust(cj, d); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.host.GC(time.Now(), false)
	if err != nil || rep.Untrusted != 1 {
		t.Fatalf("gc: %+v %v", rep, err)
	}
	if trusted(t, cj, f.host.Layout.ScratchDir(a.ID)) || !trusted(t, cj, other) {
		t.Fatal("untrusted the wrong dirs")
	}
}

func TestGCKeepsDirtyWorktree(t *testing.T) {
	f := newFixture(t)
	cj := f.withClaudeJSON(t)
	repo := gitRepo(t)
	a := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	var made []store.ManagedWorktree
	for _, b := range []string{"clean", "dirty"} {
		p, err := f.host.planWorktree(a, b, WorktreeRequest{Repo: repo, Branch: b})
		if err != nil {
			t.Fatal(err)
		}
		w, err := f.host.makeWorktree(a, p)
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, w)
	}
	os.WriteFile(filepath.Join(made[1].Path, "wip.go"), []byte("package x"), 0o644)

	rep, err := f.host.GC(time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Removed) != 1 || rep.Removed[0] != made[0].Path || len(rep.Kept) != 1 || rep.Kept[0].Reason != store.WorktreeDirty {
		t.Fatalf("report: %+v", rep)
	}
	if exists(made[0].Path) || !exists(filepath.Join(made[1].Path, "wip.go")) {
		t.Fatal("wrong worktree removed")
	}
	for _, b := range []string{"clean", "dirty"} {
		if exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/"+b).Run() != nil {
			t.Fatalf("branch %s deleted", b)
		}
	}
	if trusted(t, cj, made[0].Path) || !trusted(t, cj, made[1].Path) {
		t.Fatal("trust entries wrong")
	}
	wts, _ := f.store.ManagedWorktrees("old")
	if wts[0].State != store.WorktreeRemoved || wts[1].State != store.WorktreeKept || wts[1].Reason != store.WorktreeDirty {
		t.Fatalf("rows: %+v", wts)
	}
	if evs := f.eventsOf(t, "worktree.kept"); len(evs) != 1 {
		t.Fatalf("kept events: %+v", evs)
	}

	f.host.GC(time.Now(), false)
	if evs := f.eventsOf(t, "worktree.kept"); len(evs) != 1 {
		t.Fatalf("kept emitted again: %+v", evs)
	}
	os.Remove(filepath.Join(made[1].Path, "wip.go"))
	f.host.GC(time.Now(), false)
	if exists(made[1].Path) {
		t.Fatal("cleaned worktree not removed on a later pass")
	}
}

func TestResumeAfterGC410(t *testing.T) {
	f := newFixture(t)
	a := f.ackedRun(t, "old", store.StateDone, ago(8*24*time.Hour))
	f.host.GC(time.Now(), false)
	if code := f.post(t, "/api/agents/"+a.ID+"/resume", nil); code != http.StatusGone {
		t.Fatalf("resume: %d", code)
	}
}

func TestTidyRunRemovesOnlyEmptyScratch(t *testing.T) {
	f := newFixture(t)
	l := f.host.Layout
	if err := f.store.CreateRun(store.Run{ID: "r", Goal: "g", Steps: []store.Step{{Name: "s"}}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []store.Agent{
		{ID: "empty", RunID: "r", Dir: l.ScratchDir("empty"), State: store.StateBusy},
		{ID: "full", RunID: "r", Dir: l.ScratchDir("full"), State: store.StateBusy},
		{ID: "live", RunID: "r", Dir: l.ScratchDir("live"), State: store.StateBusy},
	} {
		f.store.CreateAgent(a)
		os.MkdirAll(a.Dir, 0o755)
		if a.ID != "live" {
			f.store.EndAgent(a.ID, store.End{State: store.StateKilled, Reason: store.ReasonKilled})
		}
	}
	os.WriteFile(filepath.Join(l.ScratchDir("full"), "x"), nil, 0o644)
	f.host.tidyRun("r")
	if exists(l.ScratchDir("empty")) || !exists(l.ScratchDir("full")) || !exists(l.ScratchDir("live")) {
		t.Fatal("tidy removed the wrong folders")
	}
}
