package host

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/precedent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

// WitnessRole is the role of the agent the host starts to check a step
// agent's claims. It is reserved: an agent in it gets the verdict tool on
// its parent's claims, no spawn tool and no grants.
const WitnessRole = workflow.WitnessRole

const (
	DefaultWitnessModel     = "sonnet"
	DefaultWitnessTimeout   = 20 * time.Minute
	DefaultWitnessMaxClaims = 12
	// NoPrecedents as WitnessConfig.Precedents hands the witness none.
	NoPrecedents = "none"
	// witnessInline is how much input goes into the witness's first prompt
	// as is; more stays in its input file. tmux refuses a command line much
	// over 16KB.
	witnessInline = 6000
	maxPrecedents = 15
)

// WitnessConfig is the witness section of config.json. A step with
// witness: true in its workflow is always witnessed; All witnesses every
// step agent that hands back claims.
type WitnessConfig struct {
	All bool `json:"all,omitempty"`
	// Model is the witness's model. Empty means DefaultWitnessModel, a
	// different tier from most producers, so their mistakes correlate less.
	Model string `json:"model,omitempty"`
	// Timeout is a Go duration, such as 20m: how long a witness may hold
	// the run before its claims are marked unwitnessed.
	Timeout string `json:"timeout,omitempty"`
	// MaxClaims caps the claims of a witnessed step.
	MaxClaims int `json:"max_claims,omitempty"`
	// EvidenceOptional lets a witnessed claim come without evidence.
	EvidenceOptional bool `json:"evidence_optional,omitempty"`
	// Precedents is the folder of precedent files. Empty means the host's
	// (config.json's precedents_dir, else $AEOLIAN_HOME/precedents);
	// NoPrecedents means none.
	Precedents string `json:"precedents,omitempty"`
}

func (c WitnessConfig) Validate() error {
	if c.Timeout != "" {
		d, err := time.ParseDuration(c.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("witness.timeout %q is not a positive duration such as 20m", c.Timeout)
		}
	}
	if c.MaxClaims < 0 {
		return errors.New("witness.max_claims must not be negative")
	}
	return nil
}

func (c WitnessConfig) timeout() time.Duration {
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultWitnessTimeout
}

func (c WitnessConfig) maxClaims() int {
	if c.MaxClaims > 0 {
		return c.MaxClaims
	}
	return DefaultWitnessMaxClaims
}

func (c WitnessConfig) model() string {
	if c.Model != "" {
		return c.Model
	}
	return DefaultWitnessModel
}

// Gate is the witness as a done gate: Check holds a witnessed step's claims
// to the policy, and Hold has a witness give each a verdict before the run
// moves on.
func (c WitnessConfig) Gate() DoneGate {
	return DoneGate{Name: "witness", Check: c.check, Hold: c.hold, Timeout: c.timeout()}
}

// mode says whether a's done is witnessed, and whether it must then carry
// claims.
func (c WitnessConfig) mode(h *Host, a store.Agent) (witnessed, required bool) {
	if a.ParentID != "" || a.RunID == "" || a.Role == WitnessRole {
		return false, false
	}
	wf, err := h.wf(a.RunID)
	if err == nil && wf.Witnessed(a.Step) {
		return true, true
	}
	return c.All, false
}

func (c WitnessConfig) check(h *Host, call DoneCall) error {
	if call.Outcome == OutcomeBlocked {
		return nil
	}
	on, required := c.mode(h, call.Agent)
	if !on {
		return nil
	}
	claims := claimsOf(call.Handoff.Parts)
	if len(claims) == 0 {
		if required {
			return fmt.Errorf("this step is witnessed: add a claims part with the statements a reader most needs checked (at most %d), each with evidence", c.maxClaims())
		}
		return nil
	}
	if len(claims) > c.maxClaims() {
		return fmt.Errorf("%d claims; a witnessed step hands back at most %d: keep the ones a reader most needs checked and merge or drop the rest", len(claims), c.maxClaims())
	}
	roots := evidenceRoots(call.Agent, call.Handoff)
	for _, cl := range claims {
		if len(cl.Evidence) == 0 && !c.EvidenceOptional {
			return fmt.Errorf("claim %s has no evidence: give at least one path:line, URL or 'cmd: ...' that shows it, or drop the claim", cl.ID)
		}
		for _, ev := range cl.Evidence {
			if err := checkEvidence(ev, roots); err != nil {
				return fmt.Errorf("claim %s: %w", cl.ID, err)
			}
		}
	}
	return nil
}

