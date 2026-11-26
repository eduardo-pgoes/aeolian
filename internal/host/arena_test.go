package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

var testPriors = []Prior{
	{Name: "cost-first", Text: "Optimize for cheapness above all."},
	{Name: "simplicity-first", Text: "Optimize for the smallest legible design."},
	{Name: "frontier-first", Text: "Optimize for raw capability."},
}

func newArenaFixture(t *testing.T, maxAgents int) *fixture {
	t.Helper()
	f := newFixture(t)
	f.keeper.killed = make(chan string, 256)
	f.host.MaxAgents = maxAgents
	return f
}

// arenaAgents is every agent of arena id in role, oldest first.
func arenaAgents(t *testing.T, f *fixture, id, role string) []store.Agent {
	t.Helper()
	all, err := f.store.Agents()
	if err != nil {
		t.Fatal(err)
	}
	var out []store.Agent
	for _, a := range all {
		if a.ParentID == "arena:"+id && a.Role == role {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(x, y store.Agent) int { return x.CreatedAt.Compare(y.CreatedAt) })
	return out
}

func waitArenaAgents(t *testing.T, f *fixture, id, role string, n int) []store.Agent {
	t.Helper()
	var got []store.Agent
	waitFor(t, "arena "+role+"s", func() bool {
		got = arenaAgents(t, f, id, role)
		return len(got) >= n
	})
	return got
}

func waitArenaState(t *testing.T, f *fixture, id, state string) Arena {
	t.Helper()
	var a Arena
	waitFor(t, "arena "+state, func() bool {
		var err error
		a, err = f.host.Arena(id)
		if err != nil {
			t.Fatal(err)
		}
		return a.State == state
	})
	return a
}

func callArenaTool(t *testing.T, f *fixture, agentID, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := f.connect(t, agentID).CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err.Error(), true
	}
	return resultText(res), res.IsError
}

// reads is everything an arena agent is given: its prompt and every file
// the host put in its folder.
func reads(t *testing.T, a store.Agent) string {
	t.Helper()
	all := a.Prompt
	filepath.WalkDir(a.Dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(path)
			all += "\n" + string(b)
		}
		return nil
	})
	return all
}

var pairLine = regexp.MustCompile(`(?m)^- a=([A-Z]) b=([A-Z])$`)

func promptPairs(prompt string) [][2]string {
	var out [][2]string
	for _, m := range pairLine.FindAllStringSubmatch(prompt, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// proposeAll has every proposer of the arena hand back a proposal that
// names its own lens, so anonymization has something to strip.
func proposeAll(t *testing.T, f *fixture, id string, n int, skip ...int) {
	t.Helper()
	props := waitArenaAgents(t, f, id, ArenaRoleProposer, n)
	a, _ := f.host.Arena(id)
	for _, e := range a.Entrants {
		if slices.Contains(skip, e.Index) {
			continue
		}
		i := slices.IndexFunc(props, func(p store.Agent) bool { return p.ID == e.AgentID })
		if i < 0 {
			t.Fatalf("entrant %d has no agent", e.Index)
		}
		p := testPriors[e.Index]
		f.done(t, props[i].ID, "As a "+p.Name+" thinker I say: "+p.Text+" Proposal number "+string(rune('0'+e.Index))+".")
	}
}

// judgeBy has judge agent jd judge every pair in its prompt, the winner
// picked by prefer from the two entrant indexes.
func judgeBy(t *testing.T, f *fixture, id string, jd store.Agent, prefer func(x, y int) (winner, margin int)) {
	t.Helper()
	idx := liveLabels(f, id)
	pairs := promptPairs(jd.Prompt)
	if len(pairs) == 0 {
		t.Fatalf("judge prompt lists no pairs:\n%s", jd.Prompt)
	}
	for _, p := range pairs {
		w, m := prefer(idx[p[0]], idx[p[1]])
		winner := p[0]
		if w == idx[p[1]] {
			winner = p[1]
		}
		if msg, isErr := callArenaTool(t, f, jd.ID, "judgment", map[string]any{
			"a": p[0], "b": p[1], "winner": winner, "margin": m, "why": "it was better", "steal": "steal from " + winner,
		}); isErr {
			t.Fatalf("judgment: %s", msg)
		}
	}
	f.done(t, jd.ID, "judged")
}

// liveLabels is the live arena's label -> entrant index map, which the API
// hides until judging ends.
func liveLabels(f *fixture, id string) map[string]int {
	ar := f.host.arenas().arena(id)
	if ar == nil {
		return nil
	}
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return maps.Clone(ar.byLabel)
}

// order prefers entrants earlier in rank.
func prefOrder(rank ...int) func(x, y int) (int, int) {
	return func(x, y int) (int, int) {
		if slices.Index(rank, x) < slices.Index(rank, y) {
			return x, 2
		}
		return y, 2
	}
}

func TestArenaRanksSplitsAndSynthesizes(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "what next?", Priors: testPriors, Judges: 2})
	if err != nil {
		t.Fatal(err)
	}
	if run := runState(t, f.host, a.ID); run.Source != SourceArena || run.State != store.StateBusy {
		t.Fatalf("run %+v", run)
	}
	proposeAll(t, f, a.ID, 3)
	judges := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 2)

	// Both judges rank 2 first. They split on 0 vs 1: judge one gives 0 a
	// decisive win, judge two gives 1 a slight one, so 0 and 1 tie on wins
	// and 0 goes above on margin.
	judgeBy(t, f, a.ID, judges[0], func(x, y int) (int, int) {
		if w, _ := prefOrder(2, 0, 1)(x, y); w == 0 {
			return 0, 3
		} else {
			return w, 2
		}
	})
	judgeBy(t, f, a.ID, judges[1], func(x, y int) (int, int) {
		if w, _ := prefOrder(2, 1, 0)(x, y); w == 1 {
			return 1, 1
		} else {
			return w, 2
		}
	})

	ev := waitArenaAgents(t, f, a.ID, ArenaRoleEvolver, 1)[0]
	for _, want := range []string{"steal from", "what next?", "judge 2 picked"} {
		if !strings.Contains(reads(t, ev), want) {
			t.Errorf("evolver prompt lacks %q", want)
		}
	}
	f.done(t, ev.ID, "the merged direction")
	got := waitArenaState(t, f, a.ID, ArenaDone)

	var priors []string
	for _, s := range got.Standings {
		priors = append(priors, s.Prior)
	}
	if want := []string{"frontier-first", "cost-first", "simplicity-first"}; !slices.Equal(priors, want) {
		t.Fatalf("standings %v, want %v", priors, want)
	}
	if len(got.Splits) != 1 || got.Splits[0].ForA != 1 || got.Splits[0].ForB != 1 || len(got.Splits[0].Why) != 2 {
		t.Fatalf("splits %+v", got.Splits)
	}
	waitFor(t, "run done", func() bool { return runState(t, f.host, a.ID).State == store.StateDone })

	report, err := os.ReadFile(got.Report)
	if err != nil {
		t.Fatal(err)
	}
	sp := got.Splits[0]
	for _, want := range []string{"## standings", "| 1 | " + got.Standings[0].Label + " | frontier-first",
		"## where the judges split", "### " + sp.A + " vs " + sp.B, got.Synthesis, "## synthesis"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("arena.md lacks %q:\n%s", want, report)
		}
	}
	if syn, _ := os.ReadFile(got.Synthesis); strings.TrimSpace(string(syn)) != "the merged direction" {
		t.Errorf("synthesis %q", syn)
	}
}

