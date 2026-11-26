package host

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// The prompts an arena hands its agents, and the arena.md it leaves behind.
// Judges see labels only: no prior's name or text reaches a judge prompt,
// and the evolver sees the same anonymized text the judges did.

func (ar *arenaRun) contextBlock() string {
	if len(ar.req.Context) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nRead this context first; it is the ground the question stands on:\n")
	for _, p := range ar.req.Context {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

func (ar *arenaRun) proposerPrompt(i int) string {
	p := ar.priors[i]
	var b strings.Builder
	fmt.Fprintf(&b, "You are one of several entrants answering the same question. Other entrants argue from other lenses; "+
		"a panel of judges will compare the answers pairwise without knowing who wrote what.\n\n")
	fmt.Fprintf(&b, "The question:\n\n%s\n", ar.req.Question)
	b.WriteString(ar.contextBlock())
	fmt.Fprintf(&b, "\nYour lens:\n\n%s\n\n", p.Text)
	fmt.Fprintf(&b, "Write a proposal under %d words in markdown: the direction you recommend, the two or three concrete "+
		"moves it implies, what you would refuse to do, and how it fails. Be specific to the context; generic advice loses. "+
		"Shorter wins ties, and anything past %d words is cut before the judges read it. "+
		"Do not name your lens or say you were given one; argue the case on its merits.\n\n", arenaWordCap, arenaWordCap*3/2)
	b.WriteString("When the proposal is ready, call done with the whole proposal as text and no worktrees. Do not write it anywhere else.\n")
	return b.String()
}

// judgeOrder is the order judge j sees the pairs in, each pair's sides
// swapped by a coin flip, seeded from the arena id and j so it is the same
// every time it is asked for.
func (ar *arenaRun) judgeOrderLocked(j int) (labels []string, pairs [][2]string) {
	for l := range ar.byLabel {
		labels = append(labels, l)
	}
	slices.Sort(labels)
	for k := range ar.pairs {
		pairs = append(pairs, k)
	}
	slices.SortFunc(pairs, func(x, y [2]string) int { return strings.Compare(x[0]+x[1], y[0]+y[1]) })
	r := arenaRand(ar.a.ID, j)
	r.Shuffle(len(labels), func(x, y int) { labels[x], labels[y] = labels[y], labels[x] })
	r.Shuffle(len(pairs), func(x, y int) { pairs[x], pairs[y] = pairs[y], pairs[x] })
	for k := range pairs {
		if r.IntN(2) == 1 {
			pairs[k][0], pairs[k][1] = pairs[k][1], pairs[k][0]
		}
	}
	return labels, pairs
}

// judgePrompt returns judge j's prompt and its entries/<label>.md files,
// each cut at the judge's reading budget.
func (ar *arenaRun) judgePrompt(j int, dir string) (string, map[string]string) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	labels, pairs := ar.judgeOrderLocked(j)
	var b strings.Builder
	b.WriteString("You are a judge. Several anonymous entries answer the same question; compare them pairwise and record " +
		"a verdict for every pair with the judgment tool.\n\n")
	budget := 0
	if p, ok := ar.persona(j); ok {
		budget = p.ReadingBudgetWords
		fmt.Fprintf(&b, "Read as this audience, not as yourself, and judge fit to this reader, not overall quality: %s.\n", p.Name())
		if br := strings.TrimSpace(p.Briefing); br != "" {
			b.WriteString("\n" + br + "\n")
		}
		if len(p.OptimizesFor) > 0 {
			b.WriteString("\nThis reader acts on, most important first:\n")
			for _, o := range p.OptimizesFor {
				fmt.Fprintf(&b, "- %s\n", o)
			}
		}
		if len(p.SaysNoWhen) > 0 {
			b.WriteString("\nThis reader says no when (one hit loses the pair unless both entries hit):\n")
			for _, o := range p.SaysNoWhen {
				fmt.Fprintf(&b, "- %s\n", o)
			}
		}
		if budget > 0 {
			fmt.Fprintf(&b, "\nReading budget: %d words. Each entry below is cut where this reader stops reading; what is past the cut does not exist for them.\n", budget)
		}
		if p.TieBreak != "" {
			fmt.Fprintf(&b, "Tie break: %s.\n", p.TieBreak)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Read as a senior engineer who has to live with the outcome.\n\n")
	}
	fmt.Fprintf(&b, "The question:\n\n%s\n", ar.req.Question)
	b.WriteString(ar.contextBlock())
	if budget == 0 && len(ar.req.Personas) == 0 {
		b.WriteString("\nRubric: prefer the entry that is more specific to the context, more likely to work, and more honest about how it fails. " +
			"Do not reward length; when two entries are otherwise even, the shorter one wins. Ignore style and confidence.\n")
	} else {
		b.WriteString("\nRubric: the entry this reader would act on wins. Do not reward length; when two entries are otherwise even, the shorter one wins.\n")
	}
	for _, r := range ar.req.Rubric {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	b.WriteString("\n")
	files := map[string]string{}
	// Entry paths are relative to the judge's own folder: an absolute one
	// would point it at the aeolian home, where the other arenas live.
	b.WriteString("The entries, one file each in your working folder, named by label; read every one, and nothing else, before judging:\n")
	for _, l := range labels {
		name := filepath.Join("entries", l+".md")
		files[name] = cutWords(ar.proposals[ar.byLabel[l]], budget, "the reader stopped here") + "\n"
		fmt.Fprintf(&b, "- %s: %s\n", l, name)
	}
	b.WriteString("\n")
	b.WriteString("Judge these pairs, in this order, one judgment call each:\n")
	for _, p := range pairs {
		fmt.Fprintf(&b, "- a=%s b=%s\n", p[0], p[1])
	}
	b.WriteString("\nFor each pair call judgment with a, b, the winner's label, a margin (1 slight, 2 clear, 3 decisive), " +
		"why in one or two sentences, and optionally steal: the one idea from either entry most worth keeping. " +
		"When every pair is judged, call done with a two-line summary of what separated the entries, and no worktrees.\n")
	return b.String(), files
}

// evolverPrompt returns the evolver's prompt and its files: the top two
// entries and what the judges said.
func (ar *arenaRun) evolverPrompt(dir string) (string, map[string]string) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var b strings.Builder
	files := map[string]string{}
	b.WriteString("You write the synthesis of a tournament. Judges ranked anonymous answers to one question; " +
		"you get the top two and what the judges said was worth stealing from any entry.\n\n")
	fmt.Fprintf(&b, "The question:\n\n%s\n", ar.req.Question)
	b.WriteString(ar.contextBlock())
	b.WriteString("\nRead these files first:\n")
	for k, s := range ar.a.Standings {
		if k == 2 {
			break
		}
		name := fmt.Sprintf("rank-%d-%s.md", s.Rank, s.Label)
		files[name] = ar.proposals[ar.byLabel[s.Label]] + "\n"
		fmt.Fprintf(&b, "- %s: entry %s, ranked %d, won %d, lost %d\n", filepath.Join(dir, name), s.Label, s.Rank, s.Wins, s.Losses)
	}
	fmt.Fprintf(&b, "- %s: what the judges marked worth stealing, and where they split\n\n", filepath.Join(dir, "judges.md"))
	var j strings.Builder
	var steals []string
	for _, jm := range ar.a.Judgments {
		if jm.Steal != "" {
			steals = append(steals, fmt.Sprintf("- (%s vs %s) %s", jm.A, jm.B, jm.Steal))
		}
	}
	j.WriteString("# what the judges said\n\n## worth stealing\n\n")
	if len(steals) == 0 {
		j.WriteString("nothing.\n")
	}
	j.WriteString(strings.Join(steals, "\n") + "\n\n## where they split\n\n")
	if len(ar.a.Splits) == 0 {
		j.WriteString("nowhere.\n")
	}
	for _, sp := range ar.a.Splits {
		fmt.Fprintf(&j, "- %s vs %s:\n", sp.A, sp.B)
		for _, w := range sp.Why {
			fmt.Fprintf(&j, "  - %s\n", w)
		}
	}
	files["judges.md"] = j.String()
	fmt.Fprintf(&b, "Merge the top entry with what is strongest in the second and in the steals into one proposal under %d words. "+
		"Keep the winner's spine; do not average the two into mush. Where the judges split, say which side you took and why. "+
		"End with a short 'what this does not settle' list. Call done with the synthesis as text and no worktrees.\n", arenaWordCap)
	return b.String(), files
}

// reportLocked is arena.md: the standings, where the judges split, and the
// synthesis or the pick left to the human.
func (ar *arenaRun) reportLocked() string {
	a := &ar.a
	var b strings.Builder
	fmt.Fprintf(&b, "# arena %s\n\n> %s\n\n", a.ID, a.Question)
	fmt.Fprintf(&b, "state: **%s**", a.State)
	if a.Note != "" {
		fmt.Fprintf(&b, " (%s)", a.Note)
	}
	b.WriteString("\n\n")
	if len(a.Context) > 0 {
		b.WriteString("context: " + strings.Join(a.Context, ", ") + "\n\n")
	}

	b.WriteString("## standings\n\n")
	if len(a.Standings) == 0 {
		b.WriteString("no ranking: no judgment came in.\n\n")
	} else {
		b.WriteString("This is what the judges agree on, not what is right. The lens column was hidden from the judges.\n\n")
		b.WriteString("| rank | entry | lens | strength | won | lost | margin | words | proposal |\n|---|---|---|---|---|---|---|---|---|\n")
		var ties []string
		for k, s := range a.Standings {
			e := a.Entrants[ar.byLabel[s.Label]]
			fmt.Fprintf(&b, "| %d | %s | %s | %.3f | %d | %d | %d | %d | %s |\n",
				s.Rank, s.Label, s.Prior, s.Score, s.Wins, s.Losses, s.Margin, s.Words, e.Proposal)
			if s.Tie {
				ties = append(ties, fmt.Sprintf("%s and %s", a.Standings[k-1].Label, s.Label))
			}
		}
		b.WriteString("\n")
		forfeited := 0
		for _, jm := range a.Judgments {
			if jm.Judge < len(a.Judges) && a.Judges[jm.Judge].State == seatForfeit {
				forfeited++
			}
		}
		if forfeited > 0 {
			fmt.Fprintf(&b, "%d of these %d judgments came from judges who later forfeited; they count, but weigh them knowing that.\n\n", forfeited, len(a.Judgments))
		}
		if len(ties) > 0 {
			fmt.Fprintf(&b, "tied: %s are equal on strength, wins and margin; their order is a coin the arena flipped, not a ranking.\n\n", strings.Join(ties, "; "))
		}
	}
	var out []string
	for _, e := range a.Entrants {
		if e.State != seatIn {
			out = append(out, fmt.Sprintf("- %s: %s %s", e.Prior, e.State, e.Note))
		}
	}
	if len(out) > 0 {
		b.WriteString("did not make it to judging:\n" + strings.Join(out, "\n") + "\n\n")
	}

	b.WriteString("## where the judges split\n\n")
	switch {
	case len(a.Standings) == 0:
		b.WriteString("nothing to split on.\n\n")
	case len(a.Splits) == 0:
		b.WriteString("no split: every pair went the same way for every judge who judged it.\n\n")
	default:
		b.WriteString("Read these first. A split is where the ranking is least sure and your judgment matters most.\n\n")
		for _, sp := range a.Splits {
			fmt.Fprintf(&b, "### %s vs %s (%d–%d)\n\n", sp.A, sp.B, sp.ForA, sp.ForB)
			for _, w := range sp.Why {
				fmt.Fprintf(&b, "- %s\n", w)
			}
			b.WriteString("\n")
		}
	}

	if a.Final == FinalHuman {
		b.WriteString("## your pick\n\n")
		if len(a.Standings) < 2 {
			b.WriteString("fewer than two ranked entries; nothing to pick between.\n\n")
		} else {
			for _, s := range a.Standings[:2] {
				fmt.Fprintf(&b, "%d. **%s** (%s): %s\n", s.Rank, s.Label, s.Prior, a.Entrants[ar.byLabel[s.Label]].Proposal)
			}
			fmt.Fprintf(&b, "\npick: read both, then write the label you choose and one line on why into %s\n\n",
				a.Dir+string(os.PathSeparator)+"pick.md")
		}
	} else {
		b.WriteString("## synthesis\n\n")
		switch {
		case a.Synthesis != "":
			fmt.Fprintf(&b, "%s\n\n", a.Synthesis)
		case a.State == ArenaDone:
			b.WriteString("no synthesis came in; read the top two proposals.\n\n")
		default:
			b.WriteString("none.\n\n")
		}
	}

	b.WriteString("## judges\n\n")
	for _, jd := range a.Judges {
		who := fmt.Sprintf("judge %d", jd.Index+1)
		if jd.Persona != "" {
			who += " (" + jd.Persona + ")"
		}
		fmt.Fprintf(&b, "- %s: %s, %d of %d pairs", who, jd.State, jd.Judged, jd.Pairs)
		if jd.Note != "" {
			fmt.Fprintf(&b, ", %s", jd.Note)
		}
		b.WriteString("\n")
	}
	if len(a.Judgments) > 0 {
		b.WriteString("\n## every judgment\n\n")
		for _, jm := range a.Judgments {
			fmt.Fprintf(&b, "- judge %d: %s over %s (margin %d): %s", jm.Judge+1, jm.Winner, other(jm.Winner, jm.A, jm.B), jm.Margin, jm.Why)
			if jm.Steal != "" {
				fmt.Fprintf(&b, " steal: %s", jm.Steal)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
