package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
)

// openFolded is open with a check, once the test is over, that the event
// log folds back into the tables exactly.
func openFolded(t *testing.T) Store {
	t.Helper()
	s := open(t)
	t.Cleanup(func() { assertFolds(t, s) })
	return s
}

func assertFolds(t *testing.T, s Store) {
	t.Helper()
	diff, err := FoldDiff(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diff {
		t.Errorf("fold: %s", d)
	}
}

func lastKinds(t *testing.T, s Store, after int64) ([]string, int64) {
	t.Helper()
	evs, err := s.Events(EventQuery{After: after})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
		after = e.ID
	}
	return kinds, after
}

// mutations are one call of every Store write, in an order that is valid
// on a fresh store, with the kinds each must append.
var mutations = []struct {
	name  string
	call  func(s Store) error
	kinds []string
}{
	{"CreateRun", func(s Store) error {
		return s.CreateRun(Run{ID: "r1", Goal: "g", Workflow: "steps: []", Source: "test", Ref: "x", Steps: []Step{{Name: "a"}, {Name: "b"}}})
	}, []string{KindRunCreated}},
	{"CreateRun queued", func(s Store) error {
		return s.CreateRun(Run{ID: "r2", Goal: "g2", State: StateQueued, Steps: []Step{{Name: "a"}}})
	}, []string{KindRunCreated}},
	{"StartQueuedRun", func(s Store) error { return s.StartQueuedRun("r2") }, []string{KindRunStarted}},
	{"CreateAgent", func(s Store) error {
		return s.CreateAgent(Agent{ID: "a1", SessionID: "a1", TmuxSession: "t1", Dir: "/d", Prompt: "p", RunID: "r1", Step: "a", State: StateBusy})
	}, []string{KindAgentCreated}},
	{"CreateAgent helper", func(s Store) error {
		return s.CreateAgent(Agent{ID: "h1", SessionID: "h1", TmuxSession: "t2", Dir: "/w", Prompt: "q", ParentID: "a1", Role: "coder", RunID: "r1", Step: "a", Headless: true, State: StateBusy})
	}, []string{KindAgentCreated}},
	{"StartStep", func(s Store) error { return s.StartStep("r1", 0, "a1", time.Now()) }, []string{KindStepStarted}},
	{"Hook SessionStart", func(s Store) error { return s.Hook("a1", "SessionStart", time.Now()) }, []string{KindHook}},
	{"Hook Stop", func(s Store) error { return s.Hook("a1", "Stop", time.Now()) }, []string{KindAgentState, KindHook}},
	{"Hook UserPromptSubmit with a human message", func(s Store) error {
		return s.Hook("a1", "UserPromptSubmit", time.Now(),
			event(time.Time{}, "", "", KindHook, HookData{V: 1, Event: "UserPromptSubmit", By: ByHuman}),
			event(time.Time{}, "", "", KindHumanMessage, HumanMessage{V: 1, Text: "hi"}))
	}, []string{KindAgentState, KindHook, KindHumanMessage}},
	{"Hook PostToolUse", func(s Store) error {
		return s.Hook("a1", "PostToolUse", time.Now(), event(time.Time{}, "", "", KindToolUsed, ToolUsed{V: 1, Tool: "Bash", OK: true}))
	}, []string{KindToolUsed}},
	{"Done helper", func(s Store) error {
		return s.Done("h1", Handoff{Text: "did it", Worktrees: []Worktree{{Repo: "/r", Path: "/w", Branch: "b"}}}, ReasonDelivered, time.Now())
	}, []string{KindAgentDone}},
	{"Reopen", func(s Store) error { return s.Reopen("h1") }, []string{KindAgentState}},
	{"EndAgentHandoff", func(s Store) error {
		return s.EndAgentHandoff("h1", End{State: StateKilled, Reason: ReasonLost}, Handoff{Text: "stalled", Synthesized: true})
	}, []string{KindAgentDone}},
	{"Reopen again", func(s Store) error { return s.Reopen("h1") }, []string{KindAgentState}},
	{"EndAgent", func(s Store) error { return s.EndAgent("h1", End{State: StateKilled, Reason: ReasonKilled}) }, []string{KindAgentEnded}},
	{"Done", func(s Store) error { return s.Done("a1", Handoff{Text: "ok"}, ReasonDelivered, time.Now()) }, []string{KindAgentDone}},
	{"FinishStep", func(s Store) error { return s.FinishStep("r1", 0, End{State: StateDone, Reason: ReasonDelivered}) }, []string{KindStepFinished}},
	{"FinishRun", func(s Store) error { return s.FinishRun("r1", End{State: StateDone, Reason: ReasonDelivered}) }, []string{KindRunFinished}},
	{"Ack", func(s Store) error { return s.Ack("r1", time.Now()) }, []string{KindRunAcked}},
	{"Ack again", func(s Store) error { return s.Ack("r1", time.Now()) }, nil},
	{"MarkGC", func(s Store) error { return s.MarkGC("h1", time.Now()) }, []string{KindAgentGC}},
	{"CreateManagedWorktree", func(s Store) error {
		_, err := s.CreateManagedWorktree(ManagedWorktree{RunID: "r1", AgentID: "a1", Repo: "/r", Path: "/r-wt", Branch: "b", Base: "main"})
		return err
	}, []string{KindWorktreeCreated}},
	{"SetManagedWorktree kept", func(s Store) error {
		return s.SetManagedWorktree(1, WorktreeKept, WorktreeDirty, time.Now())
	}, []string{KindWorktreeKept}},
	{"SetManagedWorktree removed", func(s Store) error {
		return s.SetManagedWorktree(1, WorktreeRemoved, "", time.Now())
	}, []string{KindWorktreeRemoved}},
}