func TestArenaJudgePromptsAreAnonymous(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 2, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	proposeAll(t, f, a.ID, 3)
	judges := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 2)
	for _, jd := range judges {
		seen := reads(t, jd)
		if !strings.Contains(seen, "Proposal number") {
			t.Fatalf("judge was not given the entries:\n%s", seen)
		}
		for _, p := range testPriors {
			for _, leak := range []string{p.Name, p.Text, "cost first", "Cost-First"} {
				if strings.Contains(strings.ToLower(seen), strings.ToLower(leak)) {
					t.Errorf("judge reads %q:\n%s", leak, seen)
				}
			}
		}
		if strings.Contains(jd.Prompt, ArenaRoleProposer) {
			t.Errorf("judge prompt names the proposer role")
		}
		if n := len(promptPairs(jd.Prompt)); n != 3 {
			t.Errorf("judge sees %d pairs, want 3", n)
		}
	}
	// Each judge seat draws its own order; two seats may match by chance,
	// nine all matching means there is no shuffle.
	ar := f.host.arenas().arena(a.ID)
	orders := map[string]bool{}
	ar.mu.Lock()
	for j := range maxArenaJudges {
		labels, pairs := ar.judgeOrderLocked(j)
		orders[fmt.Sprint(labels, pairs)] = true
	}
	ar.mu.Unlock()
	if len(orders) < 2 {
		t.Error("every judge seat gets the same order")
	}
	byLabel := liveLabels(f, a.ID)
	labels := make([]string, 3)
	for l, i := range byLabel {
		labels[i] = l
	}
	if slices.Equal(labels, []string{"A", "B", "C"}) {
		t.Errorf("labels %v follow entrant order", labels)
	}

	// While judging, nothing a judge could reach ties a label to a lens:
	// not its prompt, not arena.json, not the API, not the proposals folder.
	for _, jd := range judges {
		for _, leak := range []string{f.host.Layout.Home, "arenas", a.ID} {
			if strings.Contains(jd.Prompt, leak) {
				t.Errorf("judge prompt names %q", leak)
			}
		}
	}
	raw, _ := os.ReadFile(filepath.Join(a.Dir, "arena.json"))
	if strings.Contains(string(raw), `"label"`) || strings.Contains(string(raw), `"words"`) {
		t.Errorf("arena.json maps labels while judging:\n%s", raw)
	}
	if got, _ := f.host.Arena(a.ID); slices.ContainsFunc(got.Entrants, func(e Entrant) bool { return e.Label != "" || e.Proposal != "" }) {
		t.Errorf("the API shows labels while judging: %+v", got.Entrants)
	}
	if files, _ := os.ReadDir(filepath.Join(a.Dir, "proposals")); len(files) != 0 {
		t.Errorf("%d proposal files on disk while judging", len(files))
	}

	for _, jd := range judges {
		judgeBy(t, f, a.ID, jd, prefOrder(0, 1, 2))
	}
	got := waitArenaState(t, f, a.ID, ArenaDone)
	for _, e := range got.Entrants {
		if byLabel[e.Label] != e.Index || e.Words == 0 {
			t.Errorf("entrant after judging %+v", e)
		}
		if p, err := os.ReadFile(e.Proposal); err != nil || !strings.Contains(string(p), e.Prior) {
			t.Errorf("proposal of %s: %q %v", e.Prior, p, err)
		}
	}
}

