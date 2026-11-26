package store

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestDoneAfterEndIsRefused(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, end := range []End{
		{State: StateKilled, Reason: ReasonKilled},
		{State: StateFailed, Reason: ReasonExited},
	} {
		id := "a-" + end.State
		if err := s.CreateAgent(Agent{ID: id, State: StateBusy}); err != nil {
			t.Fatal(err)
		}
		if err := s.EndAgent(id, end); err != nil {
			t.Fatal(err)
		}
		if err := s.Done(id, Handoff{Text: "late"}, ReasonDelivered, now); !errors.Is(err, ErrFinished) {
			t.Fatalf("done after %s: %v", end.State, err)
		}
		a, err := s.Agent(id)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != end.State || a.Reason != end.Reason || a.Handoff != nil {
			t.Fatalf("late done overwrote the end: %+v", a)
		}
	}
}

func TestDoneTwiceIsRefused(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.Done("a1", Handoff{Text: "first"}, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	if err := s.Done("a1", Handoff{Text: "second"}, ReasonBlocked, now); !errors.Is(err, ErrFinished) {
		t.Fatalf("second done: %v", err)
	}
	if a, _ := s.Agent("a1"); a.Handoff.Text != "first" || a.Reason != ReasonDelivered {
		t.Fatalf("second done changed the agent: %+v", a)
	}
}

func TestReopenThenDone(t *testing.T) {
	s := open(t)
	if err := s.CreateAgent(Agent{ID: "a1", State: StateBusy}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonLost, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen("a1"); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.Agent("a1"); a.State != StateIdle || a.Reason != "" {
		t.Fatalf("reopened agent keeps its end: %+v", a)
	}
	h := Handoff{Text: "back", Synthesized: true, Worktrees: []Worktree{{Repo: "/r", Path: "/r/wt", Branch: "b"}}}
	if err := s.Done("a1", h, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Agent("a1")
	if a.State != StateDone || a.Reason != ReasonDelivered || a.Handoff == nil || !a.Handoff.Synthesized || len(a.Handoff.Worktrees) != 1 {
		t.Fatalf("done after reopen: %+v %+v", a, a.Handoff)
	}
}

func TestDoneUnknownAgent(t *testing.T) {
	s := open(t)
	if err := s.Done("x", Handoff{}, ReasonDelivered, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("done unknown: %v", err)
	}
	if err := s.EndAgent("x", End{State: StateKilled}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("end unknown: %v", err)
	}
}

func TestReasonsOnStepAndRun(t *testing.T) {
	s := open(t)
	if err := s.CreateRun(Run{ID: "r1", Goal: "g", Steps: []Step{{Name: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishStep("r1", 0, End{State: StateFailed, Reason: ReasonStartFailed}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateFailed, Reason: ReasonStartFailed}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateDone, Reason: ReasonEnded}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second finish: %v", err)
	}
	r, err := s.Run("r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != StateFailed || r.Reason != ReasonStartFailed || r.DoneAt == nil {
		t.Fatalf("run: %+v", r)
	}
	if st := r.Steps[0]; st.State != StateFailed || st.Reason != ReasonStartFailed || st.DoneAt == nil {
		t.Fatalf("step: %+v", st)
	}
}

func TestLiveAgents(t *testing.T) {
	s := open(t)
	now := time.Now()
	for i, a := range []Agent{
		{ID: "busy", State: StateBusy},
		{ID: "idle", State: StateIdle},
		{ID: "done", State: StateBusy},
		{ID: "killed", State: StateBusy},
		{ID: "failed", State: StateBusy},
	} {
		a.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Done("done", Handoff{Text: "x"}, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAgent("killed", End{State: StateKilled, Reason: ReasonKilled}); err != nil {
		t.Fatal(err)
	}
	if err := s.EndAgent("failed", End{State: StateFailed, Reason: ReasonExited}); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 || live[0].ID != "busy" || live[1].ID != "idle" {
		t.Fatalf("live: %+v", live)
	}
	if live[0].Handoff != nil {
		t.Fatalf("live agents carry handoffs: %+v", live[0])
	}
}

// oldSchema is the production schema from before reasons and events, as
// SQLite printed it.
var oldSchema = []string{
	"CREATE TABLE `agents` (`id` text,`session_id` text,`tmux_session` text,`dir` text,`prompt` text,`parent_id` text,`role` text,`run_id` text,`step` text,`headless` numeric,`state` text,`last_hook` text,`last_hook_at` datetime,`created_at` datetime,`done_at` datetime,PRIMARY KEY (`id`))",
	"CREATE INDEX `idx_agents_run_id` ON `agents`(`run_id`)",
	"CREATE TABLE `handoffs` (`agent_id` text,`text` text,`created_at` datetime,PRIMARY KEY (`agent_id`))",
	"CREATE TABLE `worktrees` (`id` integer PRIMARY KEY AUTOINCREMENT,`agent_id` text,`repo` text,`path` text,`branch` text,CONSTRAINT `fk_handoffs_worktrees` FOREIGN KEY (`agent_id`) REFERENCES `handoffs`(`agent_id`))",
	"CREATE INDEX `idx_worktrees_agent_id` ON `worktrees`(`agent_id`)",
	"CREATE TABLE `runs` (`id` text,`goal` text,`workflow` text,`state` text,`created_at` datetime,`done_at` datetime,`ticket_id` text,`ticket_identifier` text,\"acked_at\" datetime, `source` text, `ref` text,PRIMARY KEY (`id`))",
	"CREATE INDEX `idx_runs_ticket_id` ON `runs`(`ticket_id`)",
	"CREATE TABLE `steps` (`run_id` text,`idx` integer,`name` text,`agent_id` text,`state` text,`started_at` datetime,`done_at` datetime,PRIMARY KEY (`run_id`,`idx`),CONSTRAINT `fk_runs_steps` FOREIGN KEY (`run_id`) REFERENCES `runs`(`id`))",
	"CREATE INDEX `idx_runs_source_ref` ON `runs`(`source`,`ref`)",
}

func TestOpenBackfillsReasons(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	stmts := append([]string{}, oldSchema...)
	stmts = append(stmts,
		// delivered: one-step run, done
		`INSERT INTO runs VALUES ('r-done','g','','done','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-done',0,'a','a-done','done','2026-01-01','2026-01-01')`,
		`INSERT INTO agents VALUES ('a-done','a-done','t','/d','p','','','r-done','a',0,'done','Stop','2026-01-01','2026-01-01','2026-01-01')`,
		`INSERT INTO handoffs VALUES ('a-done','ok','2026-01-01')`,
		// blocked
		`INSERT INTO runs VALUES ('r-blocked','g','','blocked','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-blocked',0,'a','a-blocked','blocked','2026-01-01','2026-01-01')`,
		`INSERT INTO agents VALUES ('a-blocked','a-blocked','t','/d','p','','','r-blocked','a',1,'done','Stop','2026-01-01','2026-01-01','2026-01-01')`,
		`INSERT INTO handoffs VALUES ('a-blocked','question?','2026-01-01')`,
		// lost: interactive step agent swept killed, step and run failed
		`INSERT INTO runs VALUES ('r-lost','g','','failed','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-lost',0,'a','a-lost','failed','2026-01-01','2026-01-01')`,
		`INSERT INTO agents VALUES ('a-lost','a-lost','t','/d','p','','','r-lost','a',0,'killed','Stop','2026-01-01','2026-01-01','2026-01-01')`,
		// exited: headless step agent failed
		`INSERT INTO runs VALUES ('r-exited','g','','failed','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-exited',0,'a','a-exited','failed','2026-01-01','2026-01-01')`,
		`INSERT INTO agents VALUES ('a-exited','a-exited','t','/d','p','','','r-exited','a',1,'failed','','2026-01-01','2026-01-01','2026-01-01')`,
		// start_failed: the step failed before any agent was stored
		`INSERT INTO runs VALUES ('r-nostart','g','','failed','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-nostart',0,'a',NULL,'failed',NULL,'2026-01-01')`,
		// start_failed: launch failed after the agent was stored
		`INSERT INTO runs VALUES ('r-launch','g','','failed','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-launch',0,'a',NULL,'failed',NULL,'2026-01-01')`,
		`INSERT INTO agents VALUES ('a-launch','a-launch','t','/d','p','','','r-launch','a',0,'done','','2026-01-01','2026-01-01','2026-01-01')`,
		`INSERT INTO handoffs VALUES ('a-launch','start agent a-launch: boom','2026-01-01')`,
		// killed run: step killed, agent and its helper killed
		`INSERT INTO runs VALUES ('r-killed','g','','killed','2026-01-01','2026-01-01','','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-killed',0,'a','a-killed','killed','2026-01-01','2026-01-01')`,
		`INSERT INTO steps VALUES ('r-killed',1,'b',NULL,'pending',NULL,NULL)`,
		`INSERT INTO agents VALUES ('a-killed','a-killed','t','/d','p','','','r-killed','a',0,'killed','','2026-01-01','2026-01-01','2026-01-01')`,
		`INSERT INTO agents VALUES ('h-killed','h-killed','t','/d','p','a-killed','coder','r-killed','a',0,'killed','','2026-01-01','2026-01-01','2026-01-01')`,
		// live run and agent keep no reason
		`INSERT INTO runs VALUES ('r-busy','g','','busy','2026-01-01',NULL,'','',NULL,'','')`,
		`INSERT INTO steps VALUES ('r-busy',0,'a','a-busy','busy','2026-01-01',NULL)`,
		`INSERT INTO agents VALUES ('a-busy','a-busy','t','/d','p','','','r-busy','a',0,'idle','Stop','2026-01-01','2026-01-01',NULL)`,
	)
	for _, q := range stmts {
		if err := db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	sqlDB, _ := db.DB()
	sqlDB.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs := map[string][2]string{ // run -> reason, step 0 reason
		"r-done":    {ReasonDelivered, ReasonDelivered},
		"r-blocked": {ReasonBlocked, ReasonBlocked},
		"r-lost":    {ReasonLost, ReasonLost},
		"r-exited":  {ReasonExited, ReasonExited},
		"r-nostart": {ReasonStartFailed, ReasonStartFailed},
		"r-launch":  {ReasonStartFailed, ReasonStartFailed},
		"r-killed":  {ReasonKilled, ReasonKilled},
		"r-busy":    {"", ""},
	}
	for id, want := range runs {
		r, err := s.Run(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Reason != want[0] || r.Steps[0].Reason != want[1] {
			t.Errorf("run %s: reason %q step %q, want %q %q", id, r.Reason, r.Steps[0].Reason, want[0], want[1])
		}
	}
	if r, _ := s.Run("r-killed"); r.Steps[1].Reason != "" {
		t.Errorf("pending step got reason %q", r.Steps[1].Reason)
	}
	agents := map[string]string{
		"a-done":    ReasonDelivered,
		"a-blocked": ReasonBlocked,
		"a-lost":    ReasonLost,
		"a-exited":  ReasonExited,
		"a-launch":  ReasonStartFailed,
		"a-killed":  ReasonKilled,
		"h-killed":  ReasonKilled,
		"a-busy":    "",
	}
	for id, want := range agents {
		a, err := s.Agent(id)
		if err != nil {
			t.Fatal(err)
		}
		if a.Reason != want {
			t.Errorf("agent %s: reason %q, want %q", id, a.Reason, want)
		}
	}
	if a, _ := s.Agent("a-done"); a.Handoff == nil || a.Handoff.Text != "ok" || a.Handoff.Synthesized {
		t.Errorf("handoff after migration: %+v", a.Handoff)
	}
	live, err := s.LiveAgents()
	if err != nil || len(live) != 1 || live[0].ID != "a-busy" {
		t.Fatalf("live after migration: %v %+v", err, live)
	}
	if err := s.Append(Event{Kind: "test"}); err != nil {
		t.Fatalf("append after migration: %v", err)
	}
	if diff, err := FoldDiff(s); err != nil || len(diff) > 0 {
		t.Fatalf("fold after backfill: %v %q", err, diff)
	}
	before, err := s.Events(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	// The backfill runs once: a reason cleared afterwards stays cleared.
	db, err = gorm.Open(sqlite.Open(path), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`UPDATE runs SET reason = '' WHERE id = 'r-done'`).Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, _ = db.DB()
	sqlDB.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if r, _ := s2.Run("r-done"); r.Reason != "" {
		t.Fatalf("backfill ran again: %q", r.Reason)
	}
	// The event backfill does not run again either.
	if evs, err := s2.Events(EventQuery{}); err != nil || len(evs) != len(before) {
		t.Fatalf("events after reopen: %v %d, want %d", err, len(evs), len(before))
	}
}

// TestOpenRealCopy migrates a copy of a real database named by
// AEOLIAN_TEST_DB. It never opens that file itself: it copies it, with its
// -wal and -shm, into a temp dir first.
func TestOpenRealCopy(t *testing.T) {
	src := os.Getenv("AEOLIAN_TEST_DB")
	if src == "" {
		t.Skip("AEOLIAN_TEST_DB not set")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "aeolian.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := copyFile(src+suffix, dst+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			t.Fatal(err)
		}
	}
	s, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs, err := s.Runs(RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, r := range runs {
		counts[r.State+"/"+r.Reason]++
		if Finished(r.State) || r.State == StateBlocked {
			if r.Reason == "" {
				t.Errorf("finished run %s has no reason", r.ID)
			}
		} else if r.Reason != "" {
			t.Errorf("live run %s has reason %q", r.ID, r.Reason)
		}
	}
	t.Logf("runs by state/reason: %v", counts)
	agents, err := s.Agents()
	if err != nil {
		t.Fatal(err)
	}
	counts = map[string]int{}
	for _, a := range agents {
		counts[a.State+"/"+a.Reason]++
		if Finished(a.State) == (a.Reason == "") {
			t.Errorf("agent %s is %s with reason %q", a.ID, a.State, a.Reason)
		}
	}
	t.Logf("agents by state/reason: %v", counts)
	live, err := s.LiveAgents()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live agents: %d", len(live))
	diff, err := FoldDiff(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diff {
		t.Error(d)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func TestEndAgentHandoff(t *testing.T) {
	s := open(t)
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		if err := s.CreateAgent(Agent{ID: id, State: StateIdle, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	h := Handoff{Text: "last words", Synthesized: true, Worktrees: []Worktree{{Repo: "/r", Path: "/r/w", Branch: "b"}}}
	if err := s.EndAgentHandoff("a", End{State: StateKilled, Reason: ReasonClosed, At: now}, h); err != nil {
		t.Fatal(err)
	}
	a, err := s.Agent("a")
	if err != nil {
		t.Fatal(err)
	}
	if a.State != StateKilled || a.Reason != ReasonClosed || a.Handoff == nil || !a.Handoff.Synthesized || a.Handoff.Text != "last words" || len(a.Handoff.Worktrees) != 1 {
		t.Fatalf("agent: %+v %+v", a, a.Handoff)
	}
	if err := s.EndAgentHandoff("a", End{State: StateKilled, Reason: ReasonLost}, Handoff{Text: "again"}); !errors.Is(err, ErrFinished) {
		t.Fatalf("second end: %v", err)
	}
	if a, _ := s.Agent("a"); a.Handoff.Text != "last words" || a.Reason != ReasonClosed {
		t.Fatalf("second end overwrote: %+v", a)
	}
	if err := s.EndAgentHandoff("nobody", End{State: StateKilled}, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown agent: %v", err)
	}
}

func TestBusyAgentIDs(t *testing.T) {
	s := open(t)
	now := time.Now()
	for i, a := range []Agent{
		{ID: "b1", State: StateBusy},
		{ID: "idle", State: StateIdle},
		{ID: "b2", State: StateBusy},
		{ID: "done", State: StateBusy},
	} {
		a.CreatedAt = now.Add(time.Duration(i) * time.Second)
		if err := s.CreateAgent(a); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Done("done", Handoff{Text: "x"}, ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	ids, err := s.BusyAgentIDs()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"b1", "b2"}) {
		t.Fatalf("busy: %v", ids)
	}
}
