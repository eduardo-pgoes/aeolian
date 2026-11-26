package precedent

import (
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

	"gopkg.in/yaml.v3"
)

// DefaultReviewAfter is how long a new precedent holds before it is due a
// re-check, when its author gives no review_by.
const DefaultReviewAfter = 60 * 24 * time.Hour

// Draft is a precedent before it has a file. ID is optional: Write picks
// one from the body when it is empty.
type Draft struct {
	ID         string    `json:"id,omitempty"`
	Kind       string    `json:"kind"`
	Scope      []string  `json:"scope"`
	Roles      []string  `json:"roles,omitempty"`
	Repos      []string  `json:"repos,omitempty"`
	ReviewBy   time.Time `json:"review_by"`
	Source     string    `json:"source"`
	Confidence string    `json:"confidence,omitempty"`
	Body       string    `json:"body"`
}

// Render is the file a draft becomes, with the id it carries.
func Render(d Draft) []byte {
	fm := frontmatter{
		ID: d.ID, Kind: d.Kind, Scope: d.Scope, Roles: d.Roles, Repos: d.Repos, ReviewBy: d.ReviewBy.Format(DateLayout),
		Source: d.Source, Confidence: d.Confidence,
	}
	head, _ := yaml.Marshal(fmScoped{fm})
	return []byte("---\n" + string(head) + "---\n" + strings.TrimSpace(d.Body) + "\n")
}

// fmScoped marshals the frontmatter with scope as one flow list, the way a
// person writes it by hand.
type fmScoped struct{ f frontmatter }

func (s fmScoped) MarshalYAML() (any, error) {
	n := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k string, v *yaml.Node) {
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: k}, v)
	}
	str := func(v string) *yaml.Node {
		var x yaml.Node
		x.Encode(v)
		return &x
	}
	add("id", str(s.f.ID))
	add("kind", str(s.f.Kind))
	list := func(vs []string) *yaml.Node {
		l := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, v := range vs {
			n := str(v)
			if v == Any {
				n.Style = yaml.DoubleQuotedStyle
			}
			l.Content = append(l.Content, n)
		}
		return l
	}
	add("scope", list(s.f.Scope))
	if len(s.f.Roles) > 0 {
		add("roles", list(s.f.Roles))
	}
	if len(s.f.Repos) > 0 {
		add("repos", list(s.f.Repos))
	}
	add("review_by", &yaml.Node{Kind: yaml.ScalarNode, Value: s.f.ReviewBy})
	add("source", str(s.f.Source))
	if s.f.Confidence != "" {
		add("confidence", str(s.f.Confidence))
	}
	return n, nil
}

// Write stores a draft as a new file in dir and returns the precedent as
// Load would read it. It defaults the kind to correction and review_by to
// DefaultReviewAfter from now, picks an id from the body when there is
// none, and never overwrites a file: a taken id gets a -2, -3... suffix.
// The draft is checked with Parse before anything is written.
func Write(dir string, d Draft, now time.Time) (Precedent, error) {
	if d.Kind == "" {
		d.Kind = KindCorrection
	}
	if d.ReviewBy.IsZero() {
		d.ReviewBy = now.Add(DefaultReviewAfter)
	}
	d.Scope, d.Roles, d.Repos = tidy(d.Scope), tidy(d.Roles), tidy(d.Repos)
	d.Source = strings.TrimSpace(d.Source)
	d.Body = strings.TrimSpace(d.Body)
	base := d.ID
	if base == "" {
		base = "p" + now.Format("20060102") + "-" + Slug(d.Body, 6)
	} else if Slug(base, 20) != base {
		return Precedent{}, fmt.Errorf("id %q may hold only lowercase letters, digits and dashes", base)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Precedent{}, err
	}
	for n := 1; n < 100; n++ {
		d.ID = base
		if n > 1 {
			d.ID = fmt.Sprintf("%s-%d", base, n)
		}
		path := filepath.Join(dir, d.ID+".md")
		b := Render(d)
		p, err := Parse(path, b)
		if err != nil {
			return Precedent{}, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return Precedent{}, err
		}
		_, werr := f.Write(b)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return Precedent{}, werr
		}
		return p, nil
	}
	return Precedent{}, fmt.Errorf("no free id for %q in %s", base, dir)
}

func tidy(vs []string) []string {
	var out []string
	for _, v := range vs {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Slug is the first words of text, lowercased and joined by dashes, at most
// words of them; "precedent" when text has none.
func Slug(text string, words int) string {
	f := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)))
	})
	if len(f) > words {
		f = f[:words]
	}
	if len(f) == 0 {
		return "precedent"
	}
	return strings.Join(f, "-")
}

// SuggestScope picks up to n terms from text to scope a precedent drawn
// from it. Names come first: words that look like identifiers (dev-ledger,
// pr_42, v1.2) or are capitalized mid-sentence (Atlas). Only when fewer
// than two names turn up is it topped up to two with the longest
// uncommon words. Every term must match for the precedent to apply, so
// the scope comes out narrow; the person widens it by editing the file.
func SuggestScope(text string, n int) []string {
	f := strings.FieldsFunc(text, func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.')
	})
	type word struct {
		w     string
		pos   int
		named bool
	}
	var names, rest []word
	seen := map[string]bool{}
	for i, raw := range f {
		raw = strings.Trim(raw, "-_.")
		w := strings.ToLower(raw)
		if len([]rune(w)) < 3 || seen[w] || commonWords[w] {
			continue
		}
		seen[w] = true
		ident := strings.ContainsAny(w, "-_.0123456789")
		capital := i > 0 && raw != "" && unicode.IsUpper([]rune(raw)[0])
		switch {
		case ident || capital:
			names = append(names, word{w, i, true})
		case len([]rune(w)) >= 4:
			rest = append(rest, word{w, i, false})
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return len(rest[i].w) > len(rest[j].w) })
	ws := names
	if len(ws) == 0 {
		ws = firstWords(rest, 2)
	}
	if len(ws) > n {
		ws = ws[:n]
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].pos < ws[j].pos })
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.w
	}
	return out
}

func firstWords[T any](s []T, n int) []T {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var commonWords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`about above after again against because been before being below between
	both could does doing down during each from further have having here into itself just more most other
	over same should some such than that their them then there these they this those through under until very
	were what when where which while will with would your yours also only always never every anything nothing
	something there's isn't wasn't aren't doesn't don't didn't cannot instead still since across within without
	using used uses make makes made need needs like likely seems seem appears actually really current currently
	agent claim claims claimed says said shows shown found file files code line lines`) {
		commonWords[w] = true
	}
}
