package host

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/digest"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// finishedRun stores a one-step run whose step agent ended as end says,
// with handoff text, at finished.
func finishedRun(t *testing.T, st store.Store, id, goal string, end store.End, text string) {
	t.Helper()
	if err := st.CreateRun(store.Run{ID: id, Goal: goal, Workflow: "steps: []", Steps: []store.Step{{Name: "work"}}}); err != nil {
		t.Fatal(err)
	}
	agentID := id + "-lead"
	must(t, st.CreateAgent(store.Agent{ID: agentID, RunID: id, Step: "work", State: store.StateBusy}))
	must(t, st.StartStep(id, 0, agentID, end.At.Add(-time.Hour)))
	if end.State == store.StateDone || end.State == store.StateBlocked {
		must(t, st.Done(agentID, store.Handoff{Text: text}, end.Reason, end.At))
	} else {
		must(t, st.EndAgent(agentID, store.End{State: end.State, Reason: end.Reason, At: end.At}))
	}
	must(t, st.FinishStep(id, 0, end))
	must(t, st.FinishRun(id, end))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func digestKinds(v digest.View) map[string][]string {
	out := map[string][]string{}
	for _, g := range v.Groups {
		key := "precedents"
		if g.Run != nil {
			key = g.Run.ID
		}
		for _, it := range g.Items {
			out[key] = append(out[key], it.Kind)
		}
	}
	return out
}

func TestDigestGathersWhatNeedsThePerson(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	delivered := store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: now.Add(-time.Hour)}

	finishedRun(t, st, "clean", "fix the upload bug", delivered, "fixed")
	finishedRun(t, st, "rfc", "humanize the o11y RFC", store.End{State: store.StateBlocked, Reason: store.ReasonBlocked, At: now.Add(-2 * time.Hour)}, "Should it cover tracing too?")
	finishedRun(t, st, "map", "map the infra", store.End{State: store.StateFailed, Reason: store.ReasonExited, At: now.Add(-3 * time.Hour)}, "")
	finishedRun(t, st, "killed", "a run the person stopped", store.End{State: store.StateKilled, Reason: store.ReasonKilled, At: now.Add(-time.Hour)}, "")
	finishedRun(t, st, "old", "yesterday's run", store.End{State: store.StateBlocked, Reason: store.ReasonBlocked, At: now.Add(-30 * time.Hour)}, "old question")
	finishedRun(t, st, "explore", "explore o11y for atlas", delivered, "report")
	must(t, st.CreateRun(store.Run{ID: "live", Goal: "still going", Steps: []store.Step{{Name: "work"}}}))

	must(t, st.CreateAgent(store.Agent{ID: "scout-1", ParentID: "explore-lead", Role: "scout", RunID: "explore", State: store.StateBusy}))
	must(t, st.EndAgent("scout-1", store.End{State: store.StateFailed, Reason: store.ReasonExited}))
	must(t, st.CreateAgent(store.Agent{ID: "scout-2", ParentID: "explore-lead", Role: "scout", RunID: "explore", State: store.StateBusy}))
	must(t, st.Done("scout-2", store.Handoff{Text: "ok"}, store.ReasonDelivered, now))

	must(t, st.AddClaims("explore-lead", []store.Claim{
		{RunID: "explore", Text: "dev-ledger is drift", Evidence: []string{"git branch -a"}},
		{RunID: "explore", Text: "Loki is cheaper"},
		{RunID: "explore", Text: "retention is 15d"},
		{RunID: "explore", Text: "a"},
		{RunID: "explore", Text: "b"},
	}))
	for i, v := range []string{store.VerdictContradicted, store.VerdictUnverifiable, store.VerdictHolds, store.VerdictUnwitnessed, store.VerdictUnwitnessed} {
		must(t, st.SetVerdict(store.ClaimID("explore-lead", i), v, "note", "w", now))
	}
	must(t, st.AddClaims("clean-lead", []store.Claim{{RunID: "clean", Text: "fixed it"}}))
	must(t, st.SetVerdict(store.ClaimID("clean-lead", 0), store.VerdictHolds, "", "w", now))

	writePrecedent(t, f.host, "p-stale", []string{"atlas"}, now.AddDate(0, -1, 0), "Atlas deploys on fridays.")
	writePrecedent(t, f.host, "p-fresh", []string{"atlas"}, now.AddDate(0, 1, 0), "Atlas is the app.")
	writePrecedent(t, f.host, "p-healed", []string{"atlas"}, now.AddDate(0, 1, 0), "Was stale once, review_by moved on.")
	must(t, os.WriteFile(filepath.Join(f.host.PrecedentsDir(), "broken.md"), []byte("no frontmatter"), 0o644))
	for _, e := range []store.Event{
		{RunID: "explore", AgentID: "explore-lead", Kind: store.KindPrecedentApplied, Data: json.RawMessage(`{"ids":["p-fresh"]}`)},
		{RunID: "explore", AgentID: "explore-lead", Kind: store.KindPrecedentStale, Data: json.RawMessage(`{"ids":["p-stale","p-healed"]}`)},
		{RunID: "old", AgentID: "old-lead", Kind: store.KindPrecedentApplied, Data: json.RawMessage(`{"ids":["p-fresh"]}`), At: now.Add(-31 * time.Hour)},
	} {
		must(t, st.Append(e))
	}

	v, err := f.host.Digest(DigestQuery{Since: now.Add(-12 * time.Hour), Max: 0})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"rfc":        {digest.KindBlocked},
		"map":        {digest.KindDead},
		"explore":    {digest.KindContradicted, digest.KindHelperDied, digest.KindUnverifiable, digest.KindUnwitnessed},
		"precedents": {digest.KindStalePrecedent, digest.KindBadPrecedent},
	}
	got := digestKinds(v)
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s: got %v, want %v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("groups %v", got)
	}
	var clean []string
	for _, r := range v.Clean {
		clean = append(clean, r.ID)
	}
	slices.Sort(clean)
	if !slices.Equal(clean, []string{"clean", "killed"}) || v.Running != 1 {
		t.Fatalf("clean %v running %d", clean, v.Running)
	}
	for _, g := range v.Groups {
		if g.Run != nil && g.Run.ID == "explore" && !slices.Equal(g.Run.Precedents, []string{"p-fresh"}) {
			t.Fatalf("explore precedents %v", g.Run.Precedents)
		}
		for _, it := range g.Items {
			switch it.Kind {
			case digest.KindBlocked:
				if it.Text != "Should it cover tracing too?" {
					t.Errorf("question %q", it.Text)
				}
			case digest.KindContradicted:
				if it.Ref != "explore-lead-0" || !strings.Contains(it.Action, "from-claim explore-lead-0") || it.Evidence[0] != "git branch -a" {
					t.Errorf("contradicted %+v", it)
				}
			case digest.KindUnwitnessed:
				if it.Count != 2 {
					t.Errorf("unwitnessed %+v", it)
				}
			case digest.KindHelperDied:
				if it.Count != 1 || !strings.Contains(it.Text, "scout (exited)") {
					t.Errorf("helpers %+v", it)
				}
			case digest.KindStalePrecedent:
				if !strings.HasPrefix(it.Text, "p-stale:") {
					t.Errorf("stale %+v", it)
				}
			}
		}
	}

	// One run, whenever it finished: only its items, no global ones.
	one, err := f.host.Digest(DigestQuery{RunID: "old", Since: now, Max: 7})
	if err != nil {
		t.Fatal(err)
	}
	if got := digestKinds(one); len(got) != 1 || !slices.Equal(got["old"], []string{digest.KindBlocked}) || one.Running != 0 {
		t.Fatalf("run old: %v", got)
	}
}

