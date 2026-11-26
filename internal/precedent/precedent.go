// Package precedent reads the corrections a person already made once, so
// agents and witnesses get them before they repeat the mistake. Each
// precedent is a markdown file under $AEOLIAN_HOME/precedents with YAML
// frontmatter, the same shape as an ekky intent card, so it can be edited
// in any notes app and synced across machines:
//
//	---
//	id: p01-dev-ledger-project-branch   # the file name without .md
//	kind: correction                    # correction, convention or safety
//	scope: [dev-ledger, branch]         # every term must appear (AND)
//	review_by: 2027-01-09               # stale after this day
//	source: "where the correction came from"
//	confidence: stated                  # stated or inferred; optional
//	---
//	The correction itself, in markdown.
//
// A stale precedent is never applied silently: Match hands it back apart,
// so the caller can flag it to the person instead.
package precedent

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Kinds a precedent may have.
const (
	KindCorrection = "correction" // a fact the person corrected
	KindConvention = "convention" // a writing or process rule
	KindSafety     = "safety"     // something an agent must never do
)

var Kinds = []string{KindCorrection, KindConvention, KindSafety}

// Confidences a precedent may state. Inferred means someone other than the
// person summarized it, so whoever applies it should ask them to confirm.
const (
	ConfidenceStated   = "stated"
	ConfidenceInferred = "inferred"
)

var Confidences = []string{ConfidenceStated, ConfidenceInferred}

const Any = "*"

// DateLayout is the layout of review_by.
const DateLayout = "2006-01-02"

// Precedent is one correction, as its file says.
type Precedent struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Scope      []string  `json:"scope"`
	Roles      []string  `json:"roles,omitempty"`
	Repos      []string  `json:"repos,omitempty"`
	ReviewBy   time.Time `json:"review_by"`
	Source     string    `json:"source"`
	Confidence string    `json:"confidence,omitempty"`
	Body       string    `json:"body"`
	Path       string    `json:"path"`
}

// Dir is where the precedents of the aeolian home live.
func Dir(home string) string { return filepath.Join(home, "precedents") }

type frontmatter struct {
	ID         string   `yaml:"id"`
	Kind       string   `yaml:"kind"`
	Scope      []string `yaml:"scope"`
	Roles      []string `yaml:"roles"`
	Repos      []string `yaml:"repos"`
	ReviewBy   string   `yaml:"review_by"`
	Source     string   `yaml:"source"`
	Confidence string   `yaml:"confidence"`
}