func (c WitnessConfig) hold(ctx context.Context, h *Host, call DoneCall) error {
	if call.Outcome == OutcomeBlocked {
		return nil
	}
	if on, _ := c.mode(h, call.Agent); !on {
		return nil
	}
	return h.RunWitness(ctx, c, call.Agent, nil)
}

func claimsOf(parts []store.Part) []ClaimInput {
	var out []ClaimInput
	for _, p := range parts {
		if p.Kind != "claims" {
			continue
		}
		var b ClaimsBody
		if json.Unmarshal(p.Body, &b) == nil {
			out = append(out, b.Claims...)
		}
	}
	return out
}

// evidenceRoots are the folders a relative evidence path may sit in: the
// agent's own and the worktrees its handoff lists.
func evidenceRoots(a store.Agent, hand store.Handoff) []string {
	var roots []string
	for _, w := range hand.Worktrees {
		roots = append(roots, w.Path)
	}
	if a.Dir != "" {
		roots = append(roots, a.Dir)
	}
	return roots
}

// witnessRoots are the folders a witness reads beside its own: those its
// claims' evidence may sit in, the producer's folder and the worktrees it
// handed back.
func witnessRoots(producer store.Agent) []string {
	if producer.Handoff == nil {
		return nil
	}
	return evidenceRoots(producer, *producer.Handoff)
}

var lineRef = regexp.MustCompile(`^(.+?):(\d+)(?:-(\d+))?$`)

// fileRef splits evidence like path:12 or path:12-20. ok is false for a
// URL, a command, or a bare path.
func fileRef(ev string) (path string, from, to int, ok bool) {
	if isCmdOrURL(ev) {
		return "", 0, 0, false
	}
	m := lineRef.FindStringSubmatch(strings.Trim(ev, "`"))
	if m == nil {
		return "", 0, 0, false
	}
	from, _ = strconv.Atoi(m[2])
	to = from
	if m[3] != "" {
		to, _ = strconv.Atoi(m[3])
	}
	return m[1], from, to, true
}

func isCmdOrURL(ev string) bool {
	l := strings.ToLower(ev)
	return strings.HasPrefix(l, "cmd:") || strings.Contains(l, "://")
}

func resolveEvidencePath(path string, roots []string) (string, bool) {
	if filepath.IsAbs(path) {
		_, err := os.Stat(path)
		return path, err == nil
	}
	for _, r := range roots {
		p := filepath.Join(r, path)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	return "", false
}

// checkEvidence refuses evidence that cannot be right: an absolute path
// that does not exist, or a line past the end of a file found. A relative
// path found nowhere passes; the witness judges it.
func checkEvidence(ev string, roots []string) error {
	path, from, to, ok := fileRef(ev)
	if !ok {
		if !isCmdOrURL(ev) && filepath.IsAbs(ev) {
			if _, err := os.Stat(ev); err != nil {
				return fmt.Errorf("evidence %q does not exist", ev)
			}
		}
		return nil
	}
	p, found := resolveEvidencePath(path, roots)
	if !found {
		if filepath.IsAbs(path) {
			return fmt.Errorf("evidence %q: %s does not exist", ev, path)
		}
		return nil
	}
	if from < 1 || to < from {
		return fmt.Errorf("evidence %q has no valid line range", ev)
	}
	n, err := countLines(p)
	if err != nil {
		return nil
	}
	if to > n {
		return fmt.Errorf("evidence %q points past the end of %s, which has %d lines", ev, p, n)
	}
	return nil
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		n++
	}
	return n, sc.Err()
}

const (
	// excerptContext is how many lines an excerpt shows on each side of
	// the lines its evidence cites.
	excerptContext = 40
	// excerptWhole is the size, in lines, up to which a cited file is
	// excerpted whole.
	excerptWhole = 150
	// excerptSpan caps the cited range itself; a longer one is cut.
	excerptSpan = 200
	// excerptLine cuts each line of an excerpt, in bytes.
	excerptLine = 300
	// claimExcerpts caps the excerpts of one claim, in bytes; inputExcerpts
	// caps all of a witness's input.
	claimExcerpts = 24 << 10
	inputExcerpts = 160 << 10
)

// excerptCut ends an excerpt that a cap shortened.
const excerptCut = "… (cut here; open the file to see more)"