func TestArenaForfeitsASilentProposer(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 1, NoEvolve: true, Timeout: "1500ms"})
	if err != nil {
		t.Fatal(err)
	}
	proposeAll(t, f, a.ID, 3, 1)
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	got, _ := f.host.Arena(a.ID)
	if e := got.Entrants[1]; e.State != seatForfeit || e.Label != "" || !strings.Contains(e.Note, "timed out") {
		t.Fatalf("silent entrant %+v", e)
	}
	if n := len(promptPairs(jd.Prompt)); n != 1 {
		t.Fatalf("judge sees %d pairs, want 1", n)
	}
	judgeBy(t, f, a.ID, jd, prefOrder(0, 2))
	got = waitArenaState(t, f, a.ID, ArenaDone)
	if len(got.Standings) != 2 || got.Standings[0].Prior != "cost-first" {
		t.Fatalf("standings %+v", got.Standings)
	}
	if ag, _ := f.store.Agent(got.Entrants[1].AgentID); ag.State != store.StateKilled {
		t.Errorf("silent proposer is %s, want killed", ag.State)
	}
	report, _ := os.ReadFile(got.Report)
	if !strings.Contains(string(report), "simplicity-first: forfeit") {
		t.Errorf("arena.md does not list the forfeit:\n%s", report)
	}
}

func TestArenaRefusesBadJudgments(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors[:2], Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	props := waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 2)
	if msg, isErr := callArenaTool(t, f, props[0].ID, "judgment", map[string]any{"a": "A", "b": "B", "winner": "A", "margin": 1, "why": "x"}); !isErr {
		t.Fatalf("a proposer could judge: %s", msg)
	}
	proposeAll(t, f, a.ID, 2)
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	judge := func(args map[string]any) (string, bool) { return callArenaTool(t, f, jd.ID, "judgment", args) }
	for name, args := range map[string]map[string]any{
		"unknown pair": {"a": "A", "b": "Z", "winner": "A", "margin": 1, "why": "x"},
		"same entry":   {"a": "A", "b": "A", "winner": "A", "margin": 1, "why": "x"},
		"bad winner":   {"a": "A", "b": "B", "winner": "C", "margin": 1, "why": "x"},
		"bad margin":   {"a": "A", "b": "B", "winner": "A", "margin": 4, "why": "x"},
		"empty why":    {"a": "A", "b": "B", "winner": "A", "margin": 1, "why": " "},
	} {
		if msg, isErr := judge(args); !isErr {
			t.Errorf("%s accepted: %s", name, msg)
		}
	}
	if msg, isErr := judge(map[string]any{"a": "b", "b": "a", "winner": "a", "margin": 2, "why": "x"}); isErr {
		t.Fatalf("good judgment refused: %s", msg)
	}
	if msg, isErr := judge(map[string]any{"a": "A", "b": "B", "winner": "B", "margin": 2, "why": "x"}); !isErr || !strings.Contains(msg, "already judged") {
		t.Fatalf("duplicate: %s", msg)
	}
	f.done(t, jd.ID, "judged")
	got := waitArenaState(t, f, a.ID, ArenaDone)
	if len(got.Judgments) != 1 {
		t.Fatalf("judgments %+v", got.Judgments)
	}
}

func TestArenaKillRunStopsEveryPhase(t *testing.T) {
	f := newArenaFixture(t, 1)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 2})
	if err != nil {
		t.Fatal(err)
	}
	first := waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 1)[0]
	if _, err := f.host.KillRun(a.ID); err != nil {
		t.Fatal(err)
	}
	got := waitArenaState(t, f, a.ID, ArenaKilled)
	time.Sleep(100 * time.Millisecond)
	all, _ := f.store.Agents()
	var mine []store.Agent
	for _, ag := range all {
		if ag.RunID == a.ID {
			mine = append(mine, ag)
		}
	}
	if len(mine) != 1 || mine[0].ID != first.ID || mine[0].State != store.StateKilled {
		t.Fatalf("agents of the killed arena: %+v", mine)
	}
	if run := runState(t, f.host, a.ID); run.State != store.StateKilled {
		t.Fatalf("run is %s", run.State)
	}
	for _, e := range got.Entrants {
		if e.State != seatKilled {
			t.Errorf("entrant %+v", e)
		}
	}
	if _, err := os.Stat(got.Report); err != nil {
		t.Errorf("no arena.md after a kill: %v", err)
	}
}

