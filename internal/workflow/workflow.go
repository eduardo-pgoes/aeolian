// Package workflow reads a workflow file: steps that run one after another,
// each with a prompt template and the helper roles it may spawn.
package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

type Workflow struct {
	Description string          `yaml:"description"`
	Steps       []Step          `yaml:"steps"`
	Roles       map[string]Role `yaml:"roles"`
}

type Step struct {
	Name    string   `yaml:"name"`
	Prompt  string   `yaml:"prompt"`
	Helpers []string `yaml:"helpers"`
	Ink     string   `yaml:"ink"`
	Mark    string   `yaml:"mark"`
	// Tuning is the waveform the map draws this identity in, one of
	// Tunings. It replaces Mark; a file sets one or the other.
	Tuning   string   `yaml:"tuning"`
	Model    string   `yaml:"model"`
	Headless bool     `yaml:"headless"`
	Grants   []string `yaml:"grants"`
	Memory   string   `yaml:"memory"`
	// Boot is the step agent's boot profile, full or lean; empty leaves it
	// to config.json.
	Boot string `yaml:"boot"`
	// Witness has the host check the step agent's claims with a clean
	// context witness before the run moves on; the step must then hand
	// back a claims part.
	Witness bool   `yaml:"witness"`
	Dir     string `yaml:"dir"`

	tmpl *template.Template
}

type Role struct {
	Prompt string `yaml:"prompt"`
	Ink    string `yaml:"ink"`
	Mark   string `yaml:"mark"`
	// Tuning is the waveform the map draws this identity in, one of
	// Tunings. It replaces Mark; a file sets one or the other.
	Tuning   string   `yaml:"tuning"`
	Model    string   `yaml:"model"`
	Headless bool     `yaml:"headless"`
	Grants   []string `yaml:"grants"`
	Memory   string   `yaml:"memory"`
	// Boot is the helper's boot profile, full or lean; empty leaves it to
	// config.json.
	Boot string `yaml:"boot"`
	Dir  string `yaml:"dir"`

	tmpl *template.Template
}

// Data is what a prompt template sees. For a step, Handoff and Worktrees
// come from the previous step's done and are empty on the first step; for a
// helper role they come from the spawn call, as does ExtraInstructions.
// Inks are the colours a step or role may name; an empty ink is plain.
var Inks = []string{"sakura", "wisteria", "sky", "kaki", "matcha"}

const (
	GrantGitHub         = "github"
	GrantGitHubRead     = "github-read"
	GrantLinear         = "linear"
	GrantMCPPrefix      = "mcp:"
	GrantClaudeAIPrefix = "claudeai:"
)

var Grants = []string{GrantGitHub, GrantGitHubRead, GrantLinear}

func checkMemory(name, memory string) error {
	if _, _, err := agent.MemoryLimits(memory); err != nil {
		return fmt.Errorf("workflow: %s: %w", name, err)
	}
	return nil
}

func checkBoot(name, boot string) error {
	if err := agent.CheckBoot(boot); err != nil {
		return fmt.Errorf("workflow: %s: %w", name, err)
	}
	return nil
}

var mcpName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func MCPServer(grant string) (string, bool) {
	name, ok := strings.CutPrefix(grant, GrantMCPPrefix)
	return name, ok
}

func ClaudeAIConnector(grant string) (string, bool) {
	return strings.CutPrefix(grant, GrantClaudeAIPrefix)
}

func CheckGrants(name string, grants []string) error {
	for _, g := range grants {
		if connector, ok := ClaudeAIConnector(g); ok {
			if !mcpName.MatchString(connector) {
				return fmt.Errorf("workflow: %s has grant %q; a claudeai grant names a connector in lowercase letters, digits, - and _", name, g)
			}
			continue
		}
		if server, ok := MCPServer(g); ok {
			if !mcpName.MatchString(server) || server == "aeolian" {
				return fmt.Errorf("workflow: %s has grant %q; an mcp grant names a server in lowercase letters, digits, - and _, and not aeolian", name, g)
			}
			continue
		}
		if !slices.Contains(Grants, g) {
			return fmt.Errorf("workflow: %s has grant %q; allowed: %s, %s<name>, %s<connector>", name, g, strings.Join(Grants, ", "), GrantMCPPrefix, GrantClaudeAIPrefix)
		}
	}
	return nil
}