// excerpt reads the lines a path:line evidence names with excerptContext
// lines around them, or the whole file when it has at most excerptWhole
// lines. Each line carries its number; a cited line is marked with *. It
// returns "" for anything else.
func excerpt(ev string, roots []string) string {
	path, from, to, ok := fileRef(ev)
	if !ok {
		return ""
	}
	p, found := resolveEvidencePath(path, roots)
	if !found {
		return ""
	}
	if from < 1 || to < from {
		return ""
	}
	if to-from >= excerptSpan {
		to = from + excerptSpan - 1
	}
	n, err := countLines(p)
	if err != nil {
		return ""
	}
	lo, hi := 1, n
	if n > excerptWhole {
		lo, hi = max(1, from-excerptContext), min(n, to+excerptContext)
	}
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var lines []string
	for i := 1; sc.Scan() && i <= hi; i++ {
		if i < lo {
			continue
		}
		l := sc.Text()
		if len(l) > excerptLine {
			l = l[:excerptLine] + "…"
		}
		mark := " "
		if i >= from && i <= to {
			mark = "*"
		}
		lines = append(lines, fmt.Sprintf("%5d%s %s", i, mark, l))
	}
	return strings.Join(lines, "\n")
}

// capExcerpt cuts x to at most n bytes, at a line's end, marking the cut.
func capExcerpt(x string, n int) string {
	if len(x) <= n {
		return x
	}
	if n <= 0 {
		return excerptCut
	}
	cut := x[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n" + excerptCut
}

// evidenceReadRoots are the folders a witness may read for the evidence
// of claims beyond base: for each file evidence that resolves, its git
// top level, or its folder when that is refused or it is in no repository.
// A folder is refused when it is /, $HOME or holds $HOME, or lies under
// home (the host's own folder) outside the producer's own folders in own.
func evidenceReadRoots(claims []WitnessClaim, base []string, home, userHome string, own []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range base {
		seen[filepath.Clean(r)] = true
	}
	allowed := func(dir string) bool {
		dir = filepath.Clean(dir)
		if dir == "/" || dir == "." || !filepath.IsAbs(dir) {
			return false
		}
		if userHome != "" && within(userHome, dir) {
			return false
		}
		if home != "" && (within(dir, home) || within(home, dir)) {
			for _, o := range own {
				if o != "" && within(dir, o) {
					return true
				}
			}
			return false
		}
		return true
	}
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, c := range claims {
		for _, e := range c.Evidence {
			path, _, _, ok := fileRef(e.Ref)
			if !ok {
				if isCmdOrURL(e.Ref) {
					continue
				}
				path = strings.Trim(e.Ref, "`")
			}
			p, found := resolveEvidencePath(path, base)
			if !found {
				continue
			}
			if r, err := filepath.EvalSymlinks(p); err == nil {
				p = r
			}
			dir := p
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				dir = filepath.Dir(p)
			}
			if top := gitTop(dir); top != "" && allowed(top) {
				add(top)
			} else if allowed(dir) {
				add(dir)
			}
		}
	}
	return out
}

// within reports whether path is dir or lies under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel)
}

// gitTop is the top level of the git repository dir lies in, or "".
func gitTop(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}

// WitnessInput is what a witness is handed, also written as JSON beside
// its prompt.
type WitnessInput struct {
	Claims    []WitnessClaim `json:"claims"`
	Artifacts []string       `json:"artifacts,omitempty"`
	Roots     []string       `json:"roots,omitempty"`
	// ReadRoots are the folders the witness may open files in beyond its
	// own, the producer's and Roots: where its evidence points.
	ReadRoots       []string       `json:"read_roots,omitempty"`
	Precedents      []precedentRef `json:"precedents,omitempty"`
	StalePrecedents []precedentRef `json:"stale_precedents,omitempty"`
	fresh           []precedent.Precedent
}

type WitnessClaim struct {
	ID         string            `json:"id"`
	Text       string            `json:"text"`
	Kind       string            `json:"kind,omitempty"`
	Confidence string            `json:"confidence,omitempty"`
	Evidence   []WitnessEvidence `json:"evidence"`
	Precedents []string          `json:"precedents,omitempty"`
	Stale      []string          `json:"stale_precedents,omitempty"`
}

type WitnessEvidence struct {
	Ref     string `json:"ref"`
	Excerpt string `json:"excerpt,omitempty"`
}