func TestArenaPersonasAndHumanFinal(t *testing.T) {
	f := newArenaFixture(t, 10)
	src, err := os.ReadFile("../../examples/arena/personas.yaml")
	if err != nil {
		t.Fatal(err)
	}
	personas, rubric, err := ParsePersonas(src)
	if err != nil || len(personas) != 3 || len(rubric) != 2 {
		t.Fatalf("example personas: %d %v %v", len(personas), rubric, err)
	}
	body, _ := json.Marshal(ArenaRequest{Question: "pitch?", Priors: testPriors[:2], Final: FinalHuman, Personas: personas, Rubric: rubric})
	resp, err := http.Post(f.srv.URL+"/api/arenas", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var a Arena
	json.NewDecoder(resp.Body).Decode(&a)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || len(a.Judges) != 3 || a.Judges[1].Persona != "CEO / leadership" || a.Final != FinalHuman {
		t.Fatalf("start: %s %+v", resp.Status, a)
	}
	if run := runState(t, f.host, a.ID); slices.ContainsFunc(run.Steps, func(s store.Step) bool { return s.Name == "evolve" }) {
		t.Error("a human-final arena has an evolve step")
	}

	// 300 words: past Product's 250-word budget, inside the others'.
	long := strings.Repeat("word ", 299) + "TAILWORD"
	for _, p := range waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 2) {
		f.done(t, p.ID, long)
	}
	judges := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 3)
	byPersona := map[string]string{}
	got, _ := f.host.Arena(a.ID)
	for _, jd := range got.Judges {
		i := slices.IndexFunc(judges, func(x store.Agent) bool { return x.ID == jd.AgentID })
		byPersona[jd.Persona] = reads(t, judges[i])
	}
	product, ceo := byPersona["Product (non-technical)"], byPersona["CEO / leadership"]
	for _, want := range []string{"exactly one question addressed to Product", "Reading budget: 250 words", "the reader stopped here",
		"Tie break: shorter wins", "judge fit to your reader"} {
		if !strings.Contains(product, want) {
			t.Errorf("product judge prompt lacks %q", want)
		}
	}
	if strings.Contains(product, "TAILWORD") {
		t.Error("product judge read past its budget")
	}
	if !strings.Contains(ceo, "TAILWORD") || strings.Contains(ceo, "the reader stopped here") {
		t.Error("CEO judge did not read the whole entry")
	}
	for _, jd := range judges {
		judgeBy(t, f, a.ID, jd, prefOrder(1, 0))
	}
	waitArenaState(t, f, a.ID, ArenaDone)
	if n := len(arenaAgents(t, f, a.ID, ArenaRoleEvolver)); n != 0 {
		t.Fatalf("%d evolvers ran", n)
	}

	resp, err = http.Get(f.srv.URL + "/api/arenas/" + a.ID)
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	report, _ := os.ReadFile(got.Report)
	for _, want := range []string{"## your pick", "1. **" + got.Standings[0].Label + "** (simplicity-first)", "pick: read both", "judge 2 (CEO / leadership)"} {
		if !strings.Contains(string(report), want) {
			t.Errorf("arena.md lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(string(report), "## synthesis") {
		t.Error("a human-final arena.md has a synthesis section")
	}
}

func TestParsePersonasAndPriors(t *testing.T) {
	ps, rubric, err := ParsePersonas([]byte("- id: x\n  briefing: you read things\n"))
	if err != nil || len(ps) != 1 || ps[0].Name() != "x" || rubric != nil {
		t.Fatalf("list form: %+v %v %v", ps, rubric, err)
	}
	for _, bad := range []string{"", "personas: []", "- role: r\n", "- id: x\n  briefing: b\n  reading_budget_words: -1\n"} {
		if _, _, err := ParsePersonas([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	pr, err := ParsePriors([]byte("priors:\n  - name: a\n    text: t\n"))
	if err != nil || len(pr) != 1 || pr[0].Text != "t" {
		t.Fatalf("priors: %+v %v", pr, err)
	}
}

func TestCutWordsKeepsLayout(t *testing.T) {
	if got := cutWords("# one\n\ntwo  three\nfour", 3, "cut"); got != "# one\n\ntwo\n\n[cut]" {
		t.Fatalf("%q", got)
	}
	if got := cutWords("a b", 2, "cut"); got != "a b" {
		t.Fatalf("%q", got)
	}
	if got := cutWords("a b", 0, "cut"); got != "a b" {
		t.Fatalf("%q", got)
	}
}

func TestStartArenaRefuses(t *testing.T) {
	f := newArenaFixture(t, 10)
	for name, req := range map[string]ArenaRequest{
		"no question":     {},
		"one entrant":     {Question: "q", Entrants: 1},
		"too many":        {Question: "q", Entrants: 5},
		"relative ctx":    {Question: "q", Context: []string{"reports"}},
		"missing ctx":     {Question: "q", Context: []string{"/nonexistent/arena/ctx"}},
		"bad final":       {Question: "q", Final: "robot"},
		"bad timeout":     {Question: "q", Timeout: "soon"},
		"empty persona":   {Question: "q", Personas: []Persona{{ID: "x"}}},
		"duplicate prior": {Question: "q", Priors: []Prior{{Name: "a", Text: "x"}, {Name: "a", Text: "y"}}},
		"past the cap":    {Question: "q", Priors: manyPriors(maxArenaEntrants + 1), Entrants: maxArenaEntrants + 1},
	} {
		if _, err := f.host.StartArena(req); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	if as, _ := f.host.Arenas(); len(as) != 0 {
		t.Errorf("refused arenas left %d on disk", len(as))
	}
}

func TestExamplePriorsAreTheDefaults(t *testing.T) {
	b, err := os.ReadFile("../../examples/arena/priors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := ParsePriors(b)
	if err != nil {
		t.Fatal(err)
	}
	for i := range ps {
		ps[i].Text = strings.TrimSpace(ps[i].Text)
	}
	if !slices.Equal(ps, DefaultPriors) {
		t.Fatalf("examples/arena/priors.yaml drifted from DefaultPriors:\n%+v", ps)
	}
}

// The prompt goes through tmux's command line, which refuses about 16KB, so
// what agents read has to go in files even with four entries at the cap.
func TestArenaPromptsStaySmall(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Judges: 1})
	if err != nil {
		t.Fatal(err)
	}
	huge := strings.Repeat("a fairly ordinary sentence of ten words, give or take. ", 300)
	for _, p := range waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 4) {
		f.done(t, p.ID, huge)
	}
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	if n := len(jd.Prompt); n > 8<<10 {
		t.Errorf("judge prompt is %d bytes", n)
	}
	if !strings.Contains(reads(t, jd), "cut by the arena host at 1350 words") {
		t.Error("entries were not cut at the cap")
	}
	judgeBy(t, f, a.ID, jd, prefOrder(0, 1, 2, 3))
	ev := waitArenaAgents(t, f, a.ID, ArenaRoleEvolver, 1)[0]
	if n := len(ev.Prompt); n > 8<<10 {
		t.Errorf("evolver prompt is %d bytes", n)
	}
	f.done(t, ev.ID, "merged")
	waitArenaState(t, f, a.ID, ArenaDone)
}

func TestArenaTakesARunSlot(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors[:2], Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.StartArena(ArenaRequest{Question: "again?", Priors: testPriors[:2], Judges: 1}); !errors.Is(err, ErrNoRunSlot) {
		t.Fatalf("second arena under max_runs 1: %v", err)
	}
	body, _ := json.Marshal(ArenaRequest{Question: "http?", Priors: testPriors[:2]})
	resp, err := http.Post(f.srv.URL+"/api/arenas", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("http start at the cap: %s", resp.Status)
	}
	if as, _ := f.host.Arenas(); len(as) != 1 {
		t.Fatalf("%d arenas on disk, want 1", len(as))
	}
	if _, err := f.host.KillRun(a.ID); err != nil {
		t.Fatal(err)
	}
	waitArenaState(t, f, a.ID, ArenaKilled)
	next, err := f.host.StartArena(ArenaRequest{Question: "now?", Priors: testPriors[:2], Judges: 1})
	if err != nil {
		t.Fatalf("arena after the slot freed: %v", err)
	}
	killArena(t, f, next.ID)
}

// A host that restarts mid-arena has no goroutine left to finish it; the
// next start fails the run, kills its agents and writes the report.
func TestCloseOrphanArenas(t *testing.T) {
	f := newArenaFixture(t, 10)
	id := "orphan-arena"
	dir := filepath.Join(f.host.arenasDir(), id)
	os.MkdirAll(dir, 0o755)
	now := time.Now()
	if err := f.store.CreateRun(store.Run{ID: id, Goal: "q?", Source: SourceArena, State: store.StateBusy, CreatedAt: now,
		Steps: []store.Step{{Name: "propose-1"}, {Name: "propose-2"}}}); err != nil {
		t.Fatal(err)
	}
	for _, ag := range []store.Agent{
		{ID: "p1", TmuxSession: "aeolian-p1", ParentID: "arena:" + id, Role: ArenaRoleProposer, RunID: id, Headless: true, State: store.StateBusy},
		{ID: "p2", TmuxSession: "aeolian-p2", ParentID: "arena:" + id, Role: ArenaRoleProposer, RunID: id, Headless: true, State: store.StateBusy},
	} {
		if err := f.store.CreateAgent(ag); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Done("p1", store.Handoff{Text: "the first proposal"}, store.ReasonDelivered, now); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Arena{ID: id, Question: "q?", State: ArenaProposing, Final: FinalEvolver, Dir: dir, CreatedAt: now,
		Entrants: []Entrant{{Index: 0, Prior: "cost-first", AgentID: "p1", State: seatIn}, {Index: 1, Prior: "frontier-first", AgentID: "p2", State: seatBusy}}})
	os.WriteFile(filepath.Join(dir, "arena.json"), b, 0o644)

	f.host.CloseOrphanArenas()

	if run := runState(t, f.host, id); run.State != store.StateFailed {
		t.Fatalf("orphan run is %s", run.State)
	}
	if ag, _ := f.store.Agent("p2"); ag.State != store.StateKilled {
		t.Errorf("orphan's live agent is %s", ag.State)
	}
	got, err := f.host.Arena(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ArenaFailed || got.DoneAt == nil || !strings.Contains(got.Note, "host restarted") {
		t.Fatalf("orphan arena %+v", got)
	}
	if e := got.Entrants[1]; e.State != seatKilled {
		t.Errorf("busy entrant %+v", e)
	}
	if p, _ := os.ReadFile(got.Entrants[0].Proposal); strings.TrimSpace(string(p)) != "the first proposal" {
		t.Errorf("proposal %q", p)
	}
	if report, _ := os.ReadFile(got.Report); !strings.Contains(string(report), "host restarted") {
		t.Errorf("arena.md:\n%s", report)
	}
	// The slot is free again.
	next, err := f.host.StartArena(ArenaRequest{Question: "next?", Priors: testPriors[:2], Judges: 1})
	if err != nil {
		t.Fatal(err)
	}
	killArena(t, f, next.ID)
}

func TestArenaAgentsCannotSpawn(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors[:2], Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	props := waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 2)
	if names := toolNames(t, f.connect(t, props[0].ID)); slices.Contains(names, "spawn") || slices.Contains(names, "await") || !slices.Contains(names, "done") {
		t.Fatalf("proposer tools %v", names)
	}
	if _, _, err := f.host.spawn(context.Background(), nil, props[1].ID, SpawnInput{Role: ArenaRoleJudge}); err == nil {
		t.Fatal("an arena agent spawned a helper")
	}
	proposeAll(t, f, a.ID, 2)
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	if names := toolNames(t, f.connect(t, jd.ID)); slices.Contains(names, "spawn") || !slices.Contains(names, "judgment") {
		t.Fatalf("judge tools %v", names)
	}
	if names := toolNames(t, f.connect(t, f.start(t).ID)); !slices.Contains(names, "spawn") {
		t.Fatalf("an ordinary agent lost spawn: %v", names)
	}
}

func manyPriors(n int) []Prior {
	var out []Prior
	for i := range n {
		out = append(out, Prior{Name: fmt.Sprintf("p%d", i), Text: "lens"})
	}
	return out
}

func TestArenaEntrantCapMessage(t *testing.T) {
	f := newArenaFixture(t, 10)
	_, err := f.host.StartArena(ArenaRequest{Question: "q", Priors: manyPriors(30), Entrants: 30})
	if err == nil || !strings.Contains(err.Error(), "at most 8") {
		t.Fatalf("30 entrants: %v", err)
	}
	a, err := f.host.StartArena(ArenaRequest{Question: "q", Priors: manyPriors(maxArenaEntrants), Entrants: maxArenaEntrants, Judges: 1})
	if err != nil {
		t.Fatalf("%d entrants: %v", maxArenaEntrants, err)
	}
	waitArenaAgents(t, f, a.ID, ArenaRoleProposer, maxArenaEntrants)
	killArena(t, f, a.ID)
}

// killArena kills a test's arena and waits for it to stop writing, so the
// test's temp folder can go.
func killArena(t *testing.T, f *fixture, id string) {
	t.Helper()
	if _, err := f.host.KillRun(id); err != nil {
		t.Fatal(err)
	}
	waitArenaState(t, f, id, ArenaKilled)
}

// cycleArena is an arena with three entrants in, labelled, whose judges
// went A>B, B>C, C>A by entrant, each by margin 1: a perfect tie.
func cycleArena(t *testing.T, id string) *arenaRun {
	t.Helper()
	ar := &arenaRun{priors: testPriors, raw: map[int]string{}, proposals: map[int]string{},
		byLabel: map[string]int{}, pairs: map[[2]string]bool{}, judged: map[int]map[[2]string]bool{}}
	ar.a = Arena{ID: id, Dir: t.TempDir(), Final: FinalHuman}
	for i, p := range testPriors {
		ar.a.Entrants = append(ar.a.Entrants, Entrant{Index: i, Prior: p.Name, State: seatIn})
	}
	ar.assignLabels([]int{0, 1, 2})
	label := func(i int) string { return ar.a.Entrants[i].Label }
	for _, g := range [][2]int{{0, 1}, {1, 2}, {2, 0}} {
		w, l := label(g[0]), label(g[1])
		ar.a.Judgments = append(ar.a.Judgments, Judgment{A: w, B: l, Winner: w, Margin: 1, Why: "x"})
	}
	ar.mu.Lock()
	ar.rankLocked()
	ar.mu.Unlock()
	return ar
}

func TestArenaTieBreakIsSeededNotPriorOrder(t *testing.T) {
	winners := map[string]int{}
	for k := range 40 {
		ar := cycleArena(t, fmt.Sprintf("arena-%d", k))
		st := ar.a.Standings
		if len(st) != 3 || st[0].Tie || !st[1].Tie || !st[2].Tie {
			t.Fatalf("a perfect cycle is not reported as a tie: %+v", st)
		}
		winners[st[0].Prior]++
		if again := cycleArena(t, ar.a.ID).a.Standings; !slices.Equal(again, st) {
			t.Fatalf("same arena id ranked differently: %+v vs %+v", again, st)
		}
		if k == 0 {
			ar.mu.Lock()
			report := ar.reportLocked()
			ar.mu.Unlock()
			if !strings.Contains(report, "tied: ") {
				t.Errorf("arena.md does not say the entries tied:\n%s", report)
			}
		}
	}
	if len(winners) < 2 {
		t.Fatalf("one prior wins every tie: %v", winners)
	}
}

// A judge that forfeits can no longer judge, and nobody can once judging
// has ended; what a forfeited judge judged before still counts, flagged.
func TestArenaRefusesJudgmentsFromForfeitedJudgesAndAfterJudging(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 2, NoEvolve: true, Timeout: "1500ms"})
	if err != nil {
		t.Fatal(err)
	}
	proposeAll(t, f, a.ID, 3)
	judges := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 2)
	slow := judges[1]
	pairs := promptPairs(slow.Prompt)
	judgeOne := func(id string, p [2]string) (string, bool) {
		return callArenaTool(t, f, id, "judgment", map[string]any{"a": p[0], "b": p[1], "winner": p[0], "margin": 1, "why": "x"})
	}
	if msg, isErr := judgeOne(slow.ID, pairs[0]); isErr {
		t.Fatalf("slow judge's first judgment: %s", msg)
	}
	judgeBy(t, f, a.ID, judges[0], prefOrder(0, 1, 2))

	waitFor(t, "slow judge forfeits", func() bool {
		got, _ := f.host.Arena(a.ID)
		return slices.ContainsFunc(got.Judges, func(j ArenaJudge) bool { return j.AgentID == slow.ID && j.State == seatForfeit })
	})
	if msg, isErr := judgeOne(slow.ID, pairs[1]); !isErr {
		t.Fatalf("a forfeited judge's judgment was recorded: %s", msg)
	}
	got := waitArenaState(t, f, a.ID, ArenaDone)
	if len(got.Judgments) != 4 {
		t.Fatalf("%d judgments, want 3 from the finished judge and 1 from the forfeited one", len(got.Judgments))
	}
	if msg, isErr := judgeOne(judges[0].ID, pairs[1]); !isErr || !strings.Contains(msg, "judging has ended") {
		t.Fatalf("judgment after judging ended: %s", msg)
	}
	report, _ := os.ReadFile(got.Report)
	if !strings.Contains(string(report), "1 of these 4 judgments came from judges who later forfeited") {
		t.Errorf("arena.md does not flag the forfeited judge's judgments:\n%s", report)
	}
}

