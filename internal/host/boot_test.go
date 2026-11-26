package host

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func TestBootPrecedence(t *testing.T) {
	f := newFixture(t)
	src := strings.Replace(strings.Replace(testWorkflow,
		"    helpers: [reviewer]\n", "    helpers: [reviewer, \"*\"]\n    boot: lean\n", 1),
		"  tester:\n", "  tester:\n    boot: full\n", 1)
	resp := f.startRun(t, src, "g")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	helper := func(role string) store.Agent {
		return store.Agent{ID: role, RunID: run.ID, Step: "build", ParentID: build.ID, Role: role}
	}
	check := store.Agent{ID: "c", RunID: run.ID, Step: "check"}
	witness := store.Agent{ID: "w", RunID: run.ID, Step: "build", ParentID: build.ID, Role: WitnessRole}
	lone := store.Agent{ID: "x"}

	cases := []struct {
		name string
		cfg  BootConfig
		a    store.Agent
		want string
	}{
		{"step boot from workflow", BootConfig{}, build, agent.BootLean},
		{"step without boot is full", BootConfig{}, check, agent.BootFull},
		{"lone agent is full", BootConfig{}, lone, agent.BootFull},
		{"default applies to steps", BootConfig{Default: agent.BootLean}, check, agent.BootLean},
		{"role default lean: judge", BootConfig{}, helper("judge"), agent.BootLean},
		{"role default lean: proposer", BootConfig{}, helper("proposer"), agent.BootLean},
		{"role default lean: evolver", BootConfig{}, helper("evolver"), agent.BootLean},
		{"role default lean: witness", BootConfig{}, witness, agent.BootLean},
		{"other role is full", BootConfig{}, helper("reviewer"), agent.BootFull},
		{"other role follows default", BootConfig{Default: agent.BootLean}, helper("reviewer"), agent.BootLean},
		{"config role over role default", BootConfig{Roles: map[string]string{"judge": agent.BootFull}}, helper("judge"), agent.BootFull},
		{"config role over default", BootConfig{Default: agent.BootFull, Roles: map[string]string{"researcher": agent.BootLean}}, helper("researcher"), agent.BootLean},
		{"config witness", BootConfig{Roles: map[string]string{WitnessRole: agent.BootFull}}, witness, agent.BootFull},
		{"workflow role over config", BootConfig{Roles: map[string]string{"tester": agent.BootLean}}, helper("tester"), agent.BootFull},
		{"role key does not reach a step", BootConfig{Roles: map[string]string{"": agent.BootLean}}, check, agent.BootFull},
	}
	for _, c := range cases {
		f.host.Boot = c.cfg
		if got := f.host.boot(c.a); got != c.want {
			t.Errorf("%s: boot %q, want %q", c.name, got, c.want)
		}
	}
	f.host.Boot = BootConfig{}
	argv := f.keeper.argv[0]
	for _, flag := range agent.LeanArgs {
		if !slices.Contains(argv, flag) {
			t.Errorf("lean step argv lacks %q: %q", flag, argv)
		}
	}
	if promptOf(t, argv) != "Build g." {
		t.Fatalf("prompt moved: %q", argv)
	}
}

func TestConfinedRoles(t *testing.T) {
	h := &Host{}
	a := func(role string) store.Agent { return store.Agent{ID: role, ParentID: "p", Role: role} }
	for role, want := range map[string]bool{WitnessRole: true, "judge": true, "proposer": false, "reviewer": false} {
		if got := h.confined(a(role)); got != want {
			t.Errorf("%s confined %v, want %v", role, got, want)
		}
	}
	if h.confined(store.Agent{ID: "s", Role: "judge"}) {
		t.Error("a step agent has no role to confine")
	}
	h.Boot.Confine = []string{}
	if h.confined(a(WitnessRole)) {
		t.Error("an empty confine list still confined the witness")
	}
	h.Boot.Confine = []string{"reviewer"}
	if !h.confined(a("reviewer")) || h.confined(a("judge")) {
		t.Error("confine list not followed")
	}
}