type precedentRef struct {
	ID       string `json:"id"`
	ReviewBy string `json:"review_by"`
}

// WitnessGiven stands in for what the host would work out itself, keyed by
// claim store id; the replay harness uses it. Excerpts are the content at
// each evidence ref, in order. Precedents are the ids in scope of a claim,
// in place of matching; they are still sorted fresh or stale by date.
type WitnessGiven struct {
	Excerpts   map[string][]string
	Precedents map[string][]string
}

// witnessInput builds the input for claims of producer. A precedent is in
// scope of a claim when its scope matches the claim's text, evidence and
// excerpts with the run's goal; given may be nil.
func (h *Host) witnessInput(c WitnessConfig, producer store.Agent, claims []store.Claim, given *WitnessGiven, now time.Time) WitnessInput {
	if given == nil {
		given = &WitnessGiven{}
	}
	goal := ""
	if producer.RunID != "" {
		if run, err := h.Store.Run(producer.RunID); err == nil {
			goal = run.Goal
		}
	}
	var hand store.Handoff
	if producer.Handoff != nil {
		hand = *producer.Handoff
	}
	roots := evidenceRoots(producer, hand)
	in := WitnessInput{Roots: roots}
	for _, p := range hand.Parts {
		if p.Kind == "artifact" {
			var b artifactBody
			if json.Unmarshal(p.Body, &b) == nil {
				in.Artifacts = append(in.Artifacts, b.Path)
			}
		}
	}
	ps := h.precedents(c)
	keys := map[string]int{}
	for _, cl := range claims {
		keys[cl.Key]++
	}
	fresh, stale := map[string]precedent.Precedent{}, map[string]precedent.Precedent{}
	inputBytes := 0
	for _, cl := range claims {
		id := cl.Key
		if id == "" || keys[id] > 1 {
			id = cl.ID
		}
		wc := WitnessClaim{ID: id, Text: cl.Text, Kind: cl.Kind, Confidence: cl.Confidence, Evidence: []WitnessEvidence{}}
		text := []string{cl.Text}
		claimBytes := 0
		for i, ev := range cl.Evidence {
			e := WitnessEvidence{Ref: ev}
			if xs, ok := given.Excerpts[cl.ID]; ok {
				if i < len(xs) {
					e.Excerpt = xs[i]
				}
			} else {
				e.Excerpt = excerpt(ev, roots)
			}
			if e.Excerpt != "" {
				room := min(claimExcerpts-claimBytes, inputExcerpts-inputBytes)
				e.Excerpt = capExcerpt(e.Excerpt, room)
				claimBytes += len(e.Excerpt)
				inputBytes += len(e.Excerpt)
			}
			wc.Evidence = append(wc.Evidence, e)
			text = append(text, ev, e.Excerpt)
		}
		var m precedent.Matched
		if ids, ok := given.Precedents[cl.ID]; ok {
			for _, p := range ps {
				switch {
				case !slices.Contains(ids, p.ID):
				case p.Stale(now):
					m.Stale = append(m.Stale, p)
				default:
					m.Fresh = append(m.Fresh, p)
				}
			}
		} else {
			m = precedent.Match(ps, now, append(text, goal)...)
		}
		for _, p := range m.Fresh {
			wc.Precedents = append(wc.Precedents, p.ID)
			fresh[p.ID] = p
		}
		for _, p := range m.Stale {
			wc.Stale = append(wc.Stale, p.ID)
			stale[p.ID] = p
		}
		in.Claims = append(in.Claims, wc)
	}
	for _, id := range slices.Sorted(maps.Keys(fresh)) {
		in.fresh = append(in.fresh, fresh[id])
		in.Precedents = append(in.Precedents, precedentRef{ID: id, ReviewBy: fresh[id].ReviewBy.Format(precedent.DateLayout)})
	}
	if len(in.fresh) > maxPrecedents {
		in.fresh, in.Precedents = in.fresh[:maxPrecedents], in.Precedents[:maxPrecedents]
	}
	for _, id := range slices.Sorted(maps.Keys(stale)) {
		in.StalePrecedents = append(in.StalePrecedents, precedentRef{ID: id, ReviewBy: stale[id].ReviewBy.Format(precedent.DateLayout)})
	}
	own := []string{h.Layout.ScratchDir(producer.ID)}
	for _, w := range hand.Worktrees {
		own = append(own, w.Path)
	}
	userHome, _ := os.UserHomeDir()
	in.ReadRoots = evidenceReadRoots(in.Claims, roots, h.Layout.Home, userHome, own)
	return in
}