func TestStartArenaRefusesAQuestionTooLongForAPrompt(t *testing.T) {
	f := newArenaFixture(t, 10)
	_, err := f.host.StartArena(ArenaRequest{Question: strings.Repeat("why? ", 3000), Priors: testPriors[:2]})
	if err == nil || !strings.Contains(err.Error(), "put the long part in a file") {
		t.Fatalf("20KB question: %v", err)
	}
	long := Persona{ID: "x", Briefing: strings.Repeat("b", arenaInlineCap)}
	if _, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors[:2], Personas: []Persona{long}}); err == nil {
		t.Fatal("an 8KB persona briefing was accepted")
	}
}

func TestArenaHostForgetsAFinishedArena(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors[:2], Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	proposeAll(t, f, a.ID, 2)
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	judgeBy(t, f, a.ID, jd, prefOrder(0, 1))
	waitArenaState(t, f, a.ID, ArenaDone)
	book := f.host.arenas()
	waitFor(t, "the book forgets the arena", func() bool {
		book.mu.Lock()
		defer book.mu.Unlock()
		return book.arenas[a.ID] == nil && book.judges[jd.ID] == nil
	})
	if got, err := f.host.Arena(a.ID); err != nil || got.State != ArenaDone || len(got.Standings) != 2 {
		t.Fatalf("arena from disk: %+v %v", got, err)
	}
}

