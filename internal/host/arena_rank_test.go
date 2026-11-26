package host

import (
	"math"
	"testing"
)

func order(st []Standing) []int {
	out := make([]int, len(st))
	for i, s := range st {
		out[i] = s.Entrant
	}
	return out
}

func TestRankClearOrder(t *testing.T) {
	// two judges, three entrants: 0 beats everyone, 2 beats 1.
	games := []Game{
		{0, 1, 3}, {0, 2, 2}, {2, 1, 1},
		{0, 1, 2}, {0, 2, 1}, {2, 1, 2},
	}
	st := Rank(3, games)
	if got := order(st); got[0] != 0 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("order %v", got)
	}
	if st[0].Wins != 4 || st[0].Losses != 0 || st[2].Wins != 0 || st[2].Losses != 4 {
		t.Fatalf("tallies %+v", st)
	}
	if !(st[0].Score > st[1].Score && st[1].Score > st[2].Score) {
		t.Fatalf("scores not strictly ordered: %+v", st)
	}
}

func TestRankTieIsDeterministic(t *testing.T) {
	// a perfect cycle: equal strengths and wins; margin then index decide.
	cycle := []Game{{0, 1, 1}, {1, 2, 1}, {2, 0, 1}}
	for range 20 {
		if got := order(Rank(3, cycle)); got[0] != 0 || got[1] != 1 || got[2] != 2 {
			t.Fatalf("cycle order %v", got)
		}
	}
	byMargin := []Game{{0, 1, 1}, {1, 2, 1}, {2, 0, 3}}
	if got := order(Rank(3, byMargin)); got[0] != 2 {
		t.Fatalf("margin tie-break order %v", got)
	}
}

func TestBradleyTerryDegenerate(t *testing.T) {
	// an entrant that wins everything stays finite thanks to the prior.
	p := BradleyTerry(2, []Game{{0, 1, 1}, {0, 1, 1}, {0, 1, 1}})
	for _, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			t.Fatalf("strengths %v", p)
		}
	}
	if p[0] <= p[1] {
		t.Fatalf("winner not stronger: %v", p)
	}
	if got := BradleyTerry(3, nil); got[0] != got[1] || got[1] != got[2] {
		t.Fatalf("no games should mean equal strengths: %v", got)
	}
	if BradleyTerry(0, nil) != nil {
		t.Fatal("zero entrants")
	}
	// out-of-range games are ignored, not a panic.
	Rank(2, []Game{{5, 0, 1}, {1, 1, 1}})
}

func TestSplits(t *testing.T) {
	games := []Game{
		{0, 1, 1}, {1, 0, 2}, // split
		{0, 2, 1}, {0, 2, 1}, // agreed
		{2, 1, 1}, {1, 2, 1}, {1, 2, 3}, // split 2-1 for 1
	}
	s := Splits(games)
	if len(s) != 2 {
		t.Fatalf("splits %+v", s)
	}
	if s[0] != (Split{A: 0, B: 1, ForA: 1, ForB: 1}) || s[1] != (Split{A: 1, B: 2, ForA: 2, ForB: 1}) {
		t.Fatalf("splits %+v", s)
	}
}
