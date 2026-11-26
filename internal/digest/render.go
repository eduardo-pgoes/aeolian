package digest

import (
	"fmt"
	"strings"
	"time"
)

// labels head each item in both renderings.
var labels = map[string]string{
	KindBlocked:        "question",
	KindPick:           "your pick",
	KindDead:           "ended without delivering",
	KindContradicted:   "contradicted",
	KindHelperDied:     "helpers died",
	KindStalePrecedent: "stale precedent, not applied",
	KindBadPrecedent:   "precedent does not parse",
	KindUnverifiable:   "unverifiable",
	KindUnwitnessed:    "unwitnessed",
}

// maxClean is how many clean runs a digest names before it counts them.
const maxClean = 5

// Markdown renders the view as a block to append to a daily note: a
// heading, one checkbox per item grouped under its run, the overflow, and a
// line of runs with nothing for the person. When nothing needs them it is a
// single line.
func Markdown(v View) string {
	var b strings.Builder
	switch {
	case v.Empty() && v.RunID != "" && len(v.Clean) == 1:
		r := v.Clean[0]
		fmt.Fprintf(&b, "- aeolian: _%s_ %s %s, nothing for you\n", line(r.Goal, 90), code(short(r.ID)), endWord(r))
		return b.String()
	case v.Empty():
		fmt.Fprintf(&b, "- aeolian: nothing needs you since %s%s\n", when(v.Since, v.Now), tally(v))
		return b.String()
	}
	fmt.Fprintf(&b, "### aeolian · %s\n", needs(v))
	if v.RunID == "" {
		fmt.Fprintf(&b, "_since %s%s_\n", when(v.Since, v.Now), tally(v))
	}
	for _, g := range v.Groups {
		b.WriteString("\n")
		if g.Run == nil {
			b.WriteString("**precedents**\n")
		} else {
			fmt.Fprintf(&b, "**%s** · %s\n", line(g.Run.Goal, 90), runFacts(*g.Run))
		}
		for _, it := range g.Items {
			fmt.Fprintf(&b, "- [%s] **%s:** %s", check(it), labels[it.Kind], line(it.Text, textMax(it)))
			if it.Note != "" {
				fmt.Fprintf(&b, " — _%s_", line(it.Note, 200))
			}
			b.WriteString("\n")
			for _, e := range firstN(it.Evidence, 3) {
				fmt.Fprintf(&b, "  - %s\n", code(line(e, 160)))
			}
			if it.Ref != "" && it.Kind != KindContradicted && it.Kind != KindUnverifiable {
				fmt.Fprintf(&b, "  - %s\n", code(it.Ref))
			}
			if it.Action != "" {
				fmt.Fprintf(&b, "  - %s\n", code(it.Action))
			}
		}
	}
	if v.Overflow > 0 {
		fmt.Fprintf(&b, "\n+%d more · %s\n", v.Overflow, code(moreCommand(v)))
	}
	if len(v.Clean) > 0 {
		b.WriteString("\nnothing for you: ")
		b.WriteString(cleanList(v.Clean, func(r Run) string {
			return fmt.Sprintf("_%s_ %s", line(r.Goal, 60), code(short(r.ID)))
		}))
		b.WriteString("\n")
	}
	return b.String()
}

