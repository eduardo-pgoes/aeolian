package host

import (
	"math"
	"slices"
)

// Game is one pairwise verdict between entrants Winner and Loser, by index.
// Margin is the judge's 1-3 confidence; it breaks ties, it does not weigh
// the win.
type Game struct {
	Winner, Loser int
	Margin        int
}

// Standing is one entrant's place after ranking. Score is its Bradley-Terry
// strength, normalized so the strengths sum to the number of entrants.
type Standing struct {
	Entrant int     `json:"entrant"`
	Score   float64 `json:"score"`
	Wins    int     `json:"wins"`
	Losses  int     `json:"losses"`
	Margin  int     `json:"margin"`
}

const (
	// btIterations is how many minorization-maximization updates the
	// ranking runs; with a handful of entrants it converges long before.
	btIterations = 50
	// btPrior is the virtual win each entrant gets against every other.
	// It keeps the strengths finite when an entrant wins or loses every
	// game, which plain Bradley-Terry cannot score.
	btPrior = 0.1
)

// BradleyTerry returns the strength of each of n entrants from games, with
// Hunter's MM update: p_i = W_i / sum_j n_ij / (p_i + p_j). Every pair also
// gets btPrior virtual wins each way, so the result is always defined.
func BradleyTerry(n int, games []Game) []float64 {
	if n == 0 {
		return nil
	}
	wins := make([]float64, n)
	pairs := make([][]float64, n)
	for i := range pairs {
		pairs[i] = make([]float64, n)
	}
	for _, g := range games {
		if g.Winner < 0 || g.Winner >= n || g.Loser < 0 || g.Loser >= n || g.Winner == g.Loser {
			continue
		}
		wins[g.Winner]++
		pairs[g.Winner][g.Loser]++
		pairs[g.Loser][g.Winner]++
	}
	for i := range n {
		for j := range n {
			if i != j {
				wins[i] += btPrior
				pairs[i][j] += 2 * btPrior
			}
		}
	}
	p := make([]float64, n)
	for i := range p {
		p[i] = 1
	}
	next := make([]float64, n)
	for range btIterations {
		for i := range n {
			den := 0.0
			for j := range n {
				if i != j && pairs[i][j] > 0 {
					den += pairs[i][j] / (p[i] + p[j])
				}
			}
			next[i] = p[i]
			if den > 0 {
				next[i] = wins[i] / den
			}
		}
		sum := 0.0
		for _, v := range next {
			sum += v
		}
		for i := range p {
			p[i] = next[i] * float64(n) / sum
		}
	}
	return p
}

// Rank orders n entrants by Bradley-Terry strength over games, rounded to
// the three decimals it reports, so two strengths that print the same count
// as equal. Ties go to more wins, then the larger summed margin of those
// wins, then the lower entrant index, so the order never depends on map or
// goroutine order. A caller that must not favour low indexes numbers its
// entrants in a shuffled order.
func Rank(n int, games []Game) []Standing {
	scores := BradleyTerry(n, games)
	out := make([]Standing, n)
	for i := range out {
		out[i] = Standing{Entrant: i, Score: math.Round(scores[i]*1000) / 1000}
	}
	for _, g := range games {
		if g.Winner < 0 || g.Winner >= n || g.Loser < 0 || g.Loser >= n || g.Winner == g.Loser {
			continue
		}
		out[g.Winner].Wins++
		out[g.Winner].Margin += g.Margin
		out[g.Loser].Losses++
	}
	slices.SortStableFunc(out, func(a, b Standing) int {
		if a.Score != b.Score {
			if b.Score > a.Score {
				return 1
			}
			return -1
		}
		if a.Wins != b.Wins {
			return b.Wins - a.Wins
		}
		if a.Margin != b.Margin {
			return b.Margin - a.Margin
		}
		return a.Entrant - b.Entrant
	})
	return out
}

// Tied says whether a and b are equal on everything Rank orders by but
// the entrant index.
func Tied(a, b Standing) bool {
	return a.Score == b.Score && a.Wins == b.Wins && a.Margin == b.Margin
}

// Split is a pair of entrants the judges did not agree on: some picked A,
// some picked B.
type Split struct {
	A, B       int
	ForA, ForB int
}

// Splits returns every pair whose games went both ways, in entrant order.
func Splits(games []Game) []Split {
	type key struct{ a, b int }
	tally := map[key]*Split{}
	var keys []key
	for _, g := range games {
		k := key{min(g.Winner, g.Loser), max(g.Winner, g.Loser)}
		s, ok := tally[k]
		if !ok {
			s = &Split{A: k.a, B: k.b}
			tally[k] = s
			keys = append(keys, k)
		}
		if g.Winner == k.a {
			s.ForA++
		} else {
			s.ForB++
		}
	}
	slices.SortFunc(keys, func(x, y key) int {
		if x.a != y.a {
			return x.a - y.a
		}
		return x.b - y.b
	})
	var out []Split
	for _, k := range keys {
		if s := tally[k]; s.ForA > 0 && s.ForB > 0 {
			out = append(out, *s)
		}
	}
	return out
}
