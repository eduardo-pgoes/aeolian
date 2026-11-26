package precedent

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(DateLayout, s)
	if err != nil {
		panic(err)
	}
	return t.Add(12 * time.Hour)
}

func TestLoadFixtures(t *testing.T) {
	ps, errs := Load("testdata/precedents")
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if len(ps) != 3 {
		t.Fatalf("loaded %d", len(ps))
	}
	p := ps[0]
	if p.ID != "p01-dev-ledger-project-branch" || p.Kind != KindCorrection || strings.Join(p.Scope, ",") != "dev-ledger,branch" ||
		p.ReviewBy.Format(DateLayout) != "2027-01-09" || p.Confidence != ConfidenceStated || !strings.HasPrefix(p.Body, "`dev-ledger` in atlas") ||
		!strings.HasPrefix(p.Source, "fixture:") {
		t.Fatalf("p01 = %+v", p)
	}
}

func TestMatchIsANDAndFlagsStale(t *testing.T) {
	ps, _ := Load("testdata/precedents")
	now := day("2026-11-09")

	m := Match(ps, now, "researcher", "is dev-ledger drift?", "look at the branches in atlas")
	if len(m.Fresh) != 1 || m.Fresh[0].ID != "p01-dev-ledger-project-branch" || len(m.Stale) != 0 {
		t.Fatalf("dev-ledger branches: %+v", m)
	}
	// One term of two is not enough.
	if m := Match(ps, now, "explain the branch protection rules"); len(m.Fresh)+len(m.Stale) != 0 {
		t.Fatalf("branch alone matched: %+v", m)
	}
	// p03's review_by (2026-11-01) is past: it matches, but stale.
	m = Match(ps, now, "map the providers; is Faxbridge live?")
	if len(m.Fresh) != 0 || len(m.Stale) != 1 || m.Stale[0].ID != "p03-faxbridge-not-used" {
		t.Fatalf("faxbridge: %+v", m)
	}
	// On its review_by day it is still fresh.
	if m := Match(ps, day("2026-11-01"), "faxbridge"); len(m.Fresh) != 1 {
		t.Fatalf("faxbridge on its review day: %+v", m)
	}
}

func TestWordMatching(t *testing.T) {
	p := Precedent{Scope: []string{"prod"}}
	for text, want := range map[string]bool{
		"query prod":           true,
		"PROD.":                true,
		"prods":                true,
		"the product team":     false,
		"preprod":              false,
		"prod-like env":        true,
		"x_prod":               false,
		"":                     false,
		"pr od":                false,
		"producto, prod, prod": true,
	} {
		if got := p.Matches(text); got != want {
			t.Errorf("%q: %v, want %v", text, got, want)
		}
	}
	hy := Precedent{Scope: []string{"dev-ledger"}}
	if !hy.Matches("Branch Dev-Ledger is ahead") || hy.Matches("dev-ledgerx") {
		t.Error("hyphenated term")
	}
	if (Precedent{}).Matches("anything") {
		t.Error("an empty scope matched")
	}
}

