package host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func TestListEventsFilters(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	b := f.start(t)
	f.hook(t, a.ID, "Stop", map[string]any{"hook_event_name": "Stop"})
	f.hook(t, b.ID, "Stop", map[string]any{"hook_event_name": "Stop"})
	get := func(query string) (int, []store.Event) {
		t.Helper()
		resp, err := http.Get(f.srv.URL + "/api/events" + query)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var evs []store.Event
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&evs); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, evs
	}
	code, all := get("")
	if code != http.StatusOK || len(all) < 4 {
		t.Fatalf("all: %d %d events", code, len(all))
	}
	_, after := get(fmt.Sprintf("?after=%d", all[1].ID))
	if len(after) != len(all)-2 || after[0].ID != all[2].ID {
		t.Fatalf("after: %d events", len(after))
	}
	_, mine := get("?agent=" + a.ID + "&kind=agent.state,hook&kind=agent.created")
	for _, e := range mine {
		if e.AgentID != a.ID {
			t.Fatalf("other agent's event %+v", e)
		}
	}
	if len(mine) != 3 {
		t.Fatalf("a's events: %+v", mine)
	}
	if _, one := get("?limit=1"); len(one) != 1 {
		t.Fatalf("limit: %d", len(one))
	}
	if code, none := get("?run=nope"); code != http.StatusOK || none == nil || len(none) != 0 {
		t.Fatalf("no run: %d %v", code, none)
	}
	for _, q := range []string{"?after=x", "?after=-1", "?limit=0", fmt.Sprintf("?limit=%d", store.DefaultEventLimit+1)} {
		if code, _ := get(q); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", q, code)
		}
	}
}

func TestPruneEventsKeepsRecentRuns(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	old := f.finishedRun(t, "old", "", store.StateDone, now.Add(-40*24*time.Hour), &store.Handoff{Text: "done"})
	fresh := f.finishedRun(t, "fresh", "", store.StateDone, now.Add(-time.Hour), &store.Handoff{Text: "done"})
	for _, id := range []string{old.ID, fresh.ID} {
		f.hook(t, id, "Notification", map[string]any{"hook_event_name": "Notification", "message": strings.Repeat("m", 500)})
	}
	if n := f.host.PruneEvents(now); n != 0 {
		t.Fatalf("pruned %d with no retention", n)
	}
	f.host.EventsRetention = DefaultEventsRetention
	if n := f.host.PruneEvents(now); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for id, pruned := range map[string]bool{old.ID: true, fresh.ID: false} {
		evs := f.events(t, id, store.KindHook)
		got := strings.Contains(string(evs[len(evs)-1].Data), `"pruned":true`)
		if got != pruned {
			t.Fatalf("%s pruned %v: %s", id, got, evs[len(evs)-1].Data)
		}
	}
	if n := f.host.PruneEvents(now); n != 0 {
		t.Fatalf("pruned %d again", n)
	}
}
