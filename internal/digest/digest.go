// Package digest builds the return page: what happened while the person
// was away, cut down to what needs them. It is pure: the host gathers runs,
// claims and precedents into an Input, and Build ranks, caps and groups
// them. A digest that lists everything is an inbox nobody reads, so the cap
// is the point: items are ranked by what it costs the person to ignore
// them, only the top few are shown, and the rest are counted.
package digest

import (
	"sort"
	"strings"
	"time"
)

// Kinds of item, in the order they rank: a run stopped on a question costs
// the most to ignore, a claim no witness looked at the least.
const (
	// KindBlocked: a run stopped on a question only the person can answer.
	KindBlocked = "blocked"
	// KindPick: an arena with a human final ranked its entries and waits
	// on the person to pick between the top two.
	KindPick = "pick"
	// KindDead: a run ended without delivering (lost, exited, failed to
	// start, failed).
	KindDead = "dead"
	// KindContradicted: a witness found a claim of a delivered result wrong.
	KindContradicted = "contradicted"
	// KindHelperDied: helpers of a run ended without done, so their lead
	// worked around a hole.
	KindHelperDied = "helper_died"
	// KindStalePrecedent: a precedent past its review_by matched a run and
	// so was left out of its prompt.
	KindStalePrecedent = "stale_precedent"
	// KindBadPrecedent: precedent files that do not parse, and so are never
	// applied.
	KindBadPrecedent = "bad_precedent"
	// KindUnverifiable: a witness could not check a claim either way.
	KindUnverifiable = "unverifiable"
	// KindUnwitnessed: claims no witness answered in time; one item per run.
	KindUnwitnessed = "unwitnessed"
)

var rank = map[string]int{
	KindBlocked: 0, KindPick: 1, KindDead: 2, KindContradicted: 3, KindHelperDied: 4,
	KindStalePrecedent: 5, KindBadPrecedent: 6, KindUnverifiable: 7, KindUnwitnessed: 8,
}

// DefaultMax is how many items a digest shows unless asked for more.
const DefaultMax = 7

// Item is one thing that needs the person. RunID is empty for items about
// precedents. Text is the headline (the question, the claim, the
// precedent id); Note says why it is here; Ref is what to act on (a claim
// id, a precedent's file); Action is a command that acts on it.
type Item struct {
	Kind     string   `json:"kind"`
	RunID    string   `json:"run_id,omitempty"`
	Text     string   `json:"text"`
	Note     string   `json:"note,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
	Ref      string   `json:"ref,omitempty"`
	Action   string   `json:"action,omitempty"`
	// Count is how many things one item stands for, such as unwitnessed
	// claims or dead helpers; 0 means one.
	Count     int       `json:"count,omitempty"`
	Key       string    `json:"key"`
	At        time.Time `json:"at"`
	Dismissed bool      `json:"dismissed,omitempty"`
}

func ItemKey(kind, runID, subject string) string {
	parts := []string{kind}
	for _, p := range []string{runID, subject} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, ":")
}

// Run is a finished run as the digest shows it. Precedents are the ids
// injected into its agents' prompts.
type Run struct {
	ID         string    `json:"id"`
	Goal       string    `json:"goal"`
	State      string    `json:"state"`
	Reason     string    `json:"reason,omitempty"`
	Source     string    `json:"source,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Precedents []string  `json:"precedents,omitempty"`
}

// Duration is how long the run took.
func (r Run) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// Input is everything the host gathered for one digest.
type Input struct {
	Since time.Time
	Now   time.Time
	// RunID is set when the digest is of one run only.
	RunID string
	// Runs finished since Since, in any order.
	Runs  []Run
	Items []Item
	// Running counts runs still busy or queued.
	Running       int
	ShowDismissed bool
}

// Group is the items of one run, or of no run (precedents) when Run is
// nil.
type Group struct {
	Run   *Run   `json:"run,omitempty"`
	Items []Item `json:"items"`
}

// View is a built digest.
type View struct {
	Since time.Time `json:"since"`
	Now   time.Time `json:"now"`
	// RunID is set when the digest is of one run only; then Since means
	// nothing.
	RunID string `json:"run_id,omitempty"`
	// Groups hold the items shown, grouped by run, the group with the most
	// costly item first.
	Groups []Group `json:"groups"`
	// Shown and Overflow count the items shown and the ones the cap left
	// out.
	Shown    int `json:"shown"`
	Overflow int `json:"overflow"`
	// Clean are the runs with nothing for the person, newest first.
	Clean     []Run `json:"clean"`
	Running   int   `json:"running"`
	Dismissed int   `json:"dismissed"`
}

// Empty reports whether nothing needs the person.
func (v View) Empty() bool { return v.Shown == 0 && v.Overflow == 0 }

// Build ranks the items by kind, then newest run first, then the order
// they came in; keeps the first max (max <= 0 keeps all); and groups them
// by run. A run none of whose items made the cut still counts toward
// Overflow and is not listed as clean.
func Build(in Input, max int) View {
	runs := map[string]*Run{}
	for i := range in.Runs {
		runs[in.Runs[i].ID] = &in.Runs[i]
	}
	finished := func(it Item) time.Time {
		if r := runs[it.RunID]; r != nil {
			return r.FinishedAt
		}
		return time.Time{}
	}
	var items []Item
	dismissed := 0
	for _, it := range in.Items {
		if it.Dismissed {
			dismissed++
			if !in.ShowDismissed {
				continue
			}
		}
		items = append(items, it)
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if rank[a.Kind] != rank[b.Kind] {
			return rank[a.Kind] < rank[b.Kind]
		}
		return finished(a).After(finished(b))
	})
	v := View{Since: in.Since, Now: in.Now, RunID: in.RunID, Running: in.Running, Dismissed: dismissed, Groups: []Group{}, Clean: []Run{}}
	if max > 0 && len(items) > max {
		v.Overflow = len(items) - max
		items = items[:max]
	}
	v.Shown = len(items)
	at := map[string]int{}
	for _, it := range items {
		g, ok := at[it.RunID]
		if !ok {
			g = len(v.Groups)
			at[it.RunID] = g
			v.Groups = append(v.Groups, Group{Run: runs[it.RunID]})
		}
		v.Groups[g].Items = append(v.Groups[g].Items, it)
	}
	busy := map[string]bool{}
	for _, it := range items {
		busy[it.RunID] = true
	}
	for _, r := range in.Runs {
		if !busy[r.ID] {
			v.Clean = append(v.Clean, r)
		}
	}
	sort.SliceStable(v.Clean, func(i, j int) bool { return v.Clean[i].FinishedAt.After(v.Clean[j].FinishedAt) })
	return v
}