func (h *Host) precedents(c WitnessConfig) []precedent.Precedent {
	dir := c.Precedents
	switch dir {
	case NoPrecedents:
		return nil
	case "":
		dir = h.PrecedentsDir()
	}
	ps, errs := precedent.Load(dir)
	for _, err := range errs {
		log.Printf("precedents: %v", err)
	}
	return ps
}

// Markdown renders the input as the witness reads it. Stale precedents
// appear by id only: their text never reaches the witness.
func (in WitnessInput) Markdown() string {
	var b strings.Builder
	b.WriteString("# Witness input\n")
	if len(in.Artifacts) > 0 {
		fmt.Fprintf(&b, "\nThe claims come from: %s\n", strings.Join(in.Artifacts, ", "))
	}
	if len(in.Roots) > 0 {
		fmt.Fprintf(&b, "\nRelative evidence paths resolve against: %s\n", strings.Join(in.Roots, ", "))
	}
	if readable := append(slices.Clone(in.Roots), in.ReadRoots...); len(readable) > 0 {
		fmt.Fprintf(&b, "\nYou may open files under: %s. Reads anywhere else are blocked.\n", strings.Join(readable, ", "))
	} else {
		b.WriteString("\nYou may open no files beyond your own folder: judge from the excerpts.\n")
	}
	b.WriteString("\n## Claims\n")
	for _, c := range in.Claims {
		meta := []string{}
		if c.Kind != "" {
			meta = append(meta, c.Kind)
		}
		if c.Confidence != "" {
			meta = append(meta, "confidence "+c.Confidence)
		}
		fmt.Fprintf(&b, "\n### %s", c.ID)
		if len(meta) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(meta, ", "))
		}
		fmt.Fprintf(&b, "\n\n%s\n\nEvidence:", c.Text)
		if len(c.Evidence) == 0 {
			b.WriteString(" none given.")
		}
		b.WriteString("\n")
		for _, e := range c.Evidence {
			fmt.Fprintf(&b, "- `%s`\n", e.Ref)
			if e.Excerpt != "" {
				for _, l := range strings.Split(e.Excerpt, "\n") {
					fmt.Fprintf(&b, "  > %s\n", l)
				}
			}
		}
		if len(c.Precedents) > 0 {
			fmt.Fprintf(&b, "\nPrecedents in scope: %s\n", strings.Join(c.Precedents, ", "))
		}
		if len(c.Stale) > 0 {
			fmt.Fprintf(&b, "\nStale precedents in scope (do not apply): %s\n", strings.Join(c.Stale, ", "))
		}
	}
	if s := precedent.Format(in.fresh, maxPrecedents); s != "" {
		b.WriteString("\n" + s)
	}
	if len(in.StalePrecedents) > 0 {
		b.WriteString("\n## Stale precedents: flagged, do not apply\n\n")
		b.WriteString("These are past their review date, so they may no longer be true. Never apply or cite one. If one would have decided a claim, say so in that claim's note so the person reviews it.\n\n")
		for _, p := range in.StalePrecedents {
			fmt.Fprintf(&b, "- %s (review_by %s)\n", p.ID, p.ReviewBy)
		}
	}
	return b.String()
}

const witnessBrief = `You are a witness. Another agent made the claims below. Check each one against its evidence, with no other context. You did not write them and owe them nothing.

For every claim, call the verdict tool once:
- holds: you checked the evidence and it shows the claim as stated, all of it.
- contradicted: the evidence, or a precedent below, shows the claim is false or out of date.
- unverifiable: the evidence does not reach the claim. Code existing is not code running in production; a difference is not a misconfiguration unless intent is shown; a claim with no evidence, or about judgment or intent, is unverifiable.
The note is one or two sentences: what you checked and what decided it. List in precedent_ids every precedent you applied.

Evidence items are path:line, a URL or "cmd: ...". An excerpt under an item is what was read there, with the lines around it: each line starts with its number, and the lines the ref cites are marked with *. Treat it as the file's content. To see more, open files only in the folders the input says you may read; reads elsewhere are blocked, so don't try them. You may run read-only commands. Never edit, write, commit, or run anything that changes state.

Precedents are corrections the person already made. Apply one only where it bears on what the claim asserts: a precedent in scope does not make every claim on its subject wrong. If a precedent says it was inferred, say in the note that the person should confirm it. Stale precedents are listed by id only: never apply or cite them.

When every claim has a verdict, call done with one line counting the verdicts.`