func checkDir(name, dir string) error {
	if dir == "" || filepath.IsAbs(dir) || strings.HasPrefix(dir, "~/") {
		return nil
	}
	return fmt.Errorf("workflow: %s has dir %q; a dir is an absolute path or starts with ~/", name, dir)
}

// ExpandDir turns a dir key into an absolute path, ~/ read as the user's home.
func ExpandDir(dir string) (string, error) {
	rest, ok := strings.CutPrefix(dir, "~/")
	if !ok {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, rest), nil
}

func checkInk(name, ink string) error {
	if ink == "" || slices.Contains(Inks, ink) {
		return nil
	}
	return fmt.Errorf("workflow: %s has ink %q; allowed: %s", name, ink, strings.Join(Inks, ", "))
}

type Data struct {
	Goal              string
	Handoff           string
	Worktrees         []store.Worktree
	ExtraInstructions string
	// Parts are the structured results beside the handoff: a step sees the
	// previous step's, a helper the ones its spawn passed. Each has a Kind
	// and a Text, its body as JSON: {{range .Parts}}{{.Kind}}: {{.Text}}{{end}}.
	Parts []store.Part
}

// Parse reads and checks a workflow, templates included, so a bad file is
// refused before its run starts. Unknown keys are refused too.
// WitnessRole is the role of the host's witness. A workflow may not define
// it: the host sets a witness's model, grants and tools itself.
const WitnessRole = "witness"

func Parse(b []byte) (*Workflow, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var w Workflow
	if err := dec.Decode(&w); err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}
	if len(w.Steps) == 0 {
		return nil, errors.New("workflow: no steps")
	}
	for name, r := range w.Roles {
		if name == WitnessRole {
			return nil, fmt.Errorf("workflow: role %q is reserved for the host's witness; give yours another name", name)
		}
		if err := checkInk("role "+name, r.Ink); err != nil {
			return nil, err
		}
		if err := checkTuning("role "+name, r.Tuning, r.Mark); err != nil {
			return nil, err
		}
		if err := CheckGrants("role "+name, r.Grants); err != nil {
			return nil, err
		}
		if err := checkMemory("role "+name, r.Memory); err != nil {
			return nil, err
		}
		if err := checkBoot("role "+name, r.Boot); err != nil {
			return nil, err
		}
		if err := checkDir("role "+name, r.Dir); err != nil {
			return nil, err
		}
		t, err := parseTemplate("role "+name, r.Prompt)
		if err != nil {
			return nil, err
		}
		r.tmpl = t
		w.Roles[name] = r
	}
	seen := map[string]bool{}
	for i := range w.Steps {
		st := &w.Steps[i]
		if st.Name == "" {
			return nil, fmt.Errorf("workflow: step %d has no name", i+1)
		}
		if seen[st.Name] {
			return nil, fmt.Errorf("workflow: two steps named %q", st.Name)
		}
		seen[st.Name] = true
		if err := checkInk("step "+st.Name, st.Ink); err != nil {
			return nil, err
		}
		if err := checkTuning("step "+st.Name, st.Tuning, st.Mark); err != nil {
			return nil, err
		}
		if err := CheckGrants("step "+st.Name, st.Grants); err != nil {
			return nil, err
		}
		if err := checkMemory("step "+st.Name, st.Memory); err != nil {
			return nil, err
		}
		if err := checkBoot("step "+st.Name, st.Boot); err != nil {
			return nil, err
		}
		if err := checkDir("step "+st.Name, st.Dir); err != nil {
			return nil, err
		}
		for _, h := range st.Helpers {
			if _, ok := w.Roles[h]; !ok && h != AnyHelper {
				return nil, fmt.Errorf("workflow: step %q lists helper %q, which is not under roles", st.Name, h)
			}
		}
		t, err := parseTemplate("step "+st.Name, st.Prompt)
		if err != nil {
			return nil, err
		}
		st.tmpl = t
	}
	return &w, nil
}

