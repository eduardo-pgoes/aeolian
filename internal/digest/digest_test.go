package digest

import (
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var t0 = time.Date(2026, 11, 9, 8, 0, 0, 0, time.UTC)

func at(h, m int) time.Time { return t0.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute) }

// sample is a morning after: an explore run full of claims, a blocked
// RFC run, a run whose helper died, a stale precedent, and two clean runs.
func sample() Input {
	return Input{
		Since: t0, Now: at(14, 0), Running: 1,
		Runs: []Run{
			{ID: "aaaaaaaa-explore", Goal: "explore observability options for atlas", State: "done", Reason: "delivered", StartedAt: at(1, 0), FinishedAt: at(3, 12), Precedents: []string{"p01-dev-ledger"}},
			{ID: "bbbbbbbb-rfc", Goal: "humanize the o11y RFC", State: "blocked", Reason: "blocked", StartedAt: at(2, 0), FinishedAt: at(2, 42)},
			{ID: "cccccccc-map", Goal: "map the infra of the org", State: "failed", Reason: "exited", StartedAt: at(4, 0), FinishedAt: at(4, 5)},
			{ID: "dddddddd-clean", Goal: "fix the upload bucket bug", State: "done", Reason: "delivered", StartedAt: at(5, 0), FinishedAt: at(5, 30)},
			{ID: "eeeeeeee-clean", Goal: "write docs for the infra map", State: "done", Reason: "delivered", StartedAt: at(0, 10), FinishedAt: at(0, 50)},
		},
		Items: []Item{
			{Kind: KindUnwitnessed, RunID: "aaaaaaaa-explore", Text: "9 claims went unwitnessed", Count: 9},
			{Kind: KindUnverifiable, RunID: "aaaaaaaa-explore", Text: "Loki costs less than Datadog at our volume", Note: "pricing page needs a login", Evidence: []string{"https://grafana.com/pricing"}},
			{Kind: KindContradicted, RunID: "aaaaaaaa-explore", Text: "dev-ledger is a drifted branch to clean up", Note: "it is a project branch with open PRs", Evidence: []string{"git -C atlas branch -a", "PR #412"}, Action: `aeolian precedent from-claim aaaaaaaa-explore-0 "…"`},
			{Kind: KindUnverifiable, RunID: "aaaaaaaa-explore", Text: "Prometheus retention is 15 days", Note: "no config in the repo"},
			{Kind: KindUnverifiable, RunID: "aaaaaaaa-explore", Text: "alerts page nobody", Note: "pagerduty not reachable"},
			{Kind: KindStalePrecedent, Text: "p02-irc-replaced", Note: "review by 2026-10-01; matched 2 runs", Ref: "/home/u/.local/share/aeolian/precedents/p02-irc-replaced.md"},
			{Kind: KindBlocked, RunID: "bbbbbbbb-rfc", Text: "Should the RFC cover tracing too,\nor only metrics and logs?"},
			{Kind: KindDead, RunID: "cccccccc-map", Text: "step map ended exited", Evidence: []string{"Error: rate limited"}},
			{Kind: KindHelperDied, RunID: "aaaaaaaa-explore", Text: "2 helpers ended without done: scout (lost), scout (exited)", Count: 2},
		},
	}
}

func kinds(v View) []string {
	var ks []string
	for _, g := range v.Groups {
		for _, it := range g.Items {
			ks = append(ks, it.Kind)
		}
	}
	return ks
}

func TestBuildCapsNoiseAndCountsOverflow(t *testing.T) {
	v := Build(sample(), DefaultMax)
	if v.Shown != 7 || v.Overflow != 2 {
		t.Fatalf("shown %d overflow %d", v.Shown, v.Overflow)
	}
	// The two cut are the cheapest to ignore: an unverifiable claim and the
	// unwitnessed tally.
	if slices.Contains(kinds(v), KindUnwitnessed) {
		t.Fatalf("unwitnessed made the cut: %v", kinds(v))
	}
	if all := Build(sample(), 0); all.Shown != 9 || all.Overflow != 0 {
		t.Fatalf("max 0: shown %d overflow %d", all.Shown, all.Overflow)
	}
	// A run with items, even only ones the cap cut, is never "clean".
	var clean []string
	for _, r := range v.Clean {
		clean = append(clean, r.ID)
	}
	if !slices.Equal(clean, []string{"dddddddd-clean", "eeeeeeee-clean"}) {
		t.Fatalf("clean %v", clean)
	}
}

