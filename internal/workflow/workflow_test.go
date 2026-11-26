package workflow

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const twoSteps = `
steps:
  - name: build
    prompt: "Build {{.Goal}}."
    helpers: [reviewer]
  - name: check
    prompt: |
      Check {{.Goal}}. {{.Handoff}}
      {{range .Worktrees}}- {{.Path}} on {{.Branch}}
      {{end}}
roles:
  reviewer:
    prompt: "Review {{.Handoff}} at {{(index .Worktrees 0).Path}}. {{.ExtraInstructions}}"
  tester:
    prompt: "Test."
`

func TestRenderPrompts(t *testing.T) {
	w, err := Parse([]byte(twoSteps))
	if err != nil {
		t.Fatal(err)
	}
	if w.StepIndex("check") != 1 || w.StepIndex("nope") != -1 {
		t.Fatal("StepIndex")
	}
	wts := []store.Worktree{{Repo: "/r", Path: "/r/wt", Branch: "fix"}}
	got, err := w.StepPrompt(1, Data{Goal: "the parser", Handoff: "it builds", Worktrees: wts})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Check the parser. it builds", "- /r/wt on fix"} {
		if !strings.Contains(got, want) {
			t.Fatalf("step prompt lacks %q: %q", want, got)
		}
	}
	got, err = w.RolePrompt("build", "reviewer", Data{Handoff: "the fix", Worktrees: wts, ExtraInstructions: "be brief"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "Review the fix at /r/wt. be brief" {
		t.Fatalf("role prompt: %q", got)
	}
}

func TestRoleNotAllowed(t *testing.T) {
	w, err := Parse([]byte(twoSteps))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ step, role, want string }{
		{"build", "tester", `role "tester" not allowed in step "build"; allowed: reviewer`},
		{"check", "reviewer", `role "reviewer" not allowed in step "check"; allowed: none`},
		{"build", "x", `role "x" not allowed in step "build"; allowed: reviewer`},
	} {
		if _, err := w.RolePrompt(c.step, c.role, Data{}); err == nil || err.Error() != c.want {
			t.Fatalf("RolePrompt(%s, %s): %v", c.step, c.role, err)
		}
	}
}

