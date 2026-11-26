package host

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/precedent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func writePrecedent(t *testing.T, h *Host, id string, scope []string, reviewBy time.Time, body string) {
	t.Helper()
	if _, err := precedent.Write(h.PrecedentsDir(), precedent.Draft{
		ID: id, Scope: scope, ReviewBy: reviewBy, Source: "test", Body: body,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func precedentEvents(t *testing.T, st store.Store, agentID, kind string) []string {
	t.Helper()
	evs, err := st.Events(store.EventQuery{AgentID: agentID, Kinds: []string{kind}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range evs {
		var p store.PrecedentIDs
		json.Unmarshal(e.Data, &p)
		ids = append(ids, p.IDs...)
	}
	return ids
}

func TestPrecedentsInjectOnlyWhenEveryScopeTermMatches(t *testing.T) {
	f := newFixture(t)
	later := time.Now().AddDate(0, 1, 0)
	writePrecedent(t, f.host, "p01-dev-ledger", []string{"dev-ledger", "branch"}, later, "dev-ledger is a PROJECT branch, not drift.")
	writePrecedent(t, f.host, "p02-atlas", []string{"atlas"}, later, "Atlas deploys from main only.")

	both, err := f.host.Start(context.Background(), "Is the dev-ledger branch in atlas drift?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(both.Prompt, precedent.Heading) || !strings.Contains(both.Prompt, "PROJECT branch") || !strings.Contains(both.Prompt, "main only") {
		t.Fatalf("prompt lacks the precedents:\n%s", both.Prompt)
	}
	// The narrower, more specific precedent comes first.
	if strings.Index(both.Prompt, "p01-dev-ledger") > strings.Index(both.Prompt, "p02-atlas") {
		t.Fatalf("order:\n%s", both.Prompt)
	}
	if got := precedentEvents(t, f.store, both.ID, store.KindPrecedentApplied); !slices.Equal(got, []string{"p01-dev-ledger", "p02-atlas"}) {
		t.Fatalf("applied events %v", got)
	}
	if stored, _ := f.store.Agent(both.ID); stored.Prompt != both.Prompt {
		t.Fatal("the stored prompt is not the one launched")
	}

	// dev-ledger alone is not enough: branch must appear too.
	one, err := f.host.Start(context.Background(), "What does dev-ledger deploy?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(one.Prompt, precedent.Heading) || len(precedentEvents(t, f.store, one.ID, store.KindPrecedentApplied)) != 0 {
		t.Fatalf("partial scope injected:\n%s", one.Prompt)
	}
}

func TestStalePrecedentIsFlaggedNotInjected(t *testing.T) {
	f := newFixture(t)
	writePrecedent(t, f.host, "p03-irc", []string{"irc"}, time.Now().AddDate(0, -1, 0), "IRC was replaced in 2026.")

	a, err := f.host.Start(context.Background(), "Where does the team chat, irc?")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.Prompt, "replaced") {
		t.Fatalf("stale precedent injected:\n%s", a.Prompt)
	}
	if got := precedentEvents(t, f.store, a.ID, store.KindPrecedentStale); !slices.Equal(got, []string{"p03-irc"}) {
		t.Fatalf("stale events %v", got)
	}
	if got := precedentEvents(t, f.store, a.ID, store.KindPrecedentApplied); len(got) != 0 {
		t.Fatalf("applied events %v", got)
	}
}

func TestBigPrecedentsGoToAFile(t *testing.T) {
	f := newFixture(t)
	later := time.Now().AddDate(0, 1, 0)
	for _, id := range []string{"p1-big", "p2-big", "p3-big"} {
		writePrecedent(t, f.host, id, []string{"atlas"}, later, strings.Repeat("A long correction about atlas. ", 40))
	}
	a, err := f.host.Start(context.Background(), "map atlas")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.host.Layout.ScratchDir(a.ID), "precedents.md")
	if !strings.Contains(a.Prompt, path) || strings.Contains(a.Prompt, "A long correction") || len(a.Prompt) > 600 {
		t.Fatalf("prompt:\n%s", a.Prompt)
	}
	b, err := os.ReadFile(path)
	if err != nil || strings.Count(string(b), "A long correction about atlas.") != 120 {
		t.Fatalf("file %v:\n%.200s", err, b)
	}
}

func TestPrecedentsReachStepAgentsAndHelpers(t *testing.T) {
	f := newFixture(t)
	writePrecedent(t, f.host, "p04-reviewer", []string{"reviewer", "widget"}, time.Now().AddDate(0, 1, 0), "Widgets ship without a changelog; do not ask for one.")

	resp := f.startRun(t, testWorkflow, "the widget")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start run: %s", resp.Status)
	}
	var run store.Run
	json.NewDecoder(resp.Body).Decode(&run)
	lead := f.stepAgent(t, run.ID, "build")
	// The lead's role is empty, so the AND-scope with reviewer misses it.
	if strings.Contains(lead.Prompt, "changelog") {
		t.Fatalf("lead prompt:\n%s", lead.Prompt)
	}

	session := f.connect(t, lead.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.spawn(ctx, t, session, "reviewer")
	helper := f.helperOf(t, lead.ID)
	if !strings.Contains(helper.Prompt, "changelog") {
		t.Fatalf("helper prompt:\n%s", helper.Prompt)
	}
	if got := precedentEvents(t, f.store, helper.ID, store.KindPrecedentApplied); !slices.Equal(got, []string{"p04-reviewer"}) {
		t.Fatalf("applied %v", got)
	}
	evs, _ := f.store.Events(store.EventQuery{AgentID: helper.ID, Kinds: []string{store.KindPrecedentApplied}})
	if len(evs) != 1 || evs[0].RunID != run.ID {
		t.Fatalf("event %+v", evs)
	}
}

func TestPrecedentFromClaim(t *testing.T) {
	f := newFixture(t)
	if err := f.store.AddClaims("agent-1", []store.Claim{
		{RunID: "run-1", Text: "The dev-ledger branch is drift and should be deleted"},
	}); err != nil {
		t.Fatal(err)
	}
	id := store.ClaimID("agent-1", 0)
	if err := f.store.SetVerdict(id, store.VerdictContradicted, "it has open PRs", "w", time.Now()); err != nil {
		t.Fatal(err)
	}

	body := `{"claim_id":"` + id + `","text":"dev-ledger is a project branch, not drift."}`
	resp, err := http.Post(f.srv.URL+"/api/precedents", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("post: %s", resp.Status)
	}
	var p precedent.Precedent
	json.NewDecoder(resp.Body).Decode(&p)
	if !slices.Equal(p.Scope, []string{"dev-ledger"}) {
		t.Fatalf("scope %q", p.Scope)
	}
	if p.Source != "run run-1, claim "+id+" (contradicted)" || p.Kind != precedent.KindCorrection {
		t.Fatalf("got %+v", p)
	}
	if !strings.Contains(p.Body, "not drift") || !strings.Contains(p.Body, `"The dev-ledger branch is drift and should be deleted"`) {
		t.Fatalf("body %q", p.Body)
	}
	ps, errs := precedent.Load(f.host.PrecedentsDir())
	if len(errs) != 0 || len(ps) != 1 || ps[0].ID != p.ID {
		t.Fatalf("on disk %+v %v", ps, errs)
	}

	// A claim that does not exist is a 404, and nothing is written.
	resp2, _ := http.Post(f.srv.URL+"/api/precedents", "application/json", strings.NewReader(`{"claim_id":"nope","text":"x"}`))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("missing claim: %s", resp2.Status)
	}
	if ps, _ := precedent.Load(f.host.PrecedentsDir()); len(ps) != 1 {
		t.Fatalf("%d precedents", len(ps))
	}
}

func TestWitnessAndJudgesTakeNoPrecedents(t *testing.T) {
	arena := arenaParentPrefix + "r"
	for _, tc := range []struct {
		a    store.Agent
		want bool
	}{
		{store.Agent{}, true},
		{store.Agent{ParentID: "lead", Role: "reviewer"}, true},
		{store.Agent{ParentID: "lead", Role: WitnessRole}, false},
		{store.Agent{ParentID: arena, Role: ArenaRoleJudge}, false},
		{store.Agent{ParentID: arena, Role: ArenaRoleProposer}, true},
		{store.Agent{ParentID: arena, Role: ArenaRoleEvolver}, true},
	} {
		if got := takesPrecedents(tc.a); got != tc.want {
			t.Errorf("%+v: %v", tc.a, got)
		}
	}

	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	writePrecedent(t, f.host, "p-faxbridge", []string{"faxbridge"}, time.Now().AddDate(0, 1, 0), "Faxbridge is not used any more.")
	_, a := f.startWitnessed(t, witnessClaims)
	w := f.witnessOf(t, a.ID)
	if got := precedentEvents(t, f.store, w.ID, store.KindPrecedentApplied); len(got) != 0 {
		t.Fatalf("witness got precedents at launch: %v", got)
	}
	if strings.Contains(w.Prompt, "Faxbridge is not used any more") {
		t.Fatal("the host's precedents reached the witness's prompt")
	}
}

func TestPrecedentsReachFannedOutHelpers(t *testing.T) {
	f := newFixture(t)
	writePrecedent(t, f.host, "p04-reviewer", []string{"reviewer", "widget"}, time.Now().AddDate(0, 1, 0), "Widgets ship without a changelog; do not ask for one.")
	_, build := f.startHeadless(t, headlessWorkflow)
	id := f.spawnLater(t, f.connect(t, build.ID), "reviewer", "review the widget")
	f.waitStarted(t, id)
	if h := f.agent(t, id); !strings.Contains(h.Prompt, "changelog") {
		t.Fatalf("helper prompt:\n%s", h.Prompt)
	}
	if got := precedentEvents(t, f.store, id, store.KindPrecedentApplied); !slices.Equal(got, []string{"p04-reviewer"}) {
		t.Fatalf("applied %v", got)
	}
}

func writeDraft(t *testing.T, h *Host, d precedent.Draft) {
	t.Helper()
	if d.ReviewBy.IsZero() {
		d.ReviewBy = time.Now().AddDate(0, 1, 0)
	}
	if d.Source == "" {
		d.Source = "test"
	}
	if _, err := precedent.Write(h.PrecedentsDir(), d, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestGlobalAndRolePrecedentsReachStepAgentsAndHelpers(t *testing.T) {
	f := newFixture(t)
	writeDraft(t, f.host, precedent.Draft{ID: "g-everyone", Kind: precedent.KindSafety, Scope: []string{precedent.Any}, Body: "Never push to main."})
	writeDraft(t, f.host, precedent.Draft{ID: "r-build", Scope: []string{precedent.Any}, Roles: []string{"build"}, Body: "The build step asks before merging."})
	writeDraft(t, f.host, precedent.Draft{ID: "r-reviewer", Scope: []string{precedent.Any}, Roles: []string{"reviewer"}, Body: "Reviewers cite file and line."})

	resp := f.startRun(t, testWorkflow, "the widget")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("start run: %s", resp.Status)
	}
	var run store.Run
	json.NewDecoder(resp.Body).Decode(&run)
	lead := f.stepAgent(t, run.ID, "build")
	if got := precedentEvents(t, f.store, lead.ID, store.KindPrecedentApplied); !slices.Equal(got, []string{"r-build", "g-everyone"}) {
		t.Fatalf("lead applied %v:\n%s", got, lead.Prompt)
	}

	session := f.connect(t, lead.ID)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.spawn(ctx, t, session, "reviewer")
	helper := f.helperOf(t, lead.ID)
	if got := precedentEvents(t, f.store, helper.ID, store.KindPrecedentApplied); !slices.Equal(got, []string{"r-reviewer", "g-everyone"}) {
		t.Fatalf("helper applied %v:\n%s", got, helper.Prompt)
	}

	bare, err := f.host.Start(context.Background(), "say hi")
	if err != nil {
		t.Fatal(err)
	}
	if got := precedentEvents(t, f.store, bare.ID, store.KindPrecedentApplied); !slices.Equal(got, []string{"g-everyone"}) {
		t.Fatalf("bare agent applied %v", got)
	}
}

func TestRepoPrecedentsMatchTheWorktreesRepo(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "feature-x")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "-q", "-b", "feature-x", wt).CombinedOutput(); err != nil {
		t.Fatalf("worktree: %v %s", err, out)
	}
	f.host.Repos = map[string]Repo{"atlas": {Path: repo}}
	writeDraft(t, f.host, precedent.Draft{ID: "by-base", Scope: []string{precedent.Any}, Repos: []string{"repo"}, Body: "By basename."})
	writeDraft(t, f.host, precedent.Draft{ID: "by-key", Scope: []string{precedent.Any}, Repos: []string{"atlas"}, Body: "By config key."})
	writeDraft(t, f.host, precedent.Draft{ID: "elsewhere", Scope: []string{precedent.Any}, Repos: []string{"aeolian"}, Body: "Not here."})

	for name, dir := range map[string]string{"main checkout": repo, "worktree": wt} {
		_, applied, _ := f.host.withPrecedents(store.Agent{ID: "a", Dir: dir, Prompt: "work"})
		if !slices.Equal(applied, []string{"by-base", "by-key"}) {
			t.Errorf("%s: applied %v", name, applied)
		}
	}
	if _, applied, _ := f.host.withPrecedents(store.Agent{ID: "a", Dir: t.TempDir(), Prompt: "work in repo atlas"}); len(applied) != 0 {
		t.Errorf("scratch dir: applied %v", applied)
	}
}

func TestGlobalPrecedentsKeepTheirSlotsUnderTheCap(t *testing.T) {
	f := newFixture(t)
	for i := range 9 {
		writeDraft(t, f.host, precedent.Draft{ID: fmt.Sprintf("t%d", i), Scope: []string{"atlas"}, Body: "Specific."})
	}
	writeDraft(t, f.host, precedent.Draft{ID: "t-narrow", Scope: []string{"atlas", "deploy"}, Body: "Narrow."})
	for _, id := range []string{"g1", "g2", "g3", "g4"} {
		writeDraft(t, f.host, precedent.Draft{ID: id, Scope: []string{precedent.Any}, Body: "Global."})
	}
	_, applied, _ := f.host.withPrecedents(store.Agent{ID: "a", Dir: t.TempDir(), Prompt: "deploy atlas"})
	if want := []string{"t-narrow", "t0", "t1", "t2", "t3", "g1", "g2", "g3"}; !slices.Equal(applied, want) {
		t.Fatalf("applied %v, want %v", applied, want)
	}
}

func TestGlobalPrecedentsSkipWitnessesAndJudges(t *testing.T) {
	f := newFixture(t)
	writeDraft(t, f.host, precedent.Draft{ID: "g", Scope: []string{precedent.Any}, Body: "Global."})
	for _, a := range []store.Agent{
		{ID: "w", Role: WitnessRole, ParentID: "p", Prompt: "check"},
		{ID: "j", Role: ArenaRoleJudge, ParentID: arenaParentPrefix + "x", Prompt: "judge"},
	} {
		if prompt, applied, _ := f.host.withPrecedents(a); len(applied) != 0 || prompt != a.Prompt {
			t.Errorf("%s got %v", a.Role, applied)
		}
	}
	if _, applied, _ := f.host.withPrecedents(store.Agent{ID: "h", Role: ArenaRoleJudge, Prompt: "a helper named judge"}); !slices.Equal(applied, []string{"g"}) {
		t.Errorf("plain helper got %v", applied)
	}
}

func TestDraftPrecedentGlobalAndRoles(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		req   PrecedentRequest
		scope []string
		err   string
	}{
		"global":           {req: PrecedentRequest{Global: true, Text: "t"}, scope: []string{"*"}},
		"role only":        {req: PrecedentRequest{Roles: []string{"lead"}, Text: "t"}, scope: []string{"*"}},
		"repo only":        {req: PrecedentRequest{Repos: []string{"aeolian"}, Text: "t"}, scope: []string{"*"}},
		"role and words":   {req: PrecedentRequest{Roles: []string{"lead"}, Scope: []string{"merge"}, Text: "t"}, scope: []string{"merge"}},
		"global and scope": {req: PrecedentRequest{Global: true, Scope: []string{"x"}, Text: "t"}, err: "global"},
		"global and role":  {req: PrecedentRequest{Global: true, Roles: []string{"lead"}, Text: "t"}, err: "global"},
	} {
		d, err := DraftPrecedent(tc.req, nil, now)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%s: err %v", name, err)
			}
			continue
		}
		if err != nil || !slices.Equal(d.Scope, tc.scope) {
			t.Errorf("%s: scope %q err %v", name, d.Scope, err)
		}
	}
}

func TestBigPrecedentsReachAStepWithItsOwnDir(t *testing.T) {
	f := newFixture(t)
	later := time.Now().AddDate(0, 1, 0)
	for _, id := range []string{"p1-big", "p2-big", "p3-big"} {
		writePrecedent(t, f.host, id, []string{"atlas"}, later, strings.Repeat("A long correction about atlas. ", 40))
	}
	dir := t.TempDir()
	src := strings.Replace(testWorkflow, "    helpers: [reviewer]\n", "    helpers: [reviewer]\n    dir: "+dir+"\n", 1)
	resp := f.startRun(t, src, "map atlas")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	a := f.stepAgent(t, run.ID, "build")
	path := filepath.Join(f.host.Layout.ScratchDir(a.ID), "precedents.md")
	if a.Dir != dir || !strings.Contains(a.Prompt, path) {
		t.Fatalf("dir %q prompt:\n%s", a.Dir, a.Prompt)
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "A long correction") {
		t.Fatalf("file %v", err)
	}
}
