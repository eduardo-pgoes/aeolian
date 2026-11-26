package workflow

import (
	"testing"
)

func TestTuningParses(t *testing.T) {
	w, err := Parse([]byte("steps: [{name: a, prompt: p, tuning: sine}, {name: b, prompt: p, mark: hammer}]\nroles: {r: {prompt: p, tuning: grain}}"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Steps[0].Tuning != "sine" || w.Steps[1].Mark != "hammer" || w.Roles["r"].Tuning != "grain" {
		t.Fatalf("parsed: %+v %+v", w.Steps, w.Roles)
	}
	for src, want := range map[string]string{
		"steps: [{name: a, prompt: p, tuning: noise}]":                         `workflow: step a has tuning "noise"; allowed: sine, tri, saw, square, pulse, arch, step, grain`,
		"steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, tuning: Sine}}": `workflow: role r has tuning "Sine"; allowed: sine, tri, saw, square, pulse, arch, step, grain`,
		"steps: [{name: a, prompt: p, tuning: saw, mark: hammer}]":             "workflow: step a has both mark and tuning; tuning replaces mark, so keep only tuning",
	} {
		if _, err := Parse([]byte(src)); err == nil || err.Error() != want {
			t.Errorf("%s: got %v, want %s", src, err, want)
		}
	}
}

// The values come from tuning() in map/src/renderer/src/marks.ts, so a
// look the host picks is the one the map drew before tunings were picked.
func TestHashTuningMatchesMap(t *testing.T) {
	for key, want := range map[string]string{
		"researcher": "square", "planner": "square", "linter": "square",
		"review": "tri", "hammer": "grain", "eye": "saw", "✿ anything": "sine",
		" Hammer ": "grain", "SAW": "saw", "": "", "  ": "",
	} {
		if got := HashTuning(key); got != want {
			t.Errorf("%q: %q, want %q", key, got, want)
		}
	}
}

func looks(t *testing.T, src string, free ...string) (map[string]Look, map[string]Look) {
	t.Helper()
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return w.Looks(free)
}

func TestLooksSplitClashingRoles(t *testing.T) {
	// all three hash to square in sky; file order of roles is by name
	steps, roles := looks(t, `
steps: [{name: a, prompt: p, mark: review, helpers: ["*"]}]
roles:
  researcher: {prompt: p, ink: sky}
  planner: {prompt: p, ink: sky}
  linter: {prompt: p, ink: sky}
`)
	want := map[string]Look{"linter": {"sky", "square"}, "planner": {"sky", "pulse"}, "researcher": {"sky", "arch"}}
	for n, l := range want {
		if roles[n] != l {
			t.Errorf("%s: %+v, want %+v", n, roles[n], l)
		}
	}
	if steps["a"] != (Look{"", "tri"}) {
		t.Errorf("step: %+v", steps["a"])
	}
}

func TestLooksKeepWhatTheFileNames(t *testing.T) {
	// b names sine, so it claims first; a keeps its sakura but leaves sine.
	steps, _ := looks(t, `
steps:
  - {name: a, prompt: p, ink: sakura, mark: sine}
  - {name: b, prompt: p, ink: sakura, tuning: sine}
  - {name: c, prompt: p, tuning: sine}
`)
	if steps["b"] != (Look{"sakura", "sine"}) || steps["a"] != (Look{"sakura", "tri"}) || steps["c"] != (Look{"", "sine"}) {
		t.Fatalf("steps: %+v", steps)
	}
}

func TestLooksWalkInksWhenTuningsRunOut(t *testing.T) {
	// nine plain steps that all ask for sine: eight tunings in plain, then
	// the ninth moves to the first ink.
	src := "steps:\n"
	for _, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"} {
		src += "  - {name: " + n + ", prompt: p, mark: sine}\n"
	}
	steps, _ := looks(t, src)
	seen := map[Look]bool{}
	for _, l := range steps {
		if seen[l] {
			t.Fatalf("two steps share %+v: %+v", l, steps)
		}
		seen[l] = true
	}
	if steps["s1"] != (Look{"", "sine"}) || steps["s8"] != (Look{"", "grain"}) || steps["s9"] != (Look{"sakura", "sine"}) {
		t.Fatalf("steps: %+v", steps)
	}
	// a fixed ink with no tunings left keeps its own pick rather than move
	src = "steps:\n"
	for _, n := range []string{"s1", "s2", "s3", "s4", "s5", "s6", "s7", "s8", "s9"} {
		src += "  - {name: " + n + ", prompt: p, ink: kaki, mark: sine}\n"
	}
	steps, _ = looks(t, src)
	if steps["s9"] != (Look{"kaki", "sine"}) {
		t.Fatalf("fixed ink: %+v", steps["s9"])
	}
}

func TestLooksFreeRoles(t *testing.T) {
	_, roles := looks(t, `
steps: [{name: a, prompt: p, helpers: ["*", r]}]
roles: {r: {prompt: p}}
`, "researcher", "r", "", "researcher", "planner")
	if len(roles) != 3 {
		t.Fatalf("roles: %+v", roles)
	}
	if _, ok := roles[""]; ok {
		t.Fatal("a helper with no role got a look; the map draws it as the open string")
	}
	r, p := roles["researcher"], roles["planner"]
	if r.Ink != FreeInk("researcher") || r.Tuning != "square" || p.Ink != FreeInk("planner") {
		t.Fatalf("free roles: %+v", roles)
	}
	if r == p {
		t.Fatalf("free roles look alike: %+v", roles)
	}
	// a free role that starts later does not move an earlier look
	_, again := looks(t, `
steps: [{name: a, prompt: p, helpers: ["*", r]}]
roles: {r: {prompt: p}}
`, "researcher", "planner", "linter")
	if again["researcher"] != r || again["planner"] != p {
		t.Fatalf("looks moved: %+v then %+v", roles, again)
	}
}
