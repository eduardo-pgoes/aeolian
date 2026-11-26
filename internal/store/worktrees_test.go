package store

import (
	"errors"
	"testing"
	"time"
)

func TestManagedWorktrees(t *testing.T) {
	s := open(t)
	w, err := s.CreateManagedWorktree(ManagedWorktree{RunID: "r1", AgentID: "a1", Repo: "/repo", Path: "/wt/1", Branch: "b1", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if w.ID == 0 || w.State != WorktreeActive {
		t.Fatalf("created %+v", w)
	}
	if _, err := s.CreateManagedWorktree(ManagedWorktree{RunID: "r1", Path: "/wt/1"}); err == nil {
		t.Fatal("same path stored twice")
	}
	if _, err := s.CreateManagedWorktree(ManagedWorktree{RunID: "r2", Path: "/wt/2"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ManagedWorktrees("r1")
	if err != nil || len(got) != 1 || got[0].Path != "/wt/1" {
		t.Fatalf("r1: %+v %v", got, err)
	}
	all, _ := s.ManagedWorktrees("")
	if len(all) != 2 {
		t.Fatalf("all: %+v", all)
	}
	at := time.Now()
	if err := s.SetManagedWorktree(w.ID, WorktreeKept, WorktreeDirty, at); err != nil {
		t.Fatal(err)
	}
	if err := s.SetManagedWorktree(w.ID, WorktreeRemoved, "", at); err != nil {
		t.Fatal(err)
	}
	got, _ = s.ManagedWorktrees("r1")
	if got[0].State != WorktreeRemoved || got[0].RemovedAt == nil {
		t.Fatalf("after remove: %+v", got[0])
	}
	if err := s.SetManagedWorktree(999, WorktreeRemoved, "", at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestAgentsOfRunsAndMarkGC(t *testing.T) {
	s := open(t)
	for _, a := range []Agent{
		{ID: "a1", RunID: "r1", State: StateBusy},
		{ID: "a2", RunID: "r2", State: StateBusy},
		{ID: "a3", State: StateBusy},
		{ID: "a4", RunID: "r1", State: StateBusy},
	} {
		if err := s.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Done("a4", Handoff{Text: "hi", Worktrees: []Worktree{{Path: "/p"}}}, ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentsOfRuns([]string{"r1"})
	if err != nil || len(got) != 2 || got[0].ID != "a1" || got[1].ID != "a4" {
		t.Fatalf("r1 agents: %+v %v", got, err)
	}
	if got[1].Handoff == nil || got[1].Handoff.Text != "hi" || len(got[1].Handoff.Worktrees) != 1 {
		t.Fatalf("handoff: %+v", got[1].Handoff)
	}
	if none, _ := s.AgentsOfRuns(nil); len(none) != 0 {
		t.Fatalf("no runs: %+v", none)
	}
	if err := s.MarkGC("a1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.GCAt == nil {
		t.Fatal("gc_at not set")
	}
	if err := s.MarkGC("nope", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestRunsLimitAndActive(t *testing.T) {
	s := open(t)
	base := time.Now().Add(-time.Hour)
	// r0 oldest ... r5 newest; r0 busy, r1 finished unacked, r2 finished
	// acked, r3..r5 finished acked.
	for i := range 6 {
		id := string(rune('0' + i))
		if err := s.CreateRun(Run{ID: "r" + id, Goal: "g", CreatedAt: base.Add(time.Duration(i) * time.Minute), Steps: []Step{{Name: "s"}}}); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
		if err := s.FinishRun("r"+id, End{State: StateDone, Reason: ReasonDelivered}); err != nil {
			t.Fatal(err)
		}
		if i != 1 {
			if err := s.Ack("r"+id, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	ids := func(runs []Run, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, r := range runs {
			out += r.ID + " "
		}
		return out
	}
	if got := ids(s.Runs(RunFilter{Limit: 2})); got != "r5 r4 " {
		t.Fatalf("limit: %q", got)
	}
	if got := ids(s.Runs(RunFilter{Limit: 2, Active: true})); got != "r5 r4 r1 r0 " {
		t.Fatalf("active: %q", got)
	}
	if got := ids(s.Runs(RunFilter{Limit: 2, Active: true, Include: []string{"r2"}})); got != "r5 r4 r2 r1 r0 " {
		t.Fatalf("include: %q", got)
	}
	if got := ids(s.Runs(RunFilter{})); got != "r5 r4 r3 r2 r1 r0 " {
		t.Fatalf("all: %q", got)
	}
}

func TestLastEventID(t *testing.T) {
	s := open(t)
	if id, err := s.LastEventID(); err != nil || id != 0 {
		t.Fatalf("empty: %d %v", id, err)
	}
	s.Append(Event{Kind: "a"}, Event{Kind: "b"})
	if id, err := s.LastEventID(); err != nil || id != 2 {
		t.Fatalf("after two: %d %v", id, err)
	}
}