// witnessPrompt is the witness's first prompt: the brief and the input,
// inline when it is small, else the path of its file.
func witnessPrompt(md, mdPath, jsonPath string) string {
	if len(md) <= witnessInline {
		return witnessBrief + "\n\nThe same input as JSON: " + jsonPath + "\n\n" + md
	}
	return witnessBrief + "\n\nYour input is in " + mdPath + ", too long to give here: read all of it first. The same input as JSON: " + jsonPath
}

// witnessJob is what the host knows about a witness it started.
type witnessJob struct {
	model        string
	fresh, stale map[string]bool
}

var (
	// witnessJobs maps a witness's agent id to its job while it runs.
	witnessJobs sync.Map
	// witnessFlights maps a producer's id to the witness run on its claims,
	// so a second Hold of one done waits on the first.
	witnessFlights sync.Map
	// witnessHolds maps a witness's agent id to its Hold's deadline while
	// the Hold waits on it.
	witnessHolds sync.Map
)

// witnessHoldMargin is how close to its Hold's deadline a quiet witness
// is left to the Hold rather than the stall watchdog: well over a sweep.
const witnessHoldMargin = 30 * time.Second

// heldByDeadline reports whether witness id is waited on by a Hold whose
// deadline has passed or is at most witnessHoldMargin after now. The Hold
// then ends the witness itself, with the note that says its timeout ran
// out, instead of racing the watchdog to it.
func heldByDeadline(id string, now time.Time) bool {
	d, ok := witnessHolds.Load(id)
	return ok && d.(time.Time).Sub(now) <= witnessHoldMargin
}

func isWitness(a store.Agent) bool { return a.Role == WitnessRole && a.ParentID != "" }

// witnessModel is the model of witness agent a.
func witnessModel(a store.Agent) string {
	if j, ok := witnessJobs.Load(a.ID); ok {
		return j.(*witnessJob).model
	}
	return DefaultWitnessModel
}

// RunWitness has one clean-context witness give a verdict on each claim of
// producer that has none, and returns once it has called done or ctx ends.
// Claims left without a verdict are marked unwitnessed. A second call for
// the same producer while one runs waits on it; a producer with no claims
// left to judge returns at once. given may be nil.
func (h *Host) RunWitness(ctx context.Context, c WitnessConfig, producer store.Agent, given *WitnessGiven) error {
	flight := make(chan struct{})
	if prev, loaded := witnessFlights.LoadOrStore(producer.ID, flight); loaded {
		select {
		case <-prev.(chan struct{}):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer func() {
		witnessFlights.Delete(producer.ID)
		close(flight)
	}()
	if p, err := h.Store.Agent(producer.ID); err == nil {
		producer = p
	}
	claims, err := h.Store.AgentClaims(producer.ID)
	if err != nil {
		return err
	}
	claims = slices.DeleteFunc(claims, func(cl store.Claim) bool { return cl.Verdict != "" })
	if len(claims) == 0 {
		return nil
	}
	wit, w, err := h.startWitness(ctx, c, producer, claims, given)
	if err != nil {
		note := "the witness could not start: " + err.Error()
		if ctx.Err() != nil {
			note = h.unwitnessedNote(ctx, c)
		}
		if _, merr := h.MarkUnwitnessed(producer.ID, note); merr != nil {
			err = errors.Join(err, merr)
		}
		return err
	}
	defer witnessJobs.Delete(wit.ID)
	if d, ok := ctx.Deadline(); ok {
		witnessHolds.Store(wit.ID, d)
		defer witnessHolds.Delete(wit.ID)
	}
	select {
	case <-w.handoff:
		// A witness the run's end, the watchdog or a person stopped hands
		// back too; its claims without a verdict say why.
		note := "the witness finished without a verdict on this claim"
		if ctx.Err() != nil {
			note = h.unwitnessedNote(ctx, c)
		} else if cur, err := h.Store.Agent(wit.ID); err == nil && cur.State != store.StateDone {
			note = fmt.Sprintf("the witness was stopped (%s) before a verdict on this claim", cmp.Or(cur.Reason, cur.State))
		}
		n, err := h.MarkUnwitnessed(producer.ID, note)
		h.emit(producer.RunID, producer.ID, store.KindWitnessFinished, store.WitnessFinished{V: store.EventVersion, WitnessID: wit.ID, Unwitnessed: n})
		return err
	case <-ctx.Done():
		h.stopWaiting(wit.ID)
		if _, err := h.Kill(wit.ID); err != nil && !errors.Is(err, store.ErrFinished) {
			log.Printf("witness %s: kill: %v", wit.ID, err)
		}
		_, err := h.MarkUnwitnessed(producer.ID, h.unwitnessedNote(ctx, c))
		return errors.Join(ctx.Err(), err)
	}
}

func (h *Host) unwitnessedNote(ctx context.Context, c WitnessConfig) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("no witness verdict within %s", c.timeout())
	}
	return "the run ended before the witness finished"
}