func TestBootConfigValidate(t *testing.T) {
	if err := (BootConfig{Default: "lean", Roles: map[string]string{"a": "full"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (BootConfig{Default: "tiny"}).Validate(); err == nil || !strings.Contains(err.Error(), "boot.default") {
		t.Fatalf("bad default: %v", err)
	}
	if err := (BootConfig{Roles: map[string]string{"judge": "slim"}}).Validate(); err == nil || !strings.Contains(err.Error(), "boot.roles.judge") {
		t.Fatalf("bad role: %v", err)
	}
}

func readSettings(t *testing.T, h *Host, id string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(h.Layout.Settings(id))
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestJudgeLaunchesLeanAndConfined(t *testing.T) {
	f := newFixture(t)
	f.host.MaxAgents = 10
	resp := f.startRun(t, strings.Replace(testWorkflow, "helpers: [reviewer]", `helpers: [reviewer, "*"]`, 1), "g")
	var run store.Run
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	build := f.stepAgent(t, run.ID, "build")
	wt := t.TempDir()
	extra := t.TempDir()
	judge, err := f.host.launch(context.Background(), store.Agent{
		ID: agent.NewSessionID(), Dir: wt, Prompt: "judge it", ParentID: build.ID, Role: "judge", RunID: run.ID, Step: "build",
	}, extra)
	if err != nil {
		t.Fatal(err)
	}
	argv := f.keeper.argv[len(f.keeper.argv)-1]
	if promptOf(t, argv) != "judge it" || !slices.Contains(argv, "--disable-slash-commands") {
		t.Fatalf("judge argv: %q", argv)
	}
	s := readSettings(t, f.host, judge.ID)
	perms, ok := s["permissions"].(map[string]any)
	if !ok || perms["blockReadsOutsideWorkingDirectories"] != true {
		t.Fatalf("judge not confined: %v", s)
	}
	var dirs []string
	for _, d := range perms["additionalDirectories"].([]any) {
		dirs = append(dirs, d.(string))
	}
	if !slices.Equal(dirs, []string{f.host.Layout.ScratchDir(judge.ID), wt, extra}) {
		t.Fatalf("read roots: %q", dirs)
	}
	var deny []string
	for _, d := range perms["deny"].([]any) {
		deny = append(deny, d.(string))
	}
	if want := "Read(/" + f.host.Layout.Home + "/agents/**)"; !slices.Contains(deny, want) {
		t.Fatalf("deny %q lacks %q", deny, want)
	}
	if s["outputStyle"] != "default" || s["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("judge settings not lean: %v", s)
	}
	if _, ok := s["hooks"]; !ok {
		t.Fatal("lean judge lost the host's hooks")
	}

	reviewer, err := f.host.launch(context.Background(), store.Agent{
		ID: agent.NewSessionID(), Dir: wt, Prompt: "review it", ParentID: build.ID, Role: "reviewer", RunID: run.ID, Step: "build",
	})
	if err != nil {
		t.Fatal(err)
	}
	s = readSettings(t, f.host, reviewer.ID)
	if _, ok := s["permissions"]; ok {
		t.Fatalf("reviewer confined: %v", s)
	}
	if _, ok := s["outputStyle"]; ok {
		t.Fatalf("full reviewer got lean settings: %v", s)
	}
	argv = f.keeper.argv[len(f.keeper.argv)-1]
	if slices.Contains(argv, "--setting-sources") {
		t.Fatalf("full reviewer argv: %q", argv)
	}
}

func TestWitnessRoots(t *testing.T) {
	if r := witnessRoots(store.Agent{}); r != nil {
		t.Fatalf("no handoff: %q", r)
	}
	p := store.Agent{Handoff: &store.Handoff{Worktrees: []store.Worktree{{Path: "/a"}, {Path: "/b"}}}}
	if r := witnessRoots(p); !slices.Equal(r, []string{"/a", "/b"}) {
		t.Fatalf("roots: %q", r)
	}
	// Evidence such as notes.md:2 resolves in the producer's own folder.
	p.Dir = "/p"
	if r := witnessRoots(p); !slices.Equal(r, []string{"/a", "/b", "/p"}) {
		t.Fatalf("roots with dir: %q", r)
	}
}

// Arena judges boot lean and confined, and can still read the entries the
// arena leaves in their scratch; proposers boot lean, unconfined.
func TestArenaJudgesReadTheirEntriesConfined(t *testing.T) {
	f := newArenaFixture(t, 10)
	a, err := f.host.StartArena(ArenaRequest{Question: "q?", Priors: testPriors, Judges: 1, NoEvolve: true})
	if err != nil {
		t.Fatal(err)
	}
	props := waitArenaAgents(t, f, a.ID, ArenaRoleProposer, 3)
	for _, p := range props {
		if argv := f.argvOf(t, p); !slices.Contains(argv, "--disable-slash-commands") {
			t.Fatalf("proposer argv: %q", argv)
		}
		if _, ok := readSettings(t, f.host, p.ID)["permissions"]; ok {
			t.Fatal("proposer confined")
		}
	}
	proposeAll(t, f, a.ID, 3)
	jd := waitArenaAgents(t, f, a.ID, ArenaRoleJudge, 1)[0]
	if argv := f.argvOf(t, jd); !slices.Contains(argv, "--disable-slash-commands") {
		t.Fatalf("judge argv: %q", argv)
	}
	perms, ok := readSettings(t, f.host, jd.ID)["permissions"].(map[string]any)
	if !ok || perms["blockReadsOutsideWorkingDirectories"] != true {
		t.Fatal("judge not confined")
	}
	var roots []string
	for _, d := range perms["additionalDirectories"].([]any) {
		roots = append(roots, d.(string))
	}
	entries, _ := filepath.Glob(filepath.Join(jd.Dir, "entries", "*.md"))
	if len(entries) != 3 {
		t.Fatalf("entries in %s: %q", jd.Dir, entries)
	}
	for _, e := range entries {
		if !slices.ContainsFunc(roots, func(r string) bool { return strings.HasPrefix(e, r+string(filepath.Separator)) }) {
			t.Fatalf("entry %s outside the judge's read roots %q", e, roots)
		}
	}
	if slices.ContainsFunc(roots, func(r string) bool { return strings.HasPrefix(a.Dir, r) }) {
		t.Fatalf("judge can read the arena folder %s: %q", a.Dir, roots)
	}
}