func TestDigestRoute(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/api/digest?since=2h&max=3")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var v digest.View
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil || !v.Empty() {
		t.Fatalf("%s %+v", resp.Status, v)
	}
	if d := time.Since(v.Since); d < 2*time.Hour || d > 2*time.Hour+time.Minute {
		t.Fatalf("since %v", v.Since)
	}
	for _, q := range []string{"since=yesterday", "max=-1"} {
		resp, err := http.Get(f.srv.URL + "/api/digest?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %s", q, resp.Status)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 9, 14, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"":                     now.Add(-12 * time.Hour),
		"90m":                  now.Add(-90 * time.Minute),
		"2d":                   now.AddDate(0, 0, -2),
		"2026-11-08":           time.Date(2026, 11, 8, 0, 0, 0, 0, time.UTC),
		"2026-11-09T08:00:00Z": time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC),
	} {
		got, err := ParseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
}

func TestDigestCountsWU2AndArenaEnds(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	delivered := store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: now.Add(-time.Hour)}

	finishedRun(t, st, "nostart", "a lead that never started", store.End{State: store.StateFailed, Reason: store.ReasonStartFailed, At: now.Add(-time.Hour)}, "")
	finishedRun(t, st, "arenafail", "an arena with too few entries", store.End{State: store.StateFailed, Reason: arenaReasonTooFew, At: now.Add(-time.Hour)}, "")
	finishedRun(t, st, "fanout", "a lead whose helpers stalled", delivered, "done anyway")
	must(t, st.CreateAgent(store.Agent{ID: "stalled", ParentID: "fanout-lead", Role: "scout", RunID: "fanout", State: store.StateBusy}))
	must(t, st.EndAgent("stalled", store.End{State: store.StateKilled, Reason: store.ReasonLost}))
	must(t, st.CreateAgent(store.Agent{ID: "idle", ParentID: "fanout-lead", Role: "scout", RunID: "fanout", State: store.StateBusy}))
	must(t, st.EndAgent("idle", store.End{State: store.StateKilled, Reason: store.ReasonClosed}))

	// A witnessed run whose witness stalled: its note is about claims.
	finishedRun(t, st, "witnessed", "a run whose witness stalled", delivered, "done")
	must(t, st.CreateAgent(store.Agent{ID: "wit", ParentID: "witnessed-lead", Role: WitnessRole, RunID: "witnessed", State: store.StateBusy}))
	must(t, st.EndAgent("wit", store.End{State: store.StateKilled, Reason: store.ReasonLost}))

	// An arena that delivered, though one judge's seat timed out.
	must(t, st.CreateRun(store.Run{ID: "arena", Goal: "which db?", Workflow: "steps: []", Steps: []store.Step{{Name: "propose-1"}, {Name: "judge-1"}}}))
	for i, name := range []string{"propose-1", "judge-1"} {
		id := "arena-" + name
		must(t, st.CreateAgent(store.Agent{ID: id, ParentID: arenaParentPrefix + "arena", RunID: "arena", Step: name, State: store.StateBusy}))
		must(t, st.StartStep("arena", i, id, now.Add(-2*time.Hour)))
	}
	must(t, st.Done("arena-propose-1", store.Handoff{Text: "postgres"}, store.ReasonDelivered, now.Add(-90*time.Minute)))
	must(t, st.FinishStep("arena", 0, store.End{State: store.StateDone, Reason: store.ReasonDelivered}))
	must(t, st.EndAgent("arena-judge-1", store.End{State: store.StateKilled, Reason: store.ReasonKilled}))
	must(t, st.FinishStep("arena", 1, store.End{State: store.StateFailed, Reason: arenaReasonTimedOut}))
	must(t, st.FinishRun("arena", delivered))

	v, err := f.host.Digest(DigestQuery{Since: now.Add(-12 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"nostart":   {digest.KindDead},
		"arenafail": {digest.KindDead},
		"fanout":    {digest.KindHelperDied},
		"arena":     {digest.KindHelperDied},
		"witnessed": {digest.KindHelperDied},
	}
	got := digestKinds(v)
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s: got %v, want %v", k, got[k], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("groups %v", got)
	}
	for _, g := range v.Groups {
		for _, it := range g.Items {
			switch {
			case g.Run.ID == "fanout" && (it.Count != 1 || !strings.Contains(it.Text, "scout (lost)") || it.Note != "the lead carried on without their handoff"):
				t.Errorf("fanout %+v", it)
			case g.Run.ID == "arena" && (it.Count != 1 || !strings.Contains(it.Text, "judge-1 (timed_out)")):
				t.Errorf("arena %+v", it)
			case g.Run.ID == "witnessed" && (it.Count != 1 || it.Note != "the claims a witness was checking went unwitnessed and the run carried on"):
				t.Errorf("witnessed %+v", it)
			}
		}
	}
}

