package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "a b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWAL(t *testing.T) {
	s := open(t).(*sqliteStore)
	var mode string
	if err := s.db.Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q", mode)
	}
}

func TestAgentLifecycle(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", SessionID: "a1", TmuxSession: "aeolian-a1", Dir: "/d", Prompt: "p", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	state := func() string {
		t.Helper()
		a, err := s.Agent("a1")
		if err != nil {
			t.Fatal(err)
		}
		return a.State
	}
	now := time.Now()
	for _, step := range []struct{ event, want string }{
		{"SessionStart", StateBusy},
		{"Stop", StateIdle},
		{"Notification", StateIdle},
		{"UserPromptSubmit", StateBusy},
	} {
		if err := s.Hook("a1", step.event, now); err != nil {
			t.Fatal(err)
		}
		if got := state(); got != step.want {
			t.Fatalf("after %s: state %q, want %q", step.event, got, step.want)
		}
	}

	h := Handoff{Text: "did it", Worktrees: []Worktree{{Repo: "/r", Path: "/r/wt", Branch: "b"}, {Repo: "/q", Path: "/q/wt", Branch: "c"}}}
	if err := s.Done("a1", h, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Hook("a1", "Stop", now); err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateDone || a.DoneAt == nil || a.LastHook != "Stop" {
		t.Fatalf("agent after done: %+v", a)
	}
	if a.Handoff == nil || a.Handoff.Text != "did it" || len(a.Handoff.Worktrees) != 2 || a.Handoff.Worktrees[1] != h.Worktrees[1] {
		t.Fatalf("handoff: %+v", a.Handoff)
	}

	all, err := s.Agents()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Handoff == nil || len(all[0].Handoff.Worktrees) != 2 {
		t.Fatalf("agents: %+v", all)
	}
}

func TestUnknownAgent(t *testing.T) {
	s := open(t)
	if _, err := s.Agent("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Agent: %v", err)
	}
	if err := s.Hook("x", "Stop", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Hook: %v", err)
	}
	if err := s.Done("x", Handoff{}, ReasonDelivered, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Done: %v", err)
	}
}

func TestHelperKeepsParentAndRole(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "h1", ParentID: "a1", Role: "reviewer", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent("h1")
	if err != nil {
		t.Fatal(err)
	}
	if a.ParentID != "a1" || a.Role != "reviewer" {
		t.Fatalf("helper: %+v", a)
	}
}