// KillRun can land after the last phase checked for it and before the
// arena closed its run; the arena then ends killed, as its run did.
func TestArenaStopAfterKillRunEndsKilled(t *testing.T) {
	f := newArenaFixture(t, 10)
	id := "raced-arena"
	dir := filepath.Join(f.host.arenasDir(), id)
	os.MkdirAll(filepath.Join(dir, "proposals"), 0o755)
	if err := f.store.CreateRun(store.Run{ID: id, Goal: "q?", Source: SourceArena, State: store.StateBusy, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.host.KillRun(id); err != nil {
		t.Fatal(err)
	}
	ar := &arenaRun{h: f.host, raw: map[int]string{}, byLabel: map[string]int{}, revealed: true}
	ar.a = Arena{ID: id, Question: "q?", Dir: dir, State: ArenaEvolving, Final: FinalEvolver}
	ar.stop(ArenaDone, store.ReasonDelivered, "")
	got, err := f.host.Arena(id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ArenaKilled {
		t.Fatalf("arena is %s on a killed run", got.State)
	}
	if report, _ := os.ReadFile(got.Report); !strings.Contains(string(report), "state: **killed**") {
		t.Errorf("arena.md:\n%s", report)
	}
}

// TestArenaStalledProposerForfeits: the watchdog stops an arena agent that
// went quiet like any helper; its handoff reaches the arena's wait, not a
// parent agent, and the seat forfeits with the agent's reason on its step.
func TestArenaStalledProposerForfeits(t *testing.T) {
	f := newArenaFixture(t, 10)
	f.host.StallAfter = 50 * time.Millisecond
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 3)
	got, _ := f.host.Arena(a.ID)
	silent := got.Entrants[1].AgentID
	f.waitStarted(t, silent)
	f.hook(t, silent, "UserPromptSubmit", struct{}{})
	proposeAll(t, f, a.ID, 3, 1)
	time.Sleep(80 * time.Millisecond)
	f.host.Sweep()
	if ag := f.agent(t, silent); ag.State != store.StateKilled || ag.Reason != store.ReasonLost {
		t.Fatalf("stalled proposer %s/%s", ag.State, ag.Reason)
	}
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	got, _ = f.host.Arena(a.ID)
	if e := got.Entrants[1]; e.State != seatForfeit || !strings.Contains(e.Note, "ended without calling done") {
		t.Fatalf("stalled entrant %+v", e)
	}
	run, _ := f.store.Run(a.ID)
	if st := run.Steps[1]; st.State != store.StateFailed || st.Reason != store.ReasonLost {
		t.Fatalf("stalled step %s/%s", st.State, st.Reason)
	}
	judgeBy(t, f, a.ID, jd, prefOrder(0, 2))
	waitArenaState(t, f, a.ID, ArenaDone)
	f.waitRun(t, a.ID, store.StateDone, store.ReasonDelivered)
	for _, kind := range []string{store.KindArenaStarted, store.KindArenaPhase, store.KindArenaJudgment, store.KindArenaFinished} {
		if evs := f.eventsOf(t, kind); len(evs) == 0 || evs[0].RunID != a.ID {
			t.Errorf("no %s event for the arena: %+v", kind, evs)
		}
	}
}

// TestResumeDoneLeavesArenasAlone: an arena step agent that delivered
// while its step is still busy (the host stopped in between) is not a
// lead, so ResumeDone must not advance the arena's run past it.
func TestResumeDoneLeavesArenasAlone(t *testing.T) {
	f := newArenaFixture(t, 10)
	src, steps, err := arenaWorkflow(ArenaRequest{Question: "q?", Judges: 1, NoEvolve: true}, testPriors[:2])
	if err != nil {
		t.Fatal(err)
	}
	run := store.Run{ID: "arena-r", Goal: "q?", Workflow: src, Source: SourceArena, State: store.StateBusy}
	for _, name := range steps {
		run.Steps = append(run.Steps, store.Step{Name: name})
	}
	if err := f.store.CreateRun(run); err != nil {
		t.Fatal(err)
	}
	p := store.Agent{ID: "p1", TmuxSession: "aeolian-p1", ParentID: "arena:arena-r", Role: ArenaRoleProposer, RunID: "arena-r", Step: steps[0], Headless: true, State: store.StateBusy}
	if err := f.store.CreateAgent(p); err != nil {
		t.Fatal(err)
	}
	if err := f.store.StartStep("arena-r", 0, "p1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Done("p1", store.Handoff{Text: "a proposal"}, store.ReasonDelivered, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.host.ResumeDone()
	time.Sleep(150 * time.Millisecond)
	got, _ := f.store.Run("arena-r")
	if got.Steps[0].State != store.StateBusy || got.Steps[1].State != store.StatePending {
		t.Fatalf("ResumeDone advanced an arena: %+v", got.Steps)
	}
}
