package store

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendAndEvents(t *testing.T) {
	s := open(t)
	if evs, err := s.Events(EventQuery{}); err != nil || len(evs) != 0 {
		t.Fatalf("empty log: %v %+v", err, evs)
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	err := s.Append(
		Event{At: at, RunID: "r1", AgentID: "a1", Kind: "agent.created", Data: json.RawMessage(`{"v":1}`)},
		Event{RunID: "r1", AgentID: "a2", Kind: "spawn.requested"},
		Event{RunID: "r2", AgentID: "a3", Kind: "agent.created"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{AgentID: "a1", Kind: "hook", Data: json.RawMessage(`{"event":"Stop"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(); err != nil {
		t.Fatal(err)
	}

	all, err := s.Events(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("all: %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("ids not increasing: %+v", all)
		}
	}
	if e := all[0]; !e.At.Equal(at) || e.RunID != "r1" || e.AgentID != "a1" || e.Kind != "agent.created" || string(e.Data) != `{"v":1}` {
		t.Fatalf("first event: %+v", e)
	}
	if e := all[1]; e.At.IsZero() || string(e.Data) != `{}` {
		t.Fatalf("defaults: %+v %s", e, e.Data)
	}

	for _, c := range []struct {
		name string
		q    EventQuery
		want []int // indexes into all
	}{
		{"after", EventQuery{After: all[1].ID}, []int{2, 3}},
		{"run", EventQuery{RunID: "r1"}, []int{0, 1}},
		{"agent", EventQuery{AgentID: "a1"}, []int{0, 3}},
		{"kinds", EventQuery{Kinds: []string{"agent.created", "hook"}}, []int{0, 2, 3}},
		{"combined", EventQuery{After: all[0].ID, Kinds: []string{"agent.created"}}, []int{2}},
		{"limit", EventQuery{Limit: 2}, []int{0, 1}},
		{"limit after", EventQuery{After: all[0].ID, Limit: 2}, []int{1, 2}},
		{"past the end", EventQuery{After: all[3].ID}, nil},
	} {
		got, err := s.Events(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("%s: got %d events, want %d: %+v", c.name, len(got), len(c.want), got)
		}
		for i, j := range c.want {
			if got[i].ID != all[j].ID {
				t.Fatalf("%s: event %d is %d, want %d", c.name, i, got[i].ID, all[j].ID)
			}
		}
	}
}

func TestEventsDefaultLimit(t *testing.T) {
	s := open(t)
	evs := make([]Event, DefaultEventLimit+5)
	for i := range evs {
		evs[i] = Event{Kind: "x"}
	}
	if err := s.Append(evs...); err != nil {
		t.Fatal(err)
	}
	got, err := s.Events(EventQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != DefaultEventLimit {
		t.Fatalf("got %d events", len(got))
	}
	rest, err := s.Events(EventQuery{After: got[len(got)-1].ID})
	if err != nil || len(rest) != 5 {
		t.Fatalf("rest: %v %d", err, len(rest))
	}
}

func TestEventIDsNeverReused(t *testing.T) {
	s := open(t)
	if err := s.Append(Event{Kind: "a"}, Event{Kind: "b"}); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Events(EventQuery{})
	if err := s.(*sqliteStore).db.Exec("DELETE FROM events WHERE id = ?", all[1].ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Event{Kind: "c"}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Events(EventQuery{After: all[0].ID})
	if len(got) != 1 || got[0].ID <= all[1].ID {
		t.Fatalf("id reused: %+v after %+v", got, all)
	}
}

func TestSubscribeCoalesces(t *testing.T) {
	s := open(t)
	sig, cancel := s.Subscribe()
	defer cancel()
	other, cancelOther := s.Subscribe()

	for range 3 {
		if err := s.Append(Event{Kind: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-sig:
	case <-time.After(time.Second):
		t.Fatal("no signal")
	}
	select {
	case <-sig:
		t.Fatal("three appends left more than one signal pending")
	default:
	}
	// The one signal is enough: the reader catches up from its cursor.
	if evs, _ := s.Events(EventQuery{}); len(evs) != 3 {
		t.Fatalf("events: %+v", evs)
	}

	select {
	case <-other:
	default:
		t.Fatal("second subscriber got no signal")
	}
	cancelOther()
	cancelOther()
	if err := s.Append(Event{Kind: "y"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-other:
		t.Fatal("cancelled subscriber got a signal")
	case <-sig:
	case <-time.After(time.Second):
		t.Fatal("no signal after cancel of another subscriber")
	}

	// A failed append wakes nobody.
	if err := s.Append(Event{}); err == nil {
		t.Fatal("event without a kind was stored")
	}
	select {
	case <-sig:
		t.Fatal("signal for a failed append")
	default:
	}
}

func TestSubscribeConcurrent(t *testing.T) {
	s := open(t)
	sig, cancel := s.Subscribe()
	defer cancel()
	const writers, each = 4, 25
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			for range each {
				if err := s.Append(Event{Kind: "x"}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	var cursor int64
	seen := 0
	deadline := time.After(10 * time.Second)
	for seen < writers*each {
		select {
		case <-sig:
		case <-deadline:
			t.Fatalf("saw %d of %d events", seen, writers*each)
		}
		evs, err := s.Events(EventQuery{After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.ID <= cursor {
				t.Fatalf("event %d at cursor %d", e.ID, cursor)
			}
			cursor = e.ID
		}
		seen += len(evs)
	}
	wg.Wait()
}

func TestLastEvents(t *testing.T) {
	s := open(t)
	err := s.Append(
		Event{AgentID: "a1", Kind: KindUsage, Data: json.RawMessage(`{"context":1}`)},
		Event{AgentID: "a2", Kind: KindUsage, Data: json.RawMessage(`{"context":2}`)},
		Event{AgentID: "a1", Kind: KindUsage, Data: json.RawMessage(`{"context":3}`)},
		Event{AgentID: "a1", Kind: KindToolUsed, Data: json.RawMessage(`{}`)},
		Event{AgentID: "a3", Kind: KindUsage, Data: json.RawMessage(`{"context":4}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	last, err := s.LastEvents(KindUsage, []string{"a1", "a2", "a9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 2 || string(last["a1"].Data) != `{"context":3}` || string(last["a2"].Data) != `{"context":2}` {
		t.Fatalf("last events: %+v", last)
	}
	if last, err := s.LastEvents(KindUsage, nil); err != nil || len(last) != 0 {
		t.Fatalf("no agents: %v %+v", err, last)
	}
}

func TestRunTokens(t *testing.T) {
	s := open(t)
	err := s.Append(
		Event{RunID: "r1", AgentID: "lead", Kind: KindUsage, Data: json.RawMessage(`{"input":10,"output":20,"cache_read":1000,"cache_write":30,"context":40}`)},
		Event{RunID: "r1", AgentID: "helper", Kind: KindUsage, Data: json.RawMessage(`{"input":1,"output":2,"cache_read":500,"cache_write":3}`)},
		Event{RunID: "r1", AgentID: "lead", Kind: KindToolUsed, Data: json.RawMessage(`{"input":999}`)},
		Event{RunID: "r1", AgentID: "lead", Kind: KindUsage, Data: json.RawMessage(`{"output":4}`)},
		Event{RunID: "r2", AgentID: "other", Kind: KindUsage, Data: json.RawMessage(`{"input":7}`)},
		Event{RunID: "r3", AgentID: "skipped", Kind: KindUsage, Data: json.RawMessage(`{"input":8}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RunTokens([]string{"r1", "r2", "r9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["r1"] != 70 || got["r2"] != 7 {
		t.Fatalf("run tokens: %+v", got)
	}
	if got, err := s.RunTokens(nil); err != nil || len(got) != 0 {
		t.Fatalf("no runs: %v %+v", err, got)
	}
}

func TestRunTokensUsesRunKindIndex(t *testing.T) {
	s := open(t).(*sqliteStore)
	var plan []struct {
		Detail string
	}
	q := `EXPLAIN QUERY PLAN SELECT run_id, SUM(1) FROM events WHERE kind = ? AND run_id IN (?) GROUP BY run_id`
	if err := s.db.Raw(q, KindUsage, "r1").Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for _, p := range plan {
		if strings.Contains(p.Detail, "idx_events_run_kind") {
			return
		}
	}
	t.Fatalf("plan: %+v", plan)
}

func TestReopenedTokens(t *testing.T) {
	s := open(t)
	err := s.Append(
		Event{RunID: "r1", AgentID: "lead", Kind: KindUsage, Data: json.RawMessage(`{"input":10,"output":20,"cache_read":1000,"cache_write":30}`)},
		Event{RunID: "r1", AgentID: "helper", Kind: KindUsage, Data: json.RawMessage(`{"input":1}`)},
		Event{RunID: "r1", AgentID: "lead", Kind: KindAgentState, Data: json.RawMessage(`{"from":"done","to":"idle","cause":"reopen"}`)},
		Event{RunID: "r1", AgentID: "lead", Kind: KindUsage, Data: json.RawMessage(`{"input":100,"output":200,"cache_read":9000,"cache_write":300}`)},
		Event{RunID: "r1", AgentID: "helper", Kind: KindAgentState, Data: json.RawMessage(`{"from":"busy","to":"idle","cause":"hook"}`)},
		Event{RunID: "r1", AgentID: "helper", Kind: KindUsage, Data: json.RawMessage(`{"input":2}`)},
		Event{RunID: "r2", AgentID: "lead", Kind: KindUsage, Data: json.RawMessage(`{"input":7}`)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReopenedTokens("r1"); err != nil || got != 600 {
		t.Fatalf("r1: %d %v", got, err)
	}
	if got, err := s.ReopenedTokens("r9"); err != nil || got != 0 {
		t.Fatalf("r9: %d %v", got, err)
	}
}