// startWitness starts the witness of claims, or picks up the one already
// running on producer's claims, as after a host restart, and waits on it.
func (h *Host) startWitness(ctx context.Context, c WitnessConfig, producer store.Agent, claims []store.Claim, given *WitnessGiven) (store.Agent, *spawnWait, error) {
	agents, err := h.Store.Agents()
	if err != nil {
		return store.Agent{}, nil, err
	}
	for _, a := range agents {
		if a.ParentID != producer.ID || !isWitness(a) || store.Finished(a.State) {
			continue
		}
		w := h.wait(a.ID, producer.ID, WitnessRole)
		if cur, err := h.Store.Agent(a.ID); err == nil && !store.Finished(cur.State) {
			log.Printf("witness %s: picked up for %s", a.ID, producer.ID)
			return cur, w, nil
		}
		h.stopWaiting(a.ID)
	}

	id := agent.NewSessionID()
	scratch := h.Layout.ScratchDir(id)
	dir := scratch
	if producer.Handoff != nil && len(producer.Handoff.Worktrees) > 0 {
		if fi, err := os.Stat(producer.Handoff.Worktrees[0].Path); err == nil && fi.IsDir() {
			dir = producer.Handoff.Worktrees[0].Path
		}
	}
	in := h.witnessInput(c, producer, claims, given, time.Now())
	md := in.Markdown()
	body, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return store.Agent{}, nil, err
	}
	mdPath, jsonPath := filepath.Join(scratch, "witness-input.md"), filepath.Join(scratch, "witness-input.json")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return store.Agent{}, nil, err
	}
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		return store.Agent{}, nil, err
	}
	if err := os.WriteFile(jsonPath, body, 0o644); err != nil {
		return store.Agent{}, nil, err
	}
	job := &witnessJob{model: c.model(), fresh: map[string]bool{}, stale: map[string]bool{}}
	for _, p := range in.Precedents {
		job.fresh[p.ID] = true
	}
	for _, p := range in.StalePrecedents {
		job.stale[p.ID] = true
	}
	witnessJobs.Store(id, job)
	w := h.wait(id, producer.ID, WitnessRole)
	a, err := h.launch(ctx, store.Agent{
		ID: id, Dir: dir, Prompt: witnessPrompt(md, mdPath, jsonPath), ParentID: producer.ID, Role: WitnessRole,
		RunID: producer.RunID, Step: producer.Step, Headless: true,
	}, append(witnessRoots(producer), in.ReadRoots...)...)
	if err != nil {
		h.stopWaiting(id)
		witnessJobs.Delete(id)
		return store.Agent{}, nil, err
	}
	stale := make([]string, len(in.StalePrecedents))
	for i, p := range in.StalePrecedents {
		stale[i] = p.ID
	}
	if len(stale) > 0 {
		h.emit(producer.RunID, producer.ID, store.KindPrecedentStale, store.PrecedentIDs{V: store.EventVersion, IDs: stale, WitnessID: id})
	}
	fresh := make([]string, len(in.Precedents))
	for i, p := range in.Precedents {
		fresh[i] = p.ID
	}
	h.emit(producer.RunID, producer.ID, store.KindWitnessStarted, store.WitnessStarted{
		V: store.EventVersion, WitnessID: id, Claims: len(claims), Precedents: fresh, StalePrecedents: stale,
	})
	return a, w, nil
}

// VerdictPart is the kind of the part the next step sees beside its
// handoff's parts once a witness has judged the claims: what is wrong
// reaches it before it builds on it.
const VerdictPart = "verdicts"