// Text renders the view for a terminal.
func Text(v View) string {
	var b strings.Builder
	if v.Empty() {
		fmt.Fprintf(&b, "nothing needs you since %s%s\n", when(v.Since, v.Now), tally(v))
		return b.String()
	}
	fmt.Fprintf(&b, "%s since %s%s\n", needs(v), when(v.Since, v.Now), tally(v))
	for _, g := range v.Groups {
		b.WriteString("\n")
		if g.Run == nil {
			b.WriteString("precedents\n")
		} else {
			fmt.Fprintf(&b, "%s  %s\n", line(g.Run.Goal, 90), runFacts(*g.Run))
		}
		for _, it := range g.Items {
			fmt.Fprintf(&b, "  %s%s: %s\n", dismissedMark(it), labels[it.Kind], line(it.Text, textMax(it)))
			if it.Note != "" {
				fmt.Fprintf(&b, "    note: %s\n", line(it.Note, 200))
			}
			for _, e := range firstN(it.Evidence, 3) {
				fmt.Fprintf(&b, "    evidence: %s\n", line(e, 160))
			}
			if it.Ref != "" && it.Kind != KindContradicted && it.Kind != KindUnverifiable {
				fmt.Fprintf(&b, "    %s\n", it.Ref)
			}
			if it.Action != "" {
				fmt.Fprintf(&b, "    $ %s\n", it.Action)
			}
			if it.Key != "" && !it.Dismissed {
				fmt.Fprintf(&b, "    done: aeolian digest dismiss %s\n", it.Key)
			}
		}
	}
	if v.Overflow > 0 {
		fmt.Fprintf(&b, "\n+%d more: %s\n", v.Overflow, moreCommand(v))
	}
	if len(v.Clean) > 0 {
		b.WriteString("\nnothing for you: ")
		b.WriteString(cleanList(v.Clean, func(r Run) string {
			return fmt.Sprintf("%s (%s)", line(r.Goal, 60), short(r.ID))
		}))
		b.WriteString("\n")
	}
	return b.String()
}

func needs(v View) string {
	n := v.Shown + v.Overflow
	if n == 1 {
		return "1 thing needs you"
	}
	return fmt.Sprintf("%d things need you", n)
}

// tally says how many runs finished clean and how many are still going.
func tally(v View) string {
	var parts []string
	if n := len(v.Clean); n > 0 {
		parts = append(parts, plural(n, "run", "runs")+" with nothing for you")
	}
	if v.Running > 0 {
		parts = append(parts, plural(v.Running, "run", "runs")+" still going")
	}
	if v.Dismissed > 0 {
		parts = append(parts, fmt.Sprintf("%d cleared", v.Dismissed))
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}

func check(it Item) string {
	if it.Dismissed {
		return "x"
	}
	return " "
}

func dismissedMark(it Item) string {
	if it.Dismissed {
		return "(cleared) "
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func endWord(r Run) string {
	if r.Reason != "" {
		return r.Reason
	}
	return r.State
}

func runFacts(r Run) string {
	facts := []string{endWord(r)}
	if !r.StartedAt.IsZero() && !r.FinishedAt.Before(r.StartedAt) {
		facts = append(facts, dur(r.Duration()))
	}
	facts = append(facts, short(r.ID))
	if len(r.Precedents) > 0 {
		facts = append(facts, "precedents: "+strings.Join(r.Precedents, ", "))
	}
	return strings.Join(facts, " · ")
}

func moreCommand(v View) string {
	if v.RunID != "" {
		return "aeolian digest --max 0 --run " + v.RunID
	}
	return fmt.Sprintf("aeolian digest --since %s --max 0", v.Since.Format(time.RFC3339))
}

func cleanList(runs []Run, f func(Run) string) string {
	var names []string
	for _, r := range firstN(runs, maxClean) {
		names = append(names, f(r))
	}
	s := strings.Join(names, " · ")
	if n := len(runs) - maxClean; n > 0 {
		s += fmt.Sprintf(" · +%d more", n)
	}
	return s
}

func textMax(it Item) int {
	if it.Kind == KindBlocked {
		return 500
	}
	return 240
}

// when is t as a clock time when it falls on now's day, with the date
// otherwise.
func when(t, now time.Time) string {
	y, m, d := t.Date()
	if ny, nm, nd := now.In(t.Location()).Date(); ny == y && nm == m && nd == d {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

func dur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// line folds s onto one line and cuts it to max runes.
func line(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// code wraps s as inline code, with enough backticks to hold any it has.
func code(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		return fence + " " + s + " " + fence
	}
	return fence + s + fence
}

func firstN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}
