package workflow

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"unicode/utf16"
)

// Tunings are the waveforms a step or role may name; the map draws its
// identity in one of them. Keep in step with TUNINGS in
// map/src/renderer/src/marks.ts.
var Tunings = []string{"sine", "tri", "saw", "square", "pulse", "arch", "step", "grain"}

func checkTuning(name, tuning, mark string) error {
	if tuning == "" {
		return nil
	}
	if !slices.Contains(Tunings, tuning) {
		return fmt.Errorf("workflow: %s has tuning %q; allowed: %s", name, tuning, strings.Join(Tunings, ", "))
	}
	if mark != "" {
		return fmt.Errorf("workflow: %s has both mark and tuning; tuning replaces mark, so keep only tuning", name)
	}
	return nil
}

// HashTuning is the tuning the map draws for key when nothing picked one:
// a tuning name is itself, any other name is FNV-1a over its UTF-16 code
// units mod 8, as tuning() in marks.ts does. No name is the open string, "".
func HashTuning(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return ""
	}
	if slices.Contains(Tunings, k) {
		return k
	}
	h := uint32(0x811c9dc5)
	for _, c := range utf16.Encode([]rune(k)) {
		h = (h ^ uint32(c)) * 0x01000193
	}
	return Tunings[h%uint32(len(Tunings))]
}

// FreeInk is the ink of a helper whose role the workflow does not define.
func FreeInk(role string) string {
	h := fnv.New32a()
	h.Write([]byte(role))
	return Inks[h.Sum32()%uint32(len(Inks))]
}

// Look is how the map draws a step or a role: an ink and a tuning.
type Look struct {
	Ink    string
	Tuning string
}

type want struct {
	steps    bool
	name     string
	ink      string // the usual ink; "" is plain
	inkFixed bool
	tuning   string
	tunFixed bool
}

// Looks gives every step and role of one run its own (ink, tuning) pair, so
// no two look alike while the 40 pairs last. free are the roles of helpers
// the workflow does not define, in start order. An ink or tuning the file
// names is kept as is. Otherwise a step or role keeps its usual ink (the
// file's, plain, or FreeInk for a free role) and its hashed tuning, and on a
// clash walks the tunings first, then the other inks. Steps with a tuning
// claim first, then the rest in file order, then roles by name, then free
// roles, so a helper starting later never moves an earlier look. A role
// with no name gets no look: the map draws it as the open string.
func (w *Workflow) Looks(free []string) (steps, roles map[string]Look) {
	var ws []want
	for _, st := range w.Steps {
		ws = append(ws, want{steps: true, name: st.Name, ink: st.Ink, inkFixed: st.Ink != "",
			tuning: pref(st.Tuning, st.Mark, st.Name), tunFixed: st.Tuning != ""})
	}
	names := make([]string, 0, len(w.Roles))
	for n := range w.Roles {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		r := w.Roles[n]
		ws = append(ws, want{name: n, ink: r.Ink, inkFixed: r.Ink != "",
			tuning: pref(r.Tuning, r.Mark, n), tunFixed: r.Tuning != ""})
	}
	for _, n := range free {
		if _, ok := w.Roles[n]; !ok && n != "" {
			ws = append(ws, want{name: n, ink: FreeInk(n), tuning: HashTuning(n)})
		}
	}
	slices.SortStableFunc(ws, func(a, b want) int {
		switch {
		case a.tunFixed == b.tunFixed:
			return 0
		case a.tunFixed:
			return -1
		}
		return 1
	})
	steps, roles = map[string]Look{}, map[string]Look{}
	taken := map[Look]bool{}
	for _, x := range ws {
		m := roles
		if x.steps {
			m = steps
		}
		if _, done := m[x.name]; done || x.name == "" {
			continue
		}
		l := pick(x, taken)
		taken[l] = true
		m[x.name] = l
	}
	return steps, roles
}

func pref(tuning, mark, name string) string {
	if tuning != "" {
		return tuning
	}
	if mark != "" {
		return HashTuning(mark)
	}
	return HashTuning(name)
}

func pick(x want, taken map[Look]bool) Look {
	inks := []string{x.ink}
	if !x.inkFixed {
		all := append([]string{""}, Inks...)
		i := slices.Index(all, x.ink)
		for k := 1; k < len(all); k++ {
			inks = append(inks, all[(i+k)%len(all)])
		}
	}
	tuns := []string{x.tuning}
	if !x.tunFixed {
		i := slices.Index(Tunings, x.tuning)
		for k := 1; k < len(Tunings); k++ {
			tuns = append(tuns, Tunings[(i+k)%len(Tunings)])
		}
	}
	for _, ink := range inks {
		for _, t := range tuns {
			if l := (Look{ink, t}); !taken[l] {
				return l
			}
		}
	}
	return Look{inks[0], tuns[0]}
}