func TestParseRefusesBadFiles(t *testing.T) {
	good := "---\nid: x\nkind: safety\nscope: [aws]\nreview_by: 2027-01-01\nsource: me\n---\nNever print secrets.\n"
	if _, err := Parse("dir/x.md", []byte(good)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse("x.md", []byte(strings.ReplaceAll(good, "\n", "\r\n"))); err != nil {
		t.Fatalf("crlf: %v", err)
	}
	for name, tc := range map[string]struct{ src, want string }{
		"no frontmatter": {"just text", "no frontmatter"},
		"unclosed":       {"---\nid: x\n", "not closed"},
		"bad yaml":       {"---\nid: [\n---\nbody", "frontmatter"},
		"id mismatch":    {strings.Replace(good, "id: x", "id: y", 1), "file name"},
		"no id":          {strings.Replace(good, "id: x\n", "", 1), "no id"},
		"bad kind":       {strings.Replace(good, "safety", "vibes", 1), "kind"},
		"no scope":       {strings.Replace(good, "scope: [aws]\n", "", 1), "no scope"},
		"empty term":     {strings.Replace(good, "[aws]", "[aws, \" \"]", 1), "empty scope term"},
		"no review_by":   {strings.Replace(good, "review_by: 2027-01-01\n", "", 1), "no review_by"},
		"bad review_by":  {strings.Replace(good, "2027-01-01", "soon", 1), "review_by"},
		"no source":      {strings.Replace(good, "source: me\n", "", 1), "no source"},
		"no body":        {strings.Replace(good, "Never print secrets.\n", "", 1), "no body"},
		"bad confidence": {strings.Replace(good, "source: me", "source: me\nconfidence: vibes", 1), "confidence"},
	} {
		_, err := Parse("x.md", []byte(tc.src))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	// Extra keys, as an intent card might carry, are fine.
	if _, err := Parse("x.md", []byte(strings.Replace(good, "source: me", "source: me\nrfc: bot-routing", 1))); err != nil {
		t.Errorf("extra key: %v", err)
	}
}

func TestLoadKeepsGoodFilesBesideBadOnes(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "---\nid: a\nkind: convention\nscope: [rfc]\nreview_by: 2027-01-01\nsource: me\n---\nShort.\n")
	write("b.md", "no frontmatter")
	write("README.md", "# what this folder is")
	write("notes.txt", "ignored")
	ps, errs := Load(dir)
	if len(ps) != 1 || ps[0].ID != "a" || len(errs) != 1 || !strings.Contains(errs[0].Error(), "b.md") {
		t.Fatalf("ps = %+v, errs = %v", ps, errs)
	}
	if ps, errs := Load(filepath.Join(dir, "missing")); ps != nil || errs != nil {
		t.Fatalf("missing dir: %v %v", ps, errs)
	}
}

func TestFormat(t *testing.T) {
	if Format(nil, 5) != "" {
		t.Fatal("empty format")
	}
	ps, _ := Load("testdata/precedents")
	ps[1].Confidence = ConfidenceInferred
	out := Format(ps, 2)
	if !strings.HasPrefix(out, Heading) || !strings.Contains(out, "### p01-dev-ledger-project-branch (correction)") ||
		!strings.Contains(out, "p02-irc") || strings.Contains(out, "p03") || strings.Count(out, "inferred") != 1 {
		t.Fatalf("format:\n%s", out)
	}
	if Dir("/h") != "/h/precedents" {
		t.Fatal(Dir("/h"))
	}
}

func TestParseWildcardRolesRepos(t *testing.T) {
	base := "---\nid: x\nkind: safety\nscope: SCOPE\nEXTRAreview_by: 2027-01-01\nsource: me\n---\nNever push to main.\n"
	src := func(scope, extra string) []byte {
		return []byte(strings.NewReplacer("SCOPE", scope, "EXTRA", extra).Replace(base))
	}
	for name, tc := range map[string]struct {
		scope, extra string
		global       bool
		roles, repos []string
		specificity  int
	}{
		"global":          {scope: `["*"]`, global: true},
		"single quoted":   {scope: `['*']`, global: true},
		"role only":       {scope: `["*"]`, extra: "roles: [Lead, qa-shard, lead]\n", roles: []string{"lead", "qa-shard"}, specificity: 1},
		"repo only":       {scope: `["*"]`, extra: "repos: [aeolian]\n", repos: []string{"aeolian"}, specificity: 1},
		"words role repo": {scope: "[migration, schema]", extra: "roles: [builder]\nrepos: [atlas]\n", roles: []string{"builder"}, repos: []string{"atlas"}, specificity: 4},
		"words only":      {scope: "[migration]", specificity: 1},
	} {
		p, err := Parse("x.md", src(tc.scope, tc.extra))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.Global() != tc.global || !slices.Equal(p.Roles, tc.roles) || !slices.Equal(p.Repos, tc.repos) || p.Specificity() != tc.specificity {
			t.Errorf("%s: global %v roles %q repos %q specificity %d", name, p.Global(), p.Roles, p.Repos, p.Specificity())
		}
	}
	for name, tc := range map[string]struct{ scope, extra, want string }{
		"empty scope":     {"[]", "", "no scope"},
		"bare star":       {"[*]", "", `quote the wildcard`},
		"star with words": {`["*", aws]`, "", "must stand alone"},
		"empty role":      {`["*"]`, "roles: [lead, \"\"]\n", "roles"},
		"star role":       {`["*"]`, "roles: [\"*\"]\n", "roles"},
		"empty repo":      {`["*"]`, "repos: [\" \"]\n", "repos"},
	} {
		_, err := Parse("x.md", src(tc.scope, tc.extra))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

func TestReaches(t *testing.T) {
	global := Precedent{Scope: []string{Any}}
	lead := Precedent{Scope: []string{Any}, Roles: []string{"lead"}}
	aeolian := Precedent{Scope: []string{Any}, Repos: []string{"aeolian"}}
	narrow := Precedent{Scope: []string{"migration"}, Roles: []string{"builder", "reviewer"}, Repos: []string{"atlas"}}
	words := Precedent{Scope: []string{"migration"}}
	for name, tc := range map[string]struct {
		p    Precedent
		t    Target
		want bool
	}{
		"global, bare agent":       {global, Target{}, true},
		"role matches":             {lead, Target{Role: "Lead"}, true},
		"role is exact":            {lead, Target{Role: "leader"}, false},
		"role not in text":         {lead, Target{Text: "the lead said"}, false},
		"repo matches":             {aeolian, Target{Repos: []string{"Aeolian"}}, true},
		"no repo":                  {aeolian, Target{Text: "aeolian"}, false},
		"all three":                {narrow, Target{Role: "reviewer", Repos: []string{"atlas"}, Text: "check the migrations"}, true},
		"all three, words missing": {narrow, Target{Role: "reviewer", Repos: []string{"atlas"}, Text: "check the schema"}, false},
		"all three, wrong repo":    {narrow, Target{Role: "builder", Repos: []string{"aeolian"}, Text: "migration"}, false},
		"all three, wrong role":    {narrow, Target{Role: "lead", Repos: []string{"atlas"}, Text: "migration"}, false},
		"words only":               {words, Target{Role: "anyone", Text: "a migration"}, true},
	} {
		if got := tc.p.Reaches(tc.t); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}

func TestMatchLeavesGlobalAndGatedOutOfClaims(t *testing.T) {
	ps := []Precedent{
		{ID: "g", Scope: []string{Any}},
		{ID: "r", Scope: []string{"migration"}, Roles: []string{"builder"}},
		{ID: "w", Scope: []string{"migration"}},
	}
	now := day("2026-11-09")
	for i := range ps {
		ps[i].ReviewBy = day("2027-01-01")
	}
	m := Match(ps, now, "the migration ran")
	if len(m.Fresh) != 1 || m.Fresh[0].ID != "w" {
		t.Fatalf("claim match %+v", m.Fresh)
	}
	m = MatchTarget(ps, now, Target{Role: "builder", Text: "the migration ran"})
	if len(m.Fresh) != 3 {
		t.Fatalf("target match %+v", m.Fresh)
	}
}

func TestSelect(t *testing.T) {
	mk := func(id, kind string, scope []string, roles ...string) Precedent {
		return Precedent{ID: id, Kind: kind, Scope: scope, Roles: roles}
	}
	ids := func(ps []Precedent) string {
		var out []string
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return strings.Join(out, " ")
	}
	wild := []string{Any}
	var targeted []Precedent
	for i := range 7 {
		targeted = append(targeted, mk(fmt.Sprintf("t%d", i), KindCorrection, []string{"a"}))
	}
	globals := []Precedent{mk("g-conv", KindConvention, wild), mk("g-corr", KindCorrection, wild), mk("g-safe", KindSafety, wild), mk("g-zz", KindSafety, wild)}
	for name, tc := range map[string]struct {
		ps   []Precedent
		max  int
		want string
	}{
		"specific first, ties by kind": {
			[]Precedent{mk("w1", KindConvention, []string{"a"}), mk("w2", KindSafety, []string{"a"}), mk("w3", KindCorrection, []string{"a", "b"}), mk("role", KindCorrection, wild, "lead")},
			8, "w3 w2 role w1",
		},
		"globals keep their slots": {append(append([]Precedent{}, targeted...), globals...), 8, "t0 t1 t2 t3 t4 g-safe g-zz g-corr"},
		"globals fill free slots":  {append([]Precedent{targeted[0]}, globals...), 8, "t0 g-safe g-zz g-corr g-conv"},
		"targeted fill free slots": {append(append([]Precedent{}, targeted...), globals[0]), 8, "t0 t1 t2 t3 t4 t5 t6 g-conv"},
		"tiny cap":                 {append([]Precedent{targeted[0]}, globals...), 2, "g-safe g-zz"},
		"no cap":                   {append([]Precedent{globals[0]}, targeted[:2]...), 0, "t0 t1 g-conv"},
	} {
		if got := ids(Select(tc.ps, tc.max, 3)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