func TestRunLifecycle(t *testing.T) {
	s := open(t)
	if err := s.CreateRun(Run{ID: "r1", Goal: "g", Workflow: "steps: []", Steps: []Step{{Name: "build"}, {Name: "check"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAgent(Agent{ID: "a1", RunID: "r1", Step: "build", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.StartStep("r1", 0, "a1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateDone, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateDone, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second FinishStep: %v", err)
	}
	r, err := s.Run("r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateBusy || r.Goal != "g" || r.Workflow != "steps: []" || len(r.Steps) != 2 {
		t.Fatalf("run: %+v", r)
	}
	if st := r.Steps[0]; st.Name != "build" || st.AgentID != "a1" || st.State != StateDone || st.StartedAt == nil || st.DoneAt == nil {
		t.Fatalf("step 0: %+v", st)
	}
	if st := r.Steps[1]; st.Index != 1 || st.Name != "check" || st.State != StatePending {
		t.Fatalf("step 1: %+v", st)
	}
	if err := s.FinishRun("r1", End{State: StateDone, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateFailed, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second FinishRun: %v", err)
	}
	if r, _ := s.Run("r1"); r.State != StateDone || r.DoneAt == nil {
		t.Fatalf("run after finish: %+v", r)
	}
	if a, _ := s.Agent("a1"); a.RunID != "r1" || a.Step != "build" {
		t.Fatalf("agent: %+v", a)
	}
}

func TestKilledRunStaysKilled(t *testing.T) {
	s := open(t)
	if err := s.CreateRun(Run{ID: "r1", Steps: []Step{{Name: "build"}, {Name: "check"}}}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.StartStep("r1", 0, "a1", now); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateKilled, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateKilled, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateFailed, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("FinishRun after kill: %v", err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateDone, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("FinishStep after kill: %v", err)
	}
	r, _ := s.Run("r1")
	if r.State != StateKilled || r.DoneAt == nil || r.Steps[0].State != StateKilled || r.Steps[1].State != StatePending {
		t.Fatalf("killed run: %+v", r)
	}
}

func TestQueuedRun(t *testing.T) {
	s := open(t)
	run := Run{ID: "r1", Source: "slack", Ref: "t1", State: StateQueued, Steps: []Step{{Name: "build"}}}
	if err := s.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run("r1"); r.State != StateQueued || r.Steps[0].State != StatePending {
		t.Fatalf("queued run: %+v", r)
	}
	var live *LiveRunError
	if err := s.CreateRun(Run{ID: "r2", Source: "slack", Ref: "t1"}); !errors.As(err, &live) || live.RunID != "r1" {
		t.Fatalf("second run on a queued ref: %v", err)
	}
	if err := s.StartQueuedRun("r1"); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run("r1"); r.State != StateBusy {
		t.Fatalf("started run: %+v", r)
	}
	if err := s.StartQueuedRun("r1"); !errors.Is(err, ErrFinished) {
		t.Fatalf("start a busy run: %v", err)
	}

	if err := s.CreateRun(Run{ID: "r3", State: StateQueued}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r3", End{State: StateKilled, At: time.Now()}); err != nil {
		t.Fatalf("kill a queued run: %v", err)
	}
	if err := s.StartQueuedRun("r3"); !errors.Is(err, ErrFinished) {
		t.Fatalf("start a killed run: %v", err)
	}
	if err := s.StartQueuedRun("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("start an unknown run: %v", err)
	}
	if err := s.CreateRun(Run{ID: "r4", State: StateDone}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run("r4"); r.State != StateBusy {
		t.Fatalf("run created done: %+v", r)
	}
}

func TestUnknownRun(t *testing.T) {
	s := open(t)
	if _, err := s.Run("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Run: %v", err)
	}
	if err := s.StartStep("x", 0, "a", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartStep: %v", err)
	}
	if err := s.FinishStep("x", 0, End{State: StateDone, At: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FinishStep: %v", err)
	}
	if err := s.FinishRun("x", End{State: StateDone, At: time.Now()}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FinishRun: %v", err)
	}
}

func TestKillAndReopen(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonKilled, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonKilled, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second Kill: %v", err)
	}
	if err := s.Hook("a1", "UserPromptSubmit", now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.State != StateKilled || a.DoneAt == nil {
		t.Fatalf("killed agent: %+v", a)
	}
	if err := s.Reopen("a1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.State != StateIdle || a.DoneAt != nil {
		t.Fatalf("reopened agent: %+v", a)
	}

	if err := s.Done("a1", Handoff{Text: "ok"}, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonKilled, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("Kill after done: %v", err)
	}
	if err := s.Reopen("a1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.State != StateIdle || a.Handoff == nil || a.Handoff.Text != "ok" {
		t.Fatalf("reopened done agent: %+v", a)
	}
	if err := s.EndAgent("x", End{State: StateKilled, Reason: ReasonKilled, At: now}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Kill unknown: %v", err)
	}
	if err := s.Reopen("x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reopen unknown: %v", err)
	}
}

func TestRunsNewestFirst(t *testing.T) {
	s := open(t)
	if runs, err := s.Runs(RunFilter{}); err != nil || len(runs) != 0 {
		t.Fatalf("no runs: %v %+v", err, runs)
	}
	now := time.Now()
	for i, id := range []string{"old", "new"} {
		r := Run{ID: id, Goal: id, CreatedAt: now.Add(time.Duration(i) * time.Second), Steps: []Step{{Name: "a"}, {Name: "b"}}}
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "new" || runs[1].ID != "old" {
		t.Fatalf("runs: %+v", runs)
	}
	if st := runs[1].Steps; len(st) != 2 || st[0].Name != "a" || st[1].Name != "b" {
		t.Fatalf("steps: %+v", st)
	}
}

func TestFailAndBlocked(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", State: StateBusy, Headless: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.EndAgent("a1", End{State: StateFailed, Reason: ReasonExited, At: now}); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{StateFailed, StateKilled} {
		if err := s.EndAgent("a1", End{State: state, At: now}); !errors.Is(err, ErrFinished) {
			t.Fatalf("after Fail: %v", err)
		}
	}
	if err := s.Hook("a1", "Stop", now); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.State != StateFailed || a.DoneAt == nil || !a.Headless || !Finished(a.State) {
		t.Fatalf("failed agent: %+v", a)
	}
	if err := s.EndAgent("x", End{State: StateFailed, Reason: ReasonExited, At: now}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Fail unknown: %v", err)
	}

	if err := s.CreateRun(Run{ID: "r1", Goal: "g", Steps: []Step{{Name: "a"}, {Name: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateBlocked, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateBlocked, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateFailed, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("finish blocked run: %v", err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateKilled, At: now}); !errors.Is(err, ErrFinished) {
		t.Fatalf("finish blocked step: %v", err)
	}
	if r, _ := s.Run("r1"); r.State != StateBlocked || r.Steps[0].State != StateBlocked || r.Steps[1].State != StatePending {
		t.Fatalf("blocked run: %+v", r)
	}
}

func TestInboxBySourceAndAck(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, r := range []Run{
		{ID: "plain", Goal: "g", CreatedAt: now},
		{ID: "r1", Goal: "g", Source: SourceLinear, Ref: "i-1", CreatedAt: now.Add(time.Second)},
		{ID: "r2", Goal: "g", CreatedAt: now.Add(2 * time.Second)},
		{ID: "r3", Goal: "g", Source: SourceLinear, Ref: "i-2", CreatedAt: now.Add(3 * time.Second)},
	} {
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(runs []Run, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(s.Inbox("")); got != nil {
		t.Fatalf("busy runs in the inbox: %v", got)
	}
	for _, id := range []string{"plain", "r1", "r3"} {
		if err := s.FinishRun(id, End{State: StateBlocked, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(s.Inbox(SourceLinear)); len(got) != 2 || got[0] != "r1" || got[1] != "r3" {
		t.Fatalf("linear inbox %v", got)
	}
	if err := s.Ack("r1", now); err != nil {
		t.Fatal(err)
	}
	if got := ids(s.Inbox(SourceLinear)); len(got) != 1 || got[0] != "r3" {
		t.Fatalf("linear inbox after ack %v", got)
	}
	if r1, err := s.Run("r1"); err != nil || r1.AckedAt == nil {
		t.Fatalf("r1 acked_at %v %v", r1.AckedAt, err)
	}
}

func TestInboxAndAck(t *testing.T) {
	s := open(t)
	now := time.Now()
	for i, r := range []Run{
		{ID: "a", Source: "slack"},
		{ID: "b"},
		{ID: "c", Source: "slack"},
		{ID: "live", Source: "slack"},
	} {
		r.Goal, r.CreatedAt = "g", now.Add(time.Duration(i)*time.Second)
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	for id, at := range map[string]time.Time{"a": now.Add(9 * time.Second), "b": now.Add(8 * time.Second), "c": now.Add(7 * time.Second)} {
		if err := s.FinishRun(id, End{State: StateDone, At: at}); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(source string) string {
		t.Helper()
		runs, err := s.Inbox(source)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(""); got != "c,b,a" {
		t.Fatalf("inbox %q, want oldest finished first", got)
	}
	if got := ids("slack"); got != "c,a" {
		t.Fatalf("slack inbox %q", got)
	}
	if err := s.Ack("live", now); !errors.Is(err, ErrNotFinished) {
		t.Fatalf("ack busy run: %v", err)
	}
	if err := s.Ack("nope", now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack unknown run: %v", err)
	}
	if err := s.Ack("c", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Ack("c", now.Add(time.Hour)); err != nil {
		t.Fatalf("second ack: %v", err)
	}
	c, err := s.Run("c")
	if err != nil || c.AckedAt == nil || !c.AckedAt.Equal(now) {
		t.Fatalf("c acked at %v, want the first ack %v (%v)", c.AckedAt, now, err)
	}
	if got := ids(""); got != "b,a" {
		t.Fatalf("inbox after ack %q", got)
	}
}

func TestOpenAcksOldHistoryOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	reported := now.Add(time.Minute)
	for _, r := range []Run{
		{ID: "plain-done"},
		{ID: "plain-busy"},
		{ID: "ticket-reported"},
		{ID: "ticket-unreported"},
	} {
		r.Goal = "g"
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	setTicket(t, s, "ticket-reported", "i-1", "ENG-1")
	setTicket(t, s, "ticket-unreported", "i-2", "ENG-2")
	for _, id := range []string{"plain-done", "ticket-reported", "ticket-unreported"} {
		if err := s.FinishRun(id, End{State: StateFailed, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	db := s.(*sqliteStore).db
	for _, q := range []string{
		"ALTER TABLE runs RENAME COLUMN acked_at TO reported_at",
		"UPDATE runs SET reported_at = NULL",
	} {
		if err := db.Exec(q).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("UPDATE runs SET reported_at = ? WHERE id = 'ticket-reported'", reported).Error; err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*time.Time{"plain-done": &now, "plain-busy": nil, "ticket-reported": &reported, "ticket-unreported": nil}
	for id, w := range want {
		r, err := s.Run(id)
		if err != nil {
			t.Fatal(err)
		}
		if (r.AckedAt == nil) != (w == nil) || (w != nil && !r.AckedAt.Equal(*w)) {
			t.Fatalf("%s acked at %v, want %v", id, r.AckedAt, w)
		}
	}
	if err := s.FinishRun("plain-busy", End{State: StateDone, At: now}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inbox, err := s.Inbox("")
	if err != nil {
		t.Fatal(err)
	}
	if len(inbox) != 2 || inbox[0].ID != "plain-busy" || inbox[1].ID != "ticket-unreported" {
		t.Fatalf("inbox after a later Open %+v, want plain-busy and ticket-unreported", inbox)
	}
}

func TestRunSourceAndRef(t *testing.T) {
	s := open(t)
	now := time.Now()
	create := func(r Run) error {
		t.Helper()
		r.Goal, r.CreatedAt, now = "g", now, now.Add(time.Second)
		return s.CreateRun(r)
	}
	if err := create(Run{ID: "t1", Source: "slack-docs", Ref: "t1"}); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Run("t1"); r.Source != "slack-docs" || r.Ref != "t1" {
		t.Fatalf("source and ref not stored: %+v", r)
	}
	err := create(Run{ID: "again", Source: "slack-docs", Ref: "t1"})
	var live *LiveRunError
	if !errors.As(err, &live) || live.RunID != "t1" {
		t.Fatalf("duplicate of a busy run: %v", err)
	}
	if _, err := s.Run("again"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("refused run was stored: %v", err)
	}
	for _, r := range []Run{
		{ID: "other-source", Source: "slack-ops", Ref: "t1"},
		{ID: "other-ref", Source: "slack-docs", Ref: "t2"},
		{ID: "no-ref-1", Source: "slack-docs"},
		{ID: "no-ref-2", Source: "slack-docs"},
		{ID: "plain-1"},
		{ID: "plain-2"},
	} {
		if err := create(r); err != nil {
			t.Fatalf("%s: %v", r.ID, err)
		}
	}
	if err := s.FinishRun("t1", End{State: StateDone, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := create(Run{ID: "after", Source: "slack-docs", Ref: "t1"}); err != nil {
		t.Fatalf("rerun after the first finished: %v", err)
	}

	ids := func(f RunFilter) []string {
		t.Helper()
		runs, err := s.Runs(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range runs {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(RunFilter{Source: "slack-docs", Ref: "t1"}); len(got) != 2 || got[0] != "after" || got[1] != "t1" {
		t.Fatalf("source and ref filter: %v", got)
	}
	if got := ids(RunFilter{Ref: "t1"}); len(got) != 3 {
		t.Fatalf("ref filter: %v", got)
	}
	if got := ids(RunFilter{Source: "slack-docs"}); len(got) != 5 {
		t.Fatalf("source filter: %v", got)
	}
	if got := ids(RunFilter{}); len(got) != 8 {
		t.Fatalf("no filter: %v", got)
	}
}

func TestOpenBackfillsTicketSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Run{
		{ID: "old-ticket"},
		{ID: "tagged-ticket", Source: "elsewhere", Ref: "x"},
		{ID: "plain"},
	} {
		if err := s.CreateRun(r); err != nil {
			t.Fatal(err)
		}
	}
	setTicket(t, s, "old-ticket", "i-1", "ENG-1")
	setTicket(t, s, "tagged-ticket", "i-2", "ENG-2")
	db := s.(*sqliteStore).db
	if err := db.Exec("UPDATE runs SET source = NULL, ref = NULL WHERE id = 'old-ticket'").Error; err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string][2]string{"old-ticket": {SourceLinear, "i-1"}, "tagged-ticket": {"elsewhere", "x"}, "plain": {"", ""}}
	for id, w := range want {
		r, err := s.Run(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Source != w[0] || r.Ref != w[1] {
			t.Fatalf("%s: source %q ref %q, want %q %q", id, r.Source, r.Ref, w[0], w[1])
		}
	}
}

func setTicket(t *testing.T, s Store, runID, ticketID, identifier string) {
	t.Helper()
	err := s.(*sqliteStore).db.Exec("UPDATE runs SET ticket_id = ?, ticket_identifier = ? WHERE id = ?", ticketID, identifier, runID).Error
	if err != nil {
		t.Fatal(err)
	}
}

func TestCollectHandoffs(t *testing.T) {
	s := open(t)
	defer s.Close()
	now := time.Now()
	for _, a := range []Agent{
		{ID: "lead", State: StateBusy},
		{ID: "h1", ParentID: "lead", Role: "r", State: StateBusy, CreatedAt: now.Add(-3 * time.Second)},
		{ID: "h2", ParentID: "lead", Role: "r", State: StateBusy, CreatedAt: now.Add(-2 * time.Second)},
		{ID: "h3", ParentID: "lead", Role: "r", State: StateBusy, CreatedAt: now.Add(-time.Second)},
		{ID: "other", ParentID: "x", Role: "r", State: StateBusy},
	} {
		if err := s.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	for i, id := range []string{"h2", "h1", "other"} {
		if err := s.Done(id, Handoff{Text: id + " out"}, ReasonDelivered, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() string {
		got, err := s.Uncollected("lead")
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range got {
			if a.Handoff == nil || a.Handoff.Text != a.ID+" out" {
				t.Fatalf("%s handoff %+v", a.ID, a.Handoff)
			}
			out = append(out, a.ID)
		}
		return strings.Join(out, ",")
	}
	// Live h3 is not owed; h2 handed back first.
	if got := ids(); got != "h2,h1" {
		t.Fatalf("uncollected %q", got)
	}
	if err := s.CollectHandoffs([]string{"h2", "h3"}, CollectedAwait, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CollectHandoffs([]string{"h2"}, CollectedTyped, now); err != nil {
		t.Fatal(err)
	}
	if got := ids(); got != "h1" {
		t.Fatalf("after collect %q", got)
	}
	evs, err := s.Events(EventQuery{Kinds: []string{KindHandoffCollected}})
	if err != nil || len(evs) != 1 || evs[0].AgentID != "h2" {
		t.Fatalf("events %v %+v", err, evs)
	}
	// A resumed helper's new handoff is owed again.
	if err := s.Reopen("h2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Done("h2", Handoff{Text: "h2 out"}, ReasonDelivered, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := ids(); got != "h1,h2" {
		t.Fatalf("after reopen %q", got)
	}
}

func TestOpenMarksOldHandoffsCollected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []Agent{{ID: "lead", State: StateBusy}, {ID: "h", ParentID: "lead", State: StateBusy}} {
		if err := s.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Done("h", Handoff{Text: "old"}, ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.(*sqliteStore).db.Exec("ALTER TABLE handoffs DROP COLUMN collected_at").Error; err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.Uncollected("lead"); err != nil || len(got) != 0 {
		t.Fatalf("old handoffs owed: %v %+v", err, got)
	}
}
