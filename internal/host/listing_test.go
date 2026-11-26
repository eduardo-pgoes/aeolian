package host

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func getJSONStatus(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestListRunsLimitActiveInclude(t *testing.T) {
	f := newFixture(t)
	f.ackedRun(t, "old1", store.StateDone, ago(time.Hour))
	f.ackedRun(t, "old2", store.StateDone, ago(time.Hour))
	f.ackedRun(t, "unacked", store.StateDone, nil)
	var runs []RunView
	if code := getJSONStatus(t, f.srv.URL+"/api/runs", &runs); code != http.StatusOK || len(runs) != 3 {
		t.Fatalf("all runs: %d %d", code, len(runs))
	}
	for _, r := range runs {
		if len(r.Agents) != 1 || r.Agents[0].ID != "agent-"+r.ID {
			t.Fatalf("run %s agents: %+v", r.ID, r.Agents)
		}
	}
	got := map[string]bool{}
	if code := getJSONStatus(t, f.srv.URL+"/api/runs?limit=1&active=1&include=old1", &runs); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	got = map[string]bool{}
	for _, r := range runs {
		got[r.ID] = true
	}
	// The newest run is unacked; old1 is pinned; old2 is neither.
	if len(runs) != 2 || !got["unacked"] || !got["old1"] {
		t.Fatalf("limited runs: %v", got)
	}
	if code := getJSONStatus(t, f.srv.URL+"/api/runs?limit=x", &runs); code != http.StatusBadRequest {
		t.Fatalf("bad limit: %d", code)
	}
}

func TestListAgentsFilters(t *testing.T) {
	f := newFixture(t)
	f.ackedRun(t, "r1", store.StateDone, nil)
	live := f.start(t)
	var agents []store.Agent
	getJSONStatus(t, f.srv.URL+"/api/agents", &agents)
	if len(agents) != 1 || agents[0].ID != live.ID {
		t.Fatalf("default: %+v", agents)
	}
	getJSONStatus(t, f.srv.URL+"/api/agents?all=1", &agents)
	if len(agents) != 2 {
		t.Fatalf("all: %+v", agents)
	}
	getJSONStatus(t, f.srv.URL+"/api/agents?run=r1", &agents)
	if len(agents) != 1 || agents[0].ID != "agent-r1" {
		t.Fatalf("run: %+v", agents)
	}
}