func TestEveryMutationEmits(t *testing.T) {
	s := openFolded(t)
	var after int64
	for _, m := range mutations {
		if err := m.call(s); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
		var kinds []string
		kinds, after = lastKinds(t, s, after)
		if !slices.Equal(kinds, m.kinds) {
			t.Errorf("%s appended %v, want %v", m.name, kinds, m.kinds)
		}
		assertFolds(t, s)
	}
	evs, _ := s.Events(EventQuery{AgentID: "a1", Kinds: []string{KindHumanMessage}})
	if len(evs) != 1 || evs[0].RunID != "r1" {
		t.Fatalf("a hook's own event gets the agent's run: %+v", evs)
	}
}

// TestFailedMutationEmitsNothing refuses every events insert; each
// mutation must then fail and leave its rows as they were.
func TestFailedMutationEmitsNothing(t *testing.T) {
	s := openFolded(t)
	ss := s.(*sqliteStore)
	boom := errors.New("events insert refused")
	failing := false
	err := ss.db.Callback().Create().Before("gorm:create").Register("test:fail_events", func(db *gorm.DB) {
		if failing && db.Statement.Table == "events" {
			db.AddError(boom)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() Snapshot {
		agents, err := s.Agents()
		if err != nil {
			t.Fatal(err)
		}
		runs, err := s.Runs(RunFilter{})
		if err != nil {
			t.Fatal(err)
		}
		return Snapshot{Runs: runs, Agents: agents}
	}
	for _, m := range mutations {
		if m.kinds == nil {
			continue
		}
		before := snapshot()
		failing = true
		err := m.call(s)
		failing = false
		if !errors.Is(err, boom) {
			t.Fatalf("%s with the events insert refused: %v", m.name, err)
		}
		if after := snapshot(); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s changed rows without its events:\n%+v\n%+v", m.name, before, after)
		}
		if err := m.call(s); err != nil {
			t.Fatalf("%s: %v", m.name, err)
		}
	}
}

func TestFinishedTwiceEmitsOnce(t *testing.T) {
	s := openFolded(t)
	for _, m := range mutations[:6] {
		if err := m.call(s); err != nil {
			t.Fatal(err)
		}
	}
	_, after := lastKinds(t, s, 0)
	if err := s.EndAgent("a1", End{State: StateKilled, Reason: ReasonKilled}); err != nil {
		t.Fatal(err)
	}
	if err := s.Done("a1", Handoff{Text: "late"}, ReasonDelivered, time.Now()); !errors.Is(err, ErrFinished) {
		t.Fatalf("done after kill: %v", err)
	}
	if err := s.FinishRun("r1", End{State: StateKilled, Reason: ReasonKilled}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun("r1", End{State: StateDone, Reason: ReasonDelivered}); !errors.Is(err, ErrFinished) {
		t.Fatalf("finish twice: %v", err)
	}
	if err := s.Hook("nobody", "Stop", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("hook of an unknown agent: %v", err)
	}
	if err := s.StartQueuedRun("r1"); !errors.Is(err, ErrFinished) {
		t.Fatalf("start a finished run: %v", err)
	}
	if kinds, _ := lastKinds(t, s, after); !slices.Equal(kinds, []string{KindAgentEnded, KindRunFinished}) {
		t.Fatalf("kinds %v", kinds)
	}
}

// TestFoldAtID folds a prefix of the log: the world as it was then.
func TestFoldAtID(t *testing.T) {
	s := openFolded(t)
	var mid int64
	for _, m := range mutations {
		if err := m.call(s); err != nil {
			t.Fatal(err)
		}
		if m.name == "Done helper" {
			_, mid = lastKinds(t, s, 0)
		}
	}
	evs, err := s.Events(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var prefix []Event
	for _, e := range evs {
		if e.ID <= mid {
			prefix = append(prefix, e)
		}
	}
	snap := Fold(prefix)
	if len(snap.Runs) != 2 || snap.Runs[0].ID != "r1" || snap.Runs[0].State != StateBusy || snap.Runs[0].Steps[0].State != StateBusy {
		t.Fatalf("runs at %d: %+v", mid, snap.Runs)
	}
	if len(snap.Agents) != 2 || snap.Agents[1].State != StateDone || snap.Agents[1].Handoff == nil || snap.Agents[1].Handoff.Worktrees[0].Branch != "b" {
		t.Fatalf("agents at %d: %+v", mid, snap.Agents)
	}
	if snap.Agents[0].State != StateBusy || snap.Agents[0].LastHook != "PostToolUse" {
		t.Fatalf("lead at %d: %+v", mid, snap.Agents[0])
	}
}

func TestFoldDiffNamesDifferences(t *testing.T) {
	s := openFolded(t)
	for _, m := range mutations[:4] {
		if err := m.call(s); err != nil {
			t.Fatal(err)
		}
	}
	ss := s.(*sqliteStore)
	if err := ss.db.Exec("UPDATE agents SET state = 'idle' WHERE id = 'a1'").Error; err != nil {
		t.Fatal(err)
	}
	diff, err := FoldDiff(s)
	if err != nil || len(diff) != 1 || !strings.Contains(diff[0], "agent a1: State") {
		t.Fatalf("diff %v %q", err, diff)
	}
	ss.db.Exec("UPDATE agents SET state = 'busy' WHERE id = 'a1'")
}

func TestTruncate(t *testing.T) {
	big := strings.Repeat("é", 1<<20) // 2 MB
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "Stop", "n": 12345678901234567, "last_assistant_message": big, "list": []string{"a", big}})
	got := Truncate(payload, HookPayloadLimit)
	if len(got) > HookPayloadLimit || !json.Valid(got) {
		t.Fatalf("%d bytes, valid %v", len(got), json.Valid(got))
	}
	var m map[string]any
	dec := json.NewDecoder(strings.NewReader(string(got)))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m["hook_event_name"] != "Stop" || m["n"].(json.Number).String() != "12345678901234567" {
		t.Fatalf("small fields changed: %v", m)
	}
	msg := m["last_assistant_message"].(string)
	if !strings.HasPrefix(msg, "éé") || !strings.HasSuffix(msg, "…[2097152 bytes]") {
		t.Fatalf("cut field: %q", msg[len(msg)-40:])
	}
	if small := `{"a":"b"}`; string(Truncate([]byte(small), 100)) != small {
		t.Fatal("a small payload changed")
	}
	if got := Truncate([]byte(strings.Repeat("x", 100)), 64); !json.Valid(got) || len(got) > 64 {
		t.Fatalf("not json: %s", got)
	}
	if got := Truncate(nil, 64); got != nil {
		t.Fatalf("empty: %s", got)
	}
	many, _ := json.Marshal(slices.Repeat([]string{"ab"}, 10000))
	if got := string(Truncate(many, 1024)); got != `{"bytes":`+itoa(len(many))+`,"truncated":true}` {
		t.Fatalf("many: %s", got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestCompactEvents(t *testing.T) {
	s := openFolded(t)
	for _, m := range mutations {
		if err := m.call(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CompactEvents([]string{"r1"}, []string{KindAgentDone}); err == nil {
		t.Fatal("compacted a projection kind")
	}
	before, _ := s.Events(EventQuery{})
	n, err := s.CompactEvents([]string{"r1"}, BulkyKinds)
	if err != nil || n != 5 {
		t.Fatalf("compacted %d: %v", n, err)
	}
	if n, err := s.CompactEvents([]string{"r1"}, BulkyKinds); err != nil || n != 0 {
		t.Fatalf("compacted twice: %d %v", n, err)
	}
	after, _ := s.Events(EventQuery{})
	if len(after) != len(before) {
		t.Fatalf("%d events, had %d", len(after), len(before))
	}
	for i, e := range after {
		if e.ID != before[i].ID || e.Kind != before[i].Kind {
			t.Fatalf("event %d changed id or kind", e.ID)
		}
		if !slices.Contains(BulkyKinds, e.Kind) {
			if string(e.Data) != string(before[i].Data) {
				t.Fatalf("%s data changed", e.Kind)
			}
			continue
		}
		var d struct {
			Pruned bool
			Bytes  int
			Event  string
		}
		if err := json.Unmarshal(e.Data, &d); err != nil || !d.Pruned || d.Bytes != len(before[i].Data) {
			t.Fatalf("%s: %s", e.Kind, e.Data)
		}
		if e.Kind == KindHook && d.Event == "" {
			t.Fatalf("pruned hook lost its name: %s", e.Data)
		}
	}
}

func TestCutStringStaysWithinLimit(t *testing.T) {
	for _, limit := range []int{0, 5, 20, 21, 1024} {
		got := CutString(strings.Repeat("é", 2000), limit)
		if len(got) > max(limit, len("…[4000 bytes]")) || !utf8.ValidString(got) || !strings.HasSuffix(got, "…[4000 bytes]") {
			t.Fatalf("limit %d: %d bytes %q", limit, len(got), got)
		}
	}
	if got := CutString("short", 5); got != "short" {
		t.Fatalf("fits: %q", got)
	}
}

func TestRunAccountFolds(t *testing.T) {
	s := openFolded(t)
	if err := s.CreateRun(Run{ID: "r", Goal: "g", Account: "work", Steps: []Step{{Name: "a"}}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run("r")
	if err != nil || r.Account != "work" {
		t.Fatalf("run %+v %v", r, err)
	}
	evs, err := s.Events(EventQuery{RunID: "r", Kinds: []string{KindRunCreated}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	var d RunCreated
	if err := json.Unmarshal(evs[0].Data, &d); err != nil || d.Account != "work" || d.V != EventVersion {
		t.Fatalf("payload %s %v", evs[0].Data, err)
	}
	if got := Fold(evs).Runs; len(got) != 1 || got[0].Account != "work" {
		t.Fatalf("fold %+v", got)
	}
}

func TestRunDirFolds(t *testing.T) {
	s := openFolded(t)
	if err := s.CreateRun(Run{ID: "r", Goal: "g", Dir: "/srv/repo", Steps: []Step{{Name: "a"}}}); err != nil {
		t.Fatal(err)
	}
	r, err := s.Run("r")
	if err != nil || r.Dir != "/srv/repo" {
		t.Fatalf("run %+v %v", r, err)
	}
	evs, err := s.Events(EventQuery{RunID: "r", Kinds: []string{KindRunCreated}})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %v %v", evs, err)
	}
	if got := Fold(evs).Runs; len(got) != 1 || got[0].Dir != "/srv/repo" {
		t.Fatalf("fold %+v", got)
	}
}