func TestDiedNote(t *testing.T) {
	if got := diedNote(2, 1); got != "the lead carried on without their handoff; the claims a witness was checking went unwitnessed" {
		t.Fatalf("mixed: %q", got)
	}
}

func TestDigestAsksForTheArenaPick(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	delivered := store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: now.Add(-time.Hour)}
	arena := func(id, final string, standings []ArenaStanding) string {
		must(t, st.CreateRun(store.Run{ID: id, Goal: "which db?", Workflow: "steps: []", Source: SourceArena}))
		must(t, st.FinishRun(id, delivered))
		dir := filepath.Join(f.host.arenasDir(), id)
		done := now.Add(-time.Hour)
		a := Arena{ID: id, Question: "which db?", State: ArenaDone, Final: final, Dir: dir,
			Report: filepath.Join(dir, "arena.md"), Standings: standings, DoneAt: &done}
		b, _ := json.Marshal(a)
		writeFile(t, filepath.Join(dir, "arena.json"), string(b))
		return filepath.Join(dir, "pick.md")
	}
	two := []ArenaStanding{{Rank: 1, Label: "B"}, {Rank: 2, Label: "A"}}
	pick := arena("human", FinalHuman, two)
	arena("evolved", FinalEvolver, two)
	arena("lonely", FinalHuman, two[:1])

	items := func() map[string][]digest.Item {
		v, err := f.host.Digest(DigestQuery{Since: now.Add(-12 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]digest.Item{}
		for _, g := range v.Groups {
			out[g.Run.ID] = append(out[g.Run.ID], g.Items...)
		}
		return out
	}
	got := items()
	if len(got) != 1 || len(got["human"]) != 1 {
		t.Fatalf("only the human-final arena with two ranked entries waits on a pick: %+v", got)
	}
	it := got["human"][0]
	if it.Kind != digest.KindPick || it.Ref != pick || !strings.Contains(it.Action, pick) || !strings.HasPrefix(it.Text, "B or A: which db?") {
		t.Fatalf("pick item %+v", it)
	}
	if md := digest.Markdown(digest.Build(digest.Input{Items: []digest.Item{it}}, 0)); !strings.Contains(md, "your pick") || !strings.Contains(md, pick) {
		t.Fatalf("markdown:\n%s", md)
	}

	// An empty pick.md is no pick; one with a label is.
	writeFile(t, pick, "")
	if len(items()["human"]) != 1 {
		t.Fatal("an empty pick.md hid the item")
	}
	writeFile(t, pick, "B: cheaper to run\n")
	if got := items(); len(got) != 0 {
		t.Fatalf("picked, still listed: %+v", got)
	}
}

func digestKeys(v digest.View) map[string]bool {
	out := map[string]bool{}
	for _, g := range v.Groups {
		for _, it := range g.Items {
			out[it.Key] = it.Dismissed
		}
	}
	return out
}

func TestDigestItemKeysAreStable(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	finishedRun(t, st, "rfc", "humanize the RFC", store.End{State: store.StateBlocked, Reason: store.ReasonBlocked, At: now.Add(-time.Hour)}, "tracing too?")
	finishedRun(t, st, "map", "map the infra", store.End{State: store.StateFailed, Reason: store.ReasonExited, At: now.Add(-time.Hour)}, "")
	must(t, st.AddClaims("rfc-lead", []store.Claim{{RunID: "rfc", Text: "a"}, {RunID: "rfc", Text: "b"}}))
	must(t, st.SetVerdict(store.ClaimID("rfc-lead", 0), store.VerdictContradicted, "no", "w", now))
	must(t, st.SetVerdict(store.ClaimID("rfc-lead", 1), store.VerdictUnwitnessed, "", "w", now))
	writePrecedent(t, f.host, "p-stale", []string{"x"}, now.AddDate(0, -1, 0), "old news")
	must(t, st.Append(store.Event{RunID: "rfc", Kind: store.KindPrecedentStale, Data: json.RawMessage(`{"ids":["p-stale"]}`)}))

	want := map[string]bool{
		"blocked:rfc:work": false, "contradicted:rfc:rfc-lead-0": false, "unwitnessed:rfc": false,
		"dead:map": false, "stale_precedent:p-stale": false,
	}
	for range 2 {
		v, err := f.host.Digest(DigestQuery{Since: now.Add(-12 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		got := digestKeys(v)
		if len(got) != len(want) {
			t.Fatalf("keys %v", got)
		}
		for k := range want {
			if _, ok := got[k]; !ok {
				t.Fatalf("no %s in %v", k, got)
			}
		}
		for _, g := range v.Groups {
			for _, it := range g.Items {
				if it.At.IsZero() {
					t.Errorf("%s has no time", it.Key)
				}
			}
		}
	}
}

func TestDigestDismissHidesUntilANewOccurrence(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	since := now.Add(-12 * time.Hour)
	finishedRun(t, st, "explore", "explore o11y", store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: now.Add(-time.Hour)}, "report")
	must(t, st.AddClaims("explore-lead", []store.Claim{{RunID: "explore", Text: "Loki is cheaper"}, {RunID: "explore", Text: "retention is 15d"}}))
	must(t, st.SetVerdict(store.ClaimID("explore-lead", 0), store.VerdictContradicted, "no", "w", now.Add(-time.Minute)))
	must(t, st.SetVerdict(store.ClaimID("explore-lead", 1), store.VerdictUnverifiable, "?", "w", now.Add(-time.Minute)))
	writePrecedent(t, f.host, "p-stale", []string{"x"}, now.AddDate(0, -1, 0), "old news")
	must(t, st.Append(store.Event{RunID: "explore", Kind: store.KindPrecedentStale, At: now.Add(-time.Minute), Data: json.RawMessage(`{"ids":["p-stale"]}`)}))

	digestOf := func(all bool) digest.View {
		t.Helper()
		v, err := f.host.Digest(DigestQuery{Since: since, All: all})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := digestOf(false); v.Shown != 3 || v.Dismissed != 0 {
		t.Fatalf("before: shown %d dismissed %d", v.Shown, v.Dismissed)
	}
	must(t, f.host.DismissDigest([]string{"contradicted:explore:explore-lead-0", "stale_precedent:p-stale", " "}))

	v := digestOf(false)
	if got := digestKeys(v); len(got) != 1 || v.Dismissed != 2 {
		t.Fatalf("after dismiss: %v dismissed %d", got, v.Dismissed)
	}
	if _, ok := digestKeys(v)["unverifiable:explore:explore-lead-1"]; !ok {
		t.Fatalf("the item not dismissed went: %v", digestKeys(v))
	}
	all := digestOf(true)
	if got := digestKeys(all); len(got) != 3 || !got["contradicted:explore:explore-lead-0"] || !got["stale_precedent:p-stale"] || got["unverifiable:explore:explore-lead-1"] {
		t.Fatalf("--all: %v", got)
	}
	if !strings.Contains(digest.Text(v), "2 cleared") || !strings.Contains(digest.Text(v), "done: aeolian digest dismiss unverifiable:explore:explore-lead-1") {
		t.Fatalf("text:\n%s", digest.Text(v))
	}
	if md := digest.Markdown(all); !strings.Contains(md, "- [x] **contradicted:**") || !strings.Contains(md, "- [ ] **unverifiable:**") {
		t.Fatalf("markdown:\n%s", md)
	}

	later := time.Now().Add(time.Minute)
	must(t, st.SetVerdict(store.ClaimID("explore-lead", 0), store.VerdictContradicted, "still no", "w2", later))
	must(t, st.Append(store.Event{RunID: "explore", Kind: store.KindPrecedentStale, At: later, Data: json.RawMessage(`{"ids":["p-stale"]}`)}))
	if got := digestKeys(digestOf(false)); len(got) != 3 || got["contradicted:explore:explore-lead-0"] || got["stale_precedent:p-stale"] {
		t.Fatalf("new occurrences stayed hidden: %v", got)
	}
}

func TestDigestAckHidesTheRunsGroup(t *testing.T) {
	f := newFixture(t)
	st := f.store
	now := time.Now()
	since := now.Add(-12 * time.Hour)
	finishedRun(t, st, "map", "map the infra", store.End{State: store.StateFailed, Reason: store.ReasonExited, At: now.Add(-time.Hour)}, "")
	finishedRun(t, st, "rfc", "humanize the RFC", store.End{State: store.StateBlocked, Reason: store.ReasonBlocked, At: now.Add(-time.Hour)}, "tracing?")
	must(t, st.CreateRun(store.Run{ID: "ticket", Goal: "fix ENG-1", Workflow: "steps: []", Source: "linear", Ref: "ENG-1"}))
	must(t, st.FinishRun("ticket", store.End{State: store.StateFailed, Reason: store.ReasonExited, At: now.Add(-time.Hour)}))

	if _, err := f.host.Ack("map"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.Ack("ticket"); err != nil {
		t.Fatal(err)
	}
	v, err := f.host.Digest(DigestQuery{Since: since})
	if err != nil {
		t.Fatal(err)
	}
	got := digestKinds(v)
	if len(got) != 2 || got["map"] != nil || got["rfc"] == nil || got["ticket"] == nil || v.Dismissed != 1 {
		t.Fatalf("after ack: %v dismissed %d", got, v.Dismissed)
	}
	if !slices.ContainsFunc(v.Clean, func(r digest.Run) bool { return r.ID == "map" }) {
		t.Fatalf("acked run not clean: %+v", v.Clean)
	}
	all, err := f.host.Digest(DigestQuery{Since: since, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := digestKeys(all); !got["dead:map"] || got["dead:ticket"] {
		t.Fatalf("--all: %v", got)
	}
}

func TestDigestDismissRoute(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	finishedRun(t, f.store, "map", "map the infra", store.End{State: store.StateFailed, Reason: store.ReasonExited, At: now.Add(-time.Hour)}, "")
	post := func(body string) int {
		t.Helper()
		resp, err := http.Post(f.srv.URL+"/api/digest/dismiss", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, bad := range []string{`{"keys":[]}`, `{"keys":[""]}`, `nope`} {
		if code := post(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if code := post(`{"keys":["dead:map"]}`); code != http.StatusOK {
		t.Fatalf("dismiss: %d", code)
	}
	get := func(q string) digest.View {
		t.Helper()
		resp, err := http.Get(f.srv.URL + "/api/digest?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var v digest.View
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
			t.Fatalf("%s: %s", q, resp.Status)
		}
		return v
	}
	if v := get("since=2h"); !v.Empty() || v.Dismissed != 1 {
		t.Fatalf("after dismiss: %+v", v)
	}
	if v := get("since=2h&all=1"); v.Shown != 1 || !v.Groups[0].Items[0].Dismissed || v.Groups[0].Items[0].Key != "dead:map" {
		t.Fatalf("all: %+v", v)
	}
	resp, err := http.Get(f.srv.URL + "/api/digest?all=maybe")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("all=maybe: %s", resp.Status)
	}
}