func TestParseRefuses(t *testing.T) {
	for name, src := range map[string]string{
		"no steps":       `roles: {}`,
		"unnamed step":   `steps: [{prompt: p}]`,
		"duplicate step": `steps: [{name: a, prompt: p}, {name: a, prompt: p}]`,
		"no prompt":      `steps: [{name: a}]`,
		"unknown helper": `steps: [{name: a, prompt: p, helpers: [ghost]}]`,
		"bad template":   `steps: [{name: a, prompt: "{{.Goal"}]`,
		"unknown field":  `steps: [{name: a, prompt: "{{.Gaol}}"}]`,
		"unknown key":    `steps: [{name: a, prompt: p, fanout: 3}]`,
		"bad role":       "steps: [{name: a, prompt: p}]\nroles: {r: {prompt: \"{{\"}}",
		"bad role ink":   "steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, ink: red}}",
		"bad grant":      `steps: [{name: a, prompt: p, grants: [slack]}]`,
		"bad role grant": "steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, grants: [aws]}}",
		"relative dir":   `steps: [{name: a, prompt: p, dir: src/app}]`,
		"bare tilde dir": "steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, dir: \"~other/x\"}}",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestInks(t *testing.T) {
	w, err := Parse([]byte("steps: [{name: a, prompt: p, ink: sakura, mark: \"✿ anything\"}, {name: b, prompt: p}]"))
	if err != nil {
		t.Fatal(err)
	}
	if w.Steps[0].Ink != "sakura" || w.Steps[0].Mark != "✿ anything" || w.Steps[1].Ink != "" {
		t.Fatalf("steps: %+v", w.Steps)
	}
	_, err = Parse([]byte("steps: [{name: a, prompt: p, ink: teal}]"))
	want := `workflow: step a has ink "teal"; allowed: sakura, wisteria, sky, kaki, matcha`
	if err == nil || err.Error() != want {
		t.Fatalf("teal: %v", err)
	}
}

func TestExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../examples/two-step.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Steps) != 2 || len(w.Steps[0].Helpers) == 0 || w.Steps[0].Ink == "" || w.Roles["reviewer"].Ink == "" {
		t.Fatalf("example: %+v", w)
	}
	got, err := w.StepPrompt(1, Data{Goal: "g", Handoff: "built it", Worktrees: []store.Worktree{{Repo: "/r", Path: "/r/wt", Branch: "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "built it") || !strings.Contains(got, "- worktree /r/wt on branch b of /r") {
		t.Fatalf("check prompt: %q", got)
	}
}

func TestAnyHelper(t *testing.T) {
	w, err := Parse([]byte(strings.Replace(twoSteps, "helpers: [reviewer]", "helpers: [\"*\"]", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !w.FreeRole("build", "researcher") || w.FreeRole("build", "reviewer") || w.FreeRole("check", "researcher") {
		t.Fatal("FreeRole")
	}
	if _, err := w.RolePrompt("build", "tester", Data{}); err != nil {
		t.Fatalf("declared role under *: %v", err)
	}
	if _, err := w.RolePrompt("build", "researcher", Data{}); err == nil {
		t.Fatal("RolePrompt rendered an undeclared role")
	}
}

func TestFreeformExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../examples/freeform.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Steps) != 1 || !w.FreeRole("lead", "anything") {
		t.Fatalf("freeform: %+v", w)
	}
	if got := w.Memory("lead", "promoter-eng-2618"); got != "4G" {
		t.Fatalf("freeform helper memory %q", got)
	}
	if !slices.Equal(w.Steps[0].Grants, []string{GrantLinear}) {
		t.Fatalf("lead grants %q", w.Steps[0].Grants)
	}
	got, err := w.StepPrompt(0, Data{Goal: "fix the login page"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Goal: fix the login page") {
		t.Fatalf("lead prompt drops the goal: %q", got)
	}
}

// Every example's first step must carry the goal, or what the person typed
// is lost when the run starts.
func TestExamplesFirstStepHasGoal(t *testing.T) {
	paths, err := filepath.Glob("../../examples/*.yaml")
	if err != nil || len(paths) == 0 {
		t.Fatalf("examples: %v %v", paths, err)
	}
	const goal = "a goal no prompt says by itself"
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		w, err := Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got, err := w.StepPrompt(0, Data{Goal: goal})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !strings.Contains(got, goal) {
			t.Errorf("%s: first step %q does not use the goal", p, w.Steps[0].Name)
		}
	}
}

func TestHeadless(t *testing.T) {
	src := strings.Replace(strings.Replace(twoSteps,
		"    helpers: [reviewer]\n", "    helpers: [reviewer]\n    headless: true\n", 1),
		"  tester:\n", "  tester:\n    headless: true\n", 1)
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		step, role string
		want       bool
	}{
		{"build", "", true},
		{"check", "", false},
		{"build", "reviewer", false},
		{"check", "tester", true},
		{"build", "ghost", false},
		{"nope", "", false},
	} {
		if got := w.Headless(c.step, c.role); got != c.want {
			t.Errorf("Headless(%q, %q) = %v", c.step, c.role, got)
		}
	}
}

func TestGrants(t *testing.T) {
	src := strings.Replace(strings.Replace(twoSteps,
		"    helpers: [reviewer]\n", "    helpers: [reviewer]\n    grants: [github, linear]\n", 1),
		"  tester:\n", "  tester:\n    grants: [linear]\n", 1)
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		step, role string
		want       []string
	}{
		{"build", "", []string{"github", "linear"}},
		{"check", "", nil},
		{"build", "reviewer", nil},
		{"check", "tester", []string{"linear"}},
		{"build", "ghost", nil},
		{"nope", "", nil},
	} {
		if got := w.Grants(c.step, c.role); !slices.Equal(got, c.want) {
			t.Errorf("Grants(%q, %q) = %q", c.step, c.role, got)
		}
	}
	_, err = Parse([]byte("steps: [{name: a, prompt: p, grants: [aws]}]"))
	if want := `workflow: step a has grant "aws"; allowed: github, github-read, linear, mcp:<name>, claudeai:<connector>`; err == nil || err.Error() != want {
		t.Fatalf("aws: %v", err)
	}
	if _, err := Parse([]byte("steps: [{name: a, prompt: p, grants: [github-read]}]")); err != nil {
		t.Fatalf("github-read: %v", err)
	}
	if _, err := Parse([]byte(`steps: [{name: a, prompt: p, grants: ["mcp:slack", "mcp:my_srv-2"]}]`)); err != nil {
		t.Fatalf("mcp: %v", err)
	}
	if _, err := Parse([]byte(`steps: [{name: a, prompt: p, grants: ["claudeai:slack", "claudeai:google-drive"]}]`)); err != nil {
		t.Fatalf("claudeai: %v", err)
	}
	for _, bad := range []string{"mcp:", "mcp:../x", "mcp:Slack", "mcp:aeolian", "mcp:a/b", "claudeai:", "claudeai:Slack", "claudeai:a b"} {
		if _, err := Parse([]byte(`steps: [{name: a, prompt: p, grants: ["` + bad + `"]}]`)); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestMemory(t *testing.T) {
	src := strings.Replace(strings.Replace(twoSteps,
		"    helpers: [reviewer]\n", "    helpers: [reviewer]\n    memory: 4G\n", 1),
		"  tester:\n", "  tester:\n    memory: 1536M\n", 1)
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ step, role, want string }{
		{"build", "", "4G"},
		{"check", "", ""},
		{"build", "reviewer", ""},
		{"check", "tester", "1536M"},
		{"build", "tester", "1536M"},
		{"build", "ghost", "4G"},
		{"check", "ghost", ""},
		{"nope", "ghost", ""},
		{"nope", "", ""},
	} {
		if got := w.Memory(c.step, c.role); got != c.want {
			t.Errorf("Memory(%q, %q) = %q", c.step, c.role, got)
		}
	}
	for src, want := range map[string]string{
		"steps: [{name: a, prompt: p, memory: 4GB}]":                        `workflow: step a: memory "4GB" is not a size like 4G or 1536M`,
		"steps: [{name: a, prompt: p, memory: 100M}]":                       `workflow: step a: memory "100M" is under 512M`,
		"steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, memory: x}}": `workflow: role r: memory "x" is not a size like 4G or 1536M`,
		"steps: [{name: a, prompt: p, boot: bare}]":                         `workflow: step a: boot "bare" is not one of full, lean`,
		"steps: [{name: a, prompt: p}]\nroles: {r: {prompt: p, boot: x}}":   `workflow: role r: boot "x" is not one of full, lean`,
		"steps: [{name: a, prompt: p, profile: lean}]":                      "workflow: yaml: unmarshal errors:\n  line 1: field profile not found in type workflow.Step",
	} {
		if _, err := Parse([]byte(src)); err == nil || err.Error() != want {
			t.Errorf("%s: %v", src, err)
		}
	}
}

func TestBoot(t *testing.T) {
	w, err := Parse([]byte("steps: [{name: a, prompt: p, boot: lean}, {name: b, prompt: p}]\nroles: {r: {prompt: p, boot: full}, q: {prompt: p}}"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ step, role, want string }{
		{"a", "", "lean"}, {"b", "", ""}, {"a", "r", "full"}, {"a", "q", ""}, {"a", "free", ""}, {"nope", "", ""},
	} {
		if got := w.Boot(c.step, c.role); got != c.want {
			t.Errorf("Boot(%q, %q) = %q, want %q", c.step, c.role, got, c.want)
		}
	}
}

func TestDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	w, err := Parse([]byte("steps: [{name: a, prompt: p, dir: /srv/app/}, {name: b, prompt: p}]\nroles: {r: {prompt: p, dir: \"~/dev/x\"}, q: {prompt: p}}"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ step, role, want string }{
		{"a", "", "/srv/app"}, {"b", "", ""}, {"a", "r", filepath.Join(home, "dev", "x")}, {"a", "q", ""}, {"a", "free", ""}, {"nope", "", ""},
	} {
		if got, err := w.Dir(c.step, c.role); err != nil || got != c.want {
			t.Errorf("Dir(%q, %q) = %q %v, want %q", c.step, c.role, got, err, c.want)
		}
	}
}

func TestTicketToPRExample(t *testing.T) {
	b, err := os.ReadFile("../../examples/ticket-to-pr.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, st := range w.Steps {
		names = append(names, st.Name)
	}
	if want := []string{"understand", "implement", "review", "publish"}; !slices.Equal(names, want) {
		t.Fatalf("steps %q, want %q", names, want)
	}
	if !slices.Equal(w.Steps[2].Helpers, []string{"skeptic"}) {
		t.Fatalf("review helpers %q", w.Steps[2].Helpers)
	}
	waiting := []string{
		"wait for a human", "wait for someone", "wait for the person", "waits for a human",
		"someone to attach", "watching this terminal", "until the person", "will talk to you",
		"ask the person", "wait false",
	}
	data := Data{Goal: "g", Handoff: "h", Worktrees: []store.Worktree{{Repo: "/r", Path: "/r/wt", Branch: "b"}}, ExtraInstructions: "x"}
	overSSH := []string{"fetch origin", "pull origin", "push origin", "ls-remote origin", "ls-remote --heads origin", "remote update", "fetch --all"}
	setsIdentity := []string{
		"--author", "set user.name", "change user.name", "user.name <", "user.email <", "user.name \"", "user.email \"",
		"git config --global", "git config --local", "git -c user.", "git_author_name=", "git_committer_name=",
	}
	check := func(name, prompt string) {
		low := strings.ToLower(strings.Join(strings.Fields(prompt), " "))
		for _, p := range []string{"co-authored-by: claude", "noreply@anthropic.com"} {
			if strings.Contains(low, p) {
				t.Errorf("%s prompt still has the Claude co-author trailer: %q", name, p)
			}
		}
		for _, p := range setsIdentity {
			for rest, i := low, strings.Index(low, p); i >= 0; i = strings.Index(rest, p) {
				before := rest[:i]
				if !strings.HasSuffix(before, "never ") && !strings.HasSuffix(before, "never pass ") {
					t.Errorf("%s prompt tells the agent to set its identity: %q in %q", name, p, rest[max(0, i-40):min(len(rest), i+40)])
				}
				rest = rest[i+len(p):]
			}
		}
		for _, p := range waiting {
			if strings.Contains(low, p) && !strings.Contains(low, "never pass "+p) {
				t.Errorf("%s prompt says %q", name, p)
			}
		}
		for _, p := range overSSH {
			if strings.Contains(low, p) {
				t.Errorf("%s prompt reaches GitHub through origin: %q", name, p)
			}
		}
	}
	memory := map[string]string{"implement": "3G", "review": "3G"}
	for _, st := range w.Steps {
		if st.Memory != memory[st.Name] {
			t.Errorf("step %s memory %q, want %q", st.Name, st.Memory, memory[st.Name])
		}
	}
	if w.Roles["skeptic"].Memory != "3G" {
		t.Errorf("skeptic memory %q", w.Roles["skeptic"].Memory)
	}
	implement, err := w.StepPrompt(w.StepIndex("implement"), data)
	if err != nil || !strings.Contains(strings.Join(strings.Fields(implement), " "), "about 3 GB of memory") {
		t.Errorf("implement prompt does not state its memory: %v", err)
	}
	for i, st := range w.Steps {
		if !st.Headless {
			t.Errorf("step %s is not headless", st.Name)
		}
		want := map[string][]string{"understand": {GrantGitHubRead}, "publish": {GrantGitHub}}[st.Name]
		if !slices.Equal(st.Grants, want) {
			t.Errorf("step %s grants %q, want %q", st.Name, st.Grants, want)
		}
		p, err := w.StepPrompt(i, data)
		if err != nil {
			t.Fatal(err)
		}
		check("step "+st.Name, p)
		if !strings.Contains(p, "outcome blocked") {
			t.Errorf("step %s prompt never says how to end blocked", st.Name)
		}
	}
	for _, name := range []string{"implement", "review"} {
		p, err := w.StepPrompt(w.StepIndex(name), data)
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(p), " ")
		for _, want := range []string{"Co-authored-by: <name> <email>", "git config user.name", "git config user.email", "never pass --author", "blank line"} {
			if !strings.Contains(flat, want) {
				t.Errorf("committing step %s prompt lacks %q", name, want)
			}
		}
	}
	publish, err := w.StepPrompt(w.StepIndex("publish"), data)
	if err != nil || !strings.Contains(strings.Join(strings.Fields(publish), " "), "Executado na máquina de <name> pelo aeolian with <name> from git config user.name") {
		t.Errorf("publish prompt lost the machine line: %v", err)
	}
	if len(w.Roles) == 0 {
		t.Fatal("no roles")
	}
	for name, r := range w.Roles {
		if !r.Headless {
			t.Errorf("role %s is not headless", name)
		}
		if len(r.Grants) != 0 {
			t.Errorf("role %s grants %q", name, r.Grants)
		}
		p, err := w.RolePrompt("review", name, data)
		if err != nil {
			t.Fatal(err)
		}
		check("role "+name, p)
	}
}

func TestDescription(t *testing.T) {
	w, err := Parse([]byte("description: Build then check.\n" + twoSteps))
	if err != nil {
		t.Fatal(err)
	}
	if w.Description != "Build then check." {
		t.Fatalf("description: %q", w.Description)
	}
	w, err = Parse([]byte(twoSteps))
	if err != nil {
		t.Fatal(err)
	}
	if w.Description != "" {
		t.Fatalf("description without one: %q", w.Description)
	}
}

func TestTemplatesSeeParts(t *testing.T) {
	w, err := Parse([]byte(`
steps:
  - name: s
    prompt: "go"
    helpers: [checker]
roles:
  checker:
    prompt: "Check:{{range .Parts}} {{.Kind}}={{.Text}}{{end}}"
`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.RolePrompt("s", "checker", Data{Parts: []store.Part{{Kind: "claims", Body: []byte(`{"claims":[]}`)}}})
	if err != nil || got != `Check: claims={"claims":[]}` {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Parse([]byte("steps:\n  - name: s\n    prompt: \"{{range .Parts}}{{.Bdy}}{{end}}\"\n")); err == nil {
		t.Fatal("a misspelt part field parsed")
	}
}

func TestWitnessedStep(t *testing.T) {
	w, err := Parse([]byte(`
steps:
  - name: map
    witness: true
    prompt: "Map {{.Goal}}."
  - name: write
    prompt: "Write."
`))
	if err != nil {
		t.Fatal(err)
	}
	if !w.Witnessed("map") || w.Witnessed("write") || w.Witnessed("nope") {
		t.Fatal("witness flags wrong")
	}
	if _, err := Parse([]byte("steps:\n  - name: a\n    witnessed: true\n    prompt: x\n")); err == nil {
		t.Fatal("a misspelt key parsed")
	}
}

func TestWitnessDemoParses(t *testing.T) {
	b, err := os.ReadFile("../../examples/witness-demo.yaml")
	if err != nil {
		t.Fatal(err)
	}
	w, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Witnessed("survey") || w.Witnessed("brief") {
		t.Fatalf("example: %+v", w.Steps)
	}
}

func TestWitnessRoleIsReserved(t *testing.T) {
	_, err := Parse([]byte("roles:\n  witness:\n    prompt: judge\nsteps:\n  - name: a\n    prompt: x\n"))
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v", err)
	}
}