func TestBuildRanksByCostThenNewestRun(t *testing.T) {
	v := Build(sample(), 0)
	want := []string{KindBlocked, KindDead, KindContradicted, KindHelperDied, KindUnverifiable, KindUnverifiable, KindUnverifiable, KindUnwitnessed, KindStalePrecedent}
	if got := kinds(v); !slices.Equal(got, want) {
		t.Fatalf("grouped kinds\n got %v\nwant %v", got, want)
	}
	var groups []string
	for _, g := range v.Groups {
		if g.Run == nil {
			groups = append(groups, "precedents")
		} else {
			groups = append(groups, g.Run.ID)
		}
	}
	if !slices.Equal(groups, []string{"bbbbbbbb-rfc", "cccccccc-map", "aaaaaaaa-explore", "precedents"}) {
		t.Fatalf("groups %v", groups)
	}

	// Same kind: the newer run's item comes first.
	in := Input{
		Runs: []Run{{ID: "old", FinishedAt: at(1, 0)}, {ID: "new", FinishedAt: at(2, 0)}},
		Items: []Item{
			{Kind: KindContradicted, RunID: "old", Text: "a"},
			{Kind: KindContradicted, RunID: "new", Text: "b"},
			{Kind: KindContradicted, RunID: "new", Text: "c"},
		},
	}
	got := Build(in, 2)
	if got.Groups[0].Run.ID != "new" || len(got.Groups) != 1 || got.Groups[0].Items[1].Text != "c" || got.Overflow != 1 {
		t.Fatalf("got %+v", got)
	}
	// Build is stable: the same input gives the same page.
	if Markdown(Build(sample(), 7)) != Markdown(Build(sample(), 7)) {
		t.Fatal("unstable")
	}
}

func TestEmptyDigestIsOneLine(t *testing.T) {
	in := sample()
	in.Items = nil
	v := Build(in, DefaultMax)
	md := Markdown(v)
	if md != "- aeolian: nothing needs you since 08:00 · 5 runs with nothing for you · 1 run still going\n" {
		t.Fatalf("md %q", md)
	}
	if got := Text(Build(Input{Since: t0, Now: at(1, 0)}, 7)); got != "nothing needs you since 08:00\n" {
		t.Fatalf("text %q", got)
	}
}

func TestOneRunDigest(t *testing.T) {
	in := sample()
	in.RunID = "dddddddd-clean"
	in.Runs = in.Runs[3:4]
	in.Items = nil
	if got := Markdown(Build(in, DefaultMax)); got != "- aeolian: _fix the upload bucket bug_ `dddddddd` delivered, nothing for you\n" {
		t.Fatalf("clean run: %q", got)
	}
	in = sample()
	in.RunID = "bbbbbbbb-rfc"
	in.Runs = in.Runs[1:2]
	in.Items = in.Items[6:7]
	in.Running = 0
	golden(t, "run.md", Markdown(Build(in, DefaultMax)))
}

func TestMarkdownGolden(t *testing.T) {
	golden(t, "digest.md", Markdown(Build(sample(), DefaultMax)))
	golden(t, "digest.txt", Text(Build(sample(), DefaultMax)))
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("%s differs; run go test ./internal/digest -update and review the diff\n--- got\n%s", name, got)
	}
}

func TestBuildLeavesOutDismissedItems(t *testing.T) {
	in := sample()
	for i := range in.Items {
		if in.Items[i].RunID == "cccccccc-map" || in.Items[i].Kind == KindStalePrecedent {
			in.Items[i].Dismissed = true
		}
	}
	v := Build(in, 0)
	if v.Shown != 7 || v.Dismissed != 2 || slices.Contains(kinds(v), KindDead) {
		t.Fatalf("shown %d dismissed %d kinds %v", v.Shown, v.Dismissed, kinds(v))
	}
	if !slices.ContainsFunc(v.Clean, func(r Run) bool { return r.ID == "cccccccc-map" }) {
		t.Fatal("a run whose items were all dismissed is not clean")
	}
	in.ShowDismissed = true
	if all := Build(in, 0); all.Shown != 9 || all.Dismissed != 2 {
		t.Fatalf("show dismissed: shown %d dismissed %d", all.Shown, all.Dismissed)
	}
}

func TestItemKey(t *testing.T) {
	for want, got := range map[string]string{
		"dead:r1":            ItemKey(KindDead, "r1", ""),
		"contradicted:r1:c3": ItemKey(KindContradicted, "r1", "c3"),
		"stale_precedent:p1": ItemKey(KindStalePrecedent, "", "p1"),
		"bad_precedent":      ItemKey(KindBadPrecedent, "", ""),
	} {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}