// Parse reads one precedent file's contents; path names it in errors and
// gives the id it must carry. Keys it does not know are ignored, so an
// intent card with more fields still reads.
func Parse(path string, b []byte) (Precedent, error) {
	fail := func(format string, args ...any) (Precedent, error) {
		return Precedent{}, fmt.Errorf("precedent %s: %s", path, fmt.Sprintf(format, args...))
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	rest, ok := bytes.CutPrefix(b, []byte("---\n"))
	if !ok {
		return fail("no frontmatter: the file must start with a --- line")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		head, ok = bytes.CutSuffix(rest, []byte("\n---"))
		if !ok {
			return fail("frontmatter is not closed by a --- line")
		}
	}
	var fm frontmatter
	if err := yaml.Unmarshal(head, &fm); err != nil {
		if bytes.Contains(head, []byte("[*")) {
			return fail("frontmatter: %v (quote the wildcard: scope: [\"*\"])", err)
		}
		return fail("frontmatter: %v", err)
	}
	p := Precedent{
		ID: strings.TrimSpace(fm.ID), Kind: strings.TrimSpace(fm.Kind), Source: strings.TrimSpace(fm.Source),
		Confidence: strings.TrimSpace(fm.Confidence), Body: strings.TrimSpace(string(body)), Path: path,
	}
	want := strings.TrimSuffix(filepath.Base(path), ".md")
	switch {
	case p.ID == "":
		return fail("no id")
	case p.ID != want:
		return fail("id %q is not the file name %q", p.ID, want)
	case !slices.Contains(Kinds, p.Kind):
		return fail("kind %q is not one of %s", p.Kind, strings.Join(Kinds, ", "))
	case p.Confidence != "" && !slices.Contains(Confidences, p.Confidence):
		return fail("confidence %q is not one of %s", p.Confidence, strings.Join(Confidences, ", "))
	case len(fm.Scope) == 0:
		return fail(`no scope: list the terms that must all appear for it to apply, or ["*"] for none`)
	case p.Source == "":
		return fail("no source: say where the correction came from")
	case p.Body == "":
		return fail("no body: write the correction under the frontmatter")
	}
	for _, term := range fm.Scope {
		term = strings.ToLower(strings.TrimSpace(term))
		if term == "" {
			return fail("an empty scope term")
		}
		p.Scope = append(p.Scope, term)
	}
	if slices.Contains(p.Scope, Any) && len(p.Scope) > 1 {
		return fail(`scope "*" must stand alone: it means no word has to appear`)
	}
	var err error
	if p.Roles, err = names("roles", fm.Roles); err != nil {
		return fail("%v", err)
	}
	if p.Repos, err = names("repos", fm.Repos); err != nil {
		return fail("%v", err)
	}
	if fm.ReviewBy == "" {
		return fail("no review_by: give the day (%s) to re-check it by", DateLayout)
	}
	t, err := time.Parse(DateLayout, strings.TrimSpace(fm.ReviewBy))
	if err != nil {
		return fail("review_by %q is not a %s date", fm.ReviewBy, DateLayout)
	}
	p.ReviewBy = t
	return p, nil
}

func names(field string, raw []string) ([]string, error) {
	var out []string
	for _, n := range raw {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || n == Any {
			return nil, fmt.Errorf("%s: %q is not a name", field, n)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// Load reads every *.md precedent in dir, sorted by id. A file that does
// not parse is left out and its error returned beside the rest, so one bad
// file never hides the others. A missing dir has no precedents. README.md
// is skipped.
func Load(dir string) ([]Precedent, []error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []error{err}
	}
	var ps []Precedent
	var errs []error
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.EqualFold(name, "README.md") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(dir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p, err := Parse(path, b)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		key := strings.ToLower(p.ID)
		if seen[key] {
			errs = append(errs, fmt.Errorf("precedent %s: id %q is taken", path, p.ID))
			continue
		}
		seen[key] = true
		ps = append(ps, p)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	return ps, errs
}

// Stale reports whether now is past the precedent's review_by day.
func (p Precedent) Stale(now time.Time) bool {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return today.After(p.ReviewBy)
}

func (p Precedent) Wild() bool { return len(p.Scope) == 1 && p.Scope[0] == Any }

func (p Precedent) Global() bool { return p.Wild() && len(p.Roles) == 0 && len(p.Repos) == 0 }

func (p Precedent) Specificity() int {
	n := len(p.Scope)
	if p.Wild() {
		n = 0
	}
	if len(p.Roles) > 0 {
		n++
	}
	if len(p.Repos) > 0 {
		n++
	}
	return n
}

// Matches reports whether every scope term appears in text as a word,
// case-insensitively. A term may carry a plural s or es (branch matches
// branches) but never runs into a longer word (prod does not match
// product).
func (p Precedent) Matches(text string) bool {
	if p.Wild() || len(p.Roles) > 0 || len(p.Repos) > 0 {
		return false
	}
	return p.matchesWords(text)
}

func (p Precedent) matchesWords(text string) bool {
	if p.Wild() {
		return true
	}
	text = strings.ToLower(text)
	for _, term := range p.Scope {
		if !hasWord(text, term) {
			return false
		}
	}
	return len(p.Scope) > 0
}

type Target struct {
	Role  string
	Repos []string
	Text  string
}

func (p Precedent) Reaches(t Target) bool {
	if len(p.Roles) > 0 && !slices.Contains(p.Roles, strings.ToLower(strings.TrimSpace(t.Role))) {
		return false
	}
	if len(p.Repos) > 0 && !slices.ContainsFunc(t.Repos, func(r string) bool { return slices.Contains(p.Repos, strings.ToLower(r)) }) {
		return false
	}
	return p.matchesWords(t.Text)
}

func hasWord(text, term string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], term)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(term)
		if boundaryBefore(text, start) {
			rest := text[end:]
			for _, suffix := range []string{"", "s", "es"} {
				if strings.HasPrefix(rest, suffix) && boundaryAfter(text, end+len(suffix)) {
					return true
				}
			}
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		from = start + size
	}
}

func boundaryBefore(text string, i int) bool {
	if i == 0 {
		return true
	}
	r, _ := utf8.DecodeLastRuneInString(text[:i])
	return !isWordRune(r)
}

func boundaryAfter(text string, i int) bool {
	if i >= len(text) {
		return true
	}
	r, _ := utf8.DecodeRuneInString(text[i:])
	return !isWordRune(r)
}

func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// Matched is what Match found for one text: Fresh precedents to apply, and
// Stale ones that matched but are past review_by and must be flagged to the
// person rather than applied.
type Matched struct {
	Fresh []Precedent
	Stale []Precedent
}

// Match picks the precedents whose scope matches the text of parts joined,
// typically a claim and its evidence, and sorts them fresh or stale as of
// now.
func Match(ps []Precedent, now time.Time, parts ...string) Matched {
	text := strings.Join(parts, "\n")
	return sortOut(ps, now, func(p Precedent) bool { return p.Matches(text) })
}

func MatchTarget(ps []Precedent, now time.Time, t Target) Matched {
	return sortOut(ps, now, func(p Precedent) bool { return p.Reaches(t) })
}

func sortOut(ps []Precedent, now time.Time, keep func(Precedent) bool) Matched {
	var m Matched
	for _, p := range ps {
		if !keep(p) {
			continue
		}
		if p.Stale(now) {
			m.Stale = append(m.Stale, p)
		} else {
			m.Fresh = append(m.Fresh, p)
		}
	}
	return m
}

var kindRank = map[string]int{KindSafety: 0, KindCorrection: 1, KindConvention: 2}

func Select(ps []Precedent, max, globalSlots int) []Precedent {
	var targeted, global []Precedent
	for _, p := range ps {
		if p.Global() {
			global = append(global, p)
		} else {
			targeted = append(targeted, p)
		}
	}
	byKind := func(a, b Precedent) int {
		if d := kindRank[a.Kind] - kindRank[b.Kind]; d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	}
	slices.SortStableFunc(targeted, func(a, b Precedent) int {
		if d := b.Specificity() - a.Specificity(); d != 0 {
			return d
		}
		return byKind(a, b)
	})
	slices.SortStableFunc(global, byKind)
	if max <= 0 {
		return append(targeted, global...)
	}
	g := min(len(global), globalSlots, max)
	t := min(len(targeted), max-g)
	g = min(len(global), max-t)
	return append(targeted[:t:t], global[:g]...)
}

// Heading opens the block Format writes.
const Heading = "## Precedents: corrections the person already made"

// Format renders fresh precedents as a prompt block, at most max of them
// (max <= 0 means all), each with its id so whoever applies one can cite
// it. It returns "" when there are none.
func Format(ps []Precedent, max int) string {
	if len(ps) == 0 {
		return ""
	}
	if max > 0 && len(ps) > max {
		ps = ps[:max]
	}
	var b strings.Builder
	b.WriteString(Heading + "\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "\n### %s (%s)\n\n%s\n", p.ID, p.Kind, p.Body)
		if p.Confidence == ConfidenceInferred {
			b.WriteString("\nThis one was inferred, not stated by the person: say so if you rely on it, and ask them to confirm.\n")
		}
	}
	return b.String()
}