// sample is rendered once at parse time so that a misspelt field fails
// then, not when the step or helper starts.
var sample = Data{Goal: "goal", Handoff: "handoff", Worktrees: []store.Worktree{{Repo: "repo", Path: "path", Branch: "branch"}}, ExtraInstructions: "extra",
	Parts: []store.Part{{Kind: "claims", Body: []byte(`{"claims":[]}`)}}}

func parseTemplate(name, text string) (*template.Template, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("workflow: %s has no prompt", name)
	}
	t, err := template.New(name).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}
	if _, err := render(t, sample); err != nil {
		return nil, err
	}
	return t, nil
}

const AnyHelper = "*"

func (w *Workflow) FreeRole(step, role string) bool {
	if _, ok := w.Roles[role]; ok {
		return false
	}
	i := w.StepIndex(step)
	return i >= 0 && slices.Contains(w.Steps[i].Helpers, AnyHelper)
}

// StepIndex returns the position of the step called name, or -1.
func (w *Workflow) StepIndex(name string) int {
	for i, st := range w.Steps {
		if st.Name == name {
			return i
		}
	}
	return -1
}

func (w *Workflow) Model(step, role string) string {
	if role != "" {
		return w.Roles[role].Model
	}
	if i := w.StepIndex(step); i >= 0 {
		return w.Steps[i].Model
	}
	return ""
}

func (w *Workflow) Headless(step, role string) bool {
	if role != "" {
		return w.Roles[role].Headless
	}
	if i := w.StepIndex(step); i >= 0 {
		return w.Steps[i].Headless
	}
	return false
}

// Dir is the expanded dir a step's agent, or a role's helper, starts in;
// empty when the workflow sets none.
func (w *Workflow) Dir(step, role string) (string, error) {
	d := ""
	if role != "" {
		d = w.Roles[role].Dir
	} else if i := w.StepIndex(step); i >= 0 {
		d = w.Steps[i].Dir
	}
	if d == "" {
		return "", nil
	}
	return ExpandDir(d)
}

// Witnessed reports whether step asks for a witness on its agent's claims.
func (w *Workflow) Witnessed(step string) bool {
	i := w.StepIndex(step)
	return i >= 0 && w.Steps[i].Witness
}

func (w *Workflow) Grants(step, role string) []string {
	if role != "" {
		return w.Roles[role].Grants
	}
	if i := w.StepIndex(step); i >= 0 {
		return w.Steps[i].Grants
	}
	return nil
}

func (w *Workflow) Memory(step, role string) string {
	if r, ok := w.Roles[role]; ok && role != "" {
		return r.Memory
	}
	if i := w.StepIndex(step); i >= 0 {
		return w.Steps[i].Memory
	}
	return ""
}

// Boot is the boot profile the workflow sets for step's agent, or for a
// helper in role; empty when it sets none.
func (w *Workflow) Boot(step, role string) string {
	if role != "" {
		return w.Roles[role].Boot
	}
	if i := w.StepIndex(step); i >= 0 {
		return w.Steps[i].Boot
	}
	return ""
}

func (w *Workflow) StepPrompt(i int, d Data) (string, error) {
	return render(w.Steps[i].tmpl, d)
}

// RolePrompt renders the first prompt of a helper in role spawned from
// step, or refuses the spawn when the step does not list the role.
func (w *Workflow) RolePrompt(step, role string, d Data) (string, error) {
	i := w.StepIndex(step)
	if i < 0 {
		return "", fmt.Errorf("no step %q in the workflow", step)
	}
	helpers := w.Steps[i].Helpers
	allowed := false
	for _, h := range helpers {
		allowed = allowed || h == role || h == AnyHelper
	}
	if !allowed {
		list := "none"
		if len(helpers) > 0 {
			list = strings.Join(helpers, ", ")
		}
		return "", fmt.Errorf("role %q not allowed in step %q; allowed: %s", role, step, list)
	}
	if _, ok := w.Roles[role]; !ok {
		return "", fmt.Errorf("role %q is not under roles", role)
	}
	return render(w.Roles[role].tmpl, d)
}

func render(t *template.Template, d Data) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, d); err != nil {
		return "", fmt.Errorf("workflow: %w", err)
	}
	return b.String(), nil
}