type partVerdict struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// withVerdicts is parts plus a verdicts part when any claim of agentID has
// a verdict.
func (h *Host) withVerdicts(agentID string, parts []store.Part) []store.Part {
	cs, err := h.Store.AgentClaims(agentID)
	if err != nil {
		log.Printf("verdicts of %s: %v", agentID, err)
		return parts
	}
	var vs []partVerdict
	for _, c := range cs {
		if c.Verdict != "" {
			vs = append(vs, partVerdict{ID: c.Key, Verdict: c.Verdict, Note: c.VerdictNote})
		}
	}
	if len(vs) == 0 {
		return parts
	}
	body, err := json.Marshal(map[string]any{"verdicts": vs})
	if err != nil {
		return parts
	}
	return append(slices.Clone(parts), store.Part{Kind: VerdictPart, Body: body})
}

// VerdictInput is one witness verdict on one claim.
type VerdictInput struct {
	ClaimID      string   `json:"claim_id" jsonschema:"the claim's id, as your input lists it"`
	Verdict      string   `json:"verdict" jsonschema:"holds, contradicted or unverifiable"`
	Note         string   `json:"note" jsonschema:"one or two sentences: what you checked and what decided it"`
	PrecedentIDs []string `json:"precedent_ids,omitempty" jsonschema:"the precedents you applied, by id; never a stale one"`
}

// witnessVerdicts are the verdicts a witness may give; unwitnessed is the
// host's.
var witnessVerdicts = []string{store.VerdictHolds, store.VerdictContradicted, store.VerdictUnverifiable}

func (h *Host) verdict(witnessID string, in VerdictInput) (*mcp.CallToolResult, any, error) {
	wit, err := h.Store.Agent(witnessID)
	if err != nil {
		return nil, nil, err
	}
	in.Verdict = strings.ToLower(strings.TrimSpace(in.Verdict))
	if !slices.Contains(witnessVerdicts, in.Verdict) {
		return nil, nil, fmt.Errorf("verdict %q is not one of %s", in.Verdict, strings.Join(witnessVerdicts, ", "))
	}
	in.Note = strings.TrimSpace(in.Note)
	if in.Note == "" {
		return nil, nil, errors.New("no note: say in a sentence what you checked and what decided it")
	}
	claims, err := h.Store.AgentClaims(wit.ParentID)
	if err != nil {
		return nil, nil, err
	}
	i := slices.IndexFunc(claims, func(c store.Claim) bool { return c.Key == in.ClaimID || c.ID == in.ClaimID })
	if i < 0 {
		return nil, nil, fmt.Errorf("no claim %q among the ones you were given", in.ClaimID)
	}
	if j, ok := witnessJobs.Load(witnessID); ok {
		job := j.(*witnessJob)
		for _, p := range in.PrecedentIDs {
			switch {
			case job.stale[p]:
				return nil, nil, fmt.Errorf("precedent %s is stale: do not apply it. Judge the claim without it, and say in the note that %s would bear on it if it were still true", p, p)
			case !job.fresh[p]:
				return nil, nil, fmt.Errorf("precedent %s was not among the ones you were given", p)
			}
		}
	}
	note := in.Note
	if len(in.PrecedentIDs) > 0 {
		note += " [precedents: " + strings.Join(in.PrecedentIDs, ", ") + "]"
	}
	if err := h.SetVerdict(claims[i].ID, in.Verdict, note, witnessID); err != nil {
		return nil, nil, err
	}
	var left []string
	for k, c := range claims {
		if k != i && c.Verdict == "" {
			left = append(left, c.Key)
		}
	}
	if len(left) == 0 {
		return textResult("Recorded. Every claim has a verdict: call done."), nil, nil
	}
	return textResult(fmt.Sprintf("Recorded. Still without a verdict: %s.", strings.Join(left, ", "))), nil, nil
}

func init() {
	extraTools = append(extraTools, func(h *Host, s *mcp.Server, id string, a store.Agent) {
		if !isWitness(a) {
			return
		}
		s.RemoveTools("spawn", "await")
		mcp.AddTool(s, &mcp.Tool{
			Name:        "verdict",
			Description: "Record your verdict on one claim: holds, contradicted or unverifiable, with a short note. Call it once per claim, then call done.",
		}, func(ctx context.Context, req *mcp.CallToolRequest, in VerdictInput) (*mcp.CallToolResult, any, error) {
			return h.verdict(id, in)
		})
	})
}
