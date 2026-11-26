package precedent

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestWriteRoundTrips(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC)
	p, err := Write(dir, Draft{
		Scope:  []string{" Dev-Ledger", "branch", "branch"},
		Source: `run 3f2a: "drift" claim`,
		Body:   "dev-ledger is a PROJECT branch, not drift.\n",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "p20261109-dev-ledger-is-a-project-branch" || p.Kind != KindCorrection {
		t.Fatalf("got %+v", p)
	}
	if !slices.Equal(p.Scope, []string{"dev-ledger", "branch"}) {
		t.Fatalf("scope %q", p.Scope)
	}
	if want := now.Add(DefaultReviewAfter).Format(DateLayout); p.ReviewBy.Format(DateLayout) != want {
		t.Fatalf("review_by %s, want %s", p.ReviewBy.Format(DateLayout), want)
	}
	b, _ := os.ReadFile(p.Path)
	if !strings.Contains(string(b), "scope: [dev-ledger, branch]\n") || !strings.Contains(string(b), "\nreview_by: 2027-01-08\n") {
		t.Fatalf("file:\n%s", b)
	}
	ps, errs := Load(dir)
	if len(errs) != 0 || len(ps) != 1 || ps[0].Body != p.Body || ps[0].Source != p.Source {
		t.Fatalf("load %+v %v", ps, errs)
	}

	again, err := Write(dir, Draft{Scope: []string{"x"}, Source: "s", Body: "dev-ledger is a project branch, again"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != p.ID+"-2" {
		t.Fatalf("second id %q", again.ID)
	}
}

func TestWriteRefusesBadDrafts(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for name, d := range map[string]Draft{
		"no scope":  {Source: "s", Body: "b"},
		"no body":   {Scope: []string{"a"}, Source: "s"},
		"no source": {Scope: []string{"a"}, Body: "b"},
		"bad kind":  {Kind: "vibe", Scope: []string{"a"}, Source: "s", Body: "b"},
		"bad id":    {ID: "Has Spaces", Scope: []string{"a"}, Source: "s", Body: "b"},
	} {
		if _, err := Write(dir, d, now); err == nil {
			t.Errorf("%s: wrote it", name)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %d files", len(entries))
	}
}

func TestSuggestScopePicksNames(t *testing.T) {
	for text, want := range map[string][]string{
		"The dev-ledger branch in Atlas is drift that should be cleaned up": {"dev-ledger", "atlas"},
		"the alerting service is dead and nobody pages on it":               {"alerting", "service"},
		"Grafana, Prometheus and Loki all run on prod-1":                    {"prometheus", "loki", "prod-1"},
	} {
		if got := SuggestScope(text, 3); !slices.Equal(got, want) {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}

func TestWriteRolesReposAndWildcard(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	p, err := Write(dir, Draft{
		ID: "lead-rule", Kind: KindSafety, Scope: []string{Any}, Roles: []string{" Lead", "qa-shard", "lead"}, Repos: []string{"Aeolian"},
		Source: "s", Body: "Never merge.",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p.Path)
	for _, want := range []string{"scope: [\"*\"]\n", "roles: [lead, qa-shard]\n", "repos: [aeolian]\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("file lacks %q:\n%s", want, b)
		}
	}
	if p.Global() || !p.Reaches(Target{Role: "lead", Repos: []string{"aeolian"}}) {
		t.Fatalf("got %+v", p)
	}
	g, err := Write(dir, Draft{ID: "everyone", Scope: []string{Any}, Source: "s", Body: "Be kind."}, now)
	if err != nil || !g.Global() {
		t.Fatalf("global %+v %v", g, err)
	}
	if b, _ := os.ReadFile(g.Path); strings.Contains(string(b), "roles") || strings.Contains(string(b), "repos") {
		t.Fatalf("global file:\n%s", b)
	}
	if _, err := Write(dir, Draft{Scope: []string{Any, "x"}, Source: "s", Body: "b"}, now); err == nil {
		t.Fatal("star with words written")
	}
}
