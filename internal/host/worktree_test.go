package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// gitRepo makes a repo with one commit on main.
func gitRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", repo},
		{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return repo
}

// withClaudeJSON points the host's trust writes at a temp config.
func (f *fixture) withClaudeJSON(t *testing.T) string {
	t.Helper()
	cj := filepath.Join(t.TempDir(), ".claude.json")
	f.host.Layout.ClaudeJSON = cj
	return cj
}

func trusted(t *testing.T, cj, dir string) bool {
	t.Helper()
	b, err := os.ReadFile(cj)
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Projects map[string]map[string]any }
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Projects[dir]["hasTrustDialogAccepted"] == true
}

func (f *fixture) eventsOf(t *testing.T, kind string) []store.Event {
	t.Helper()
	evs, err := f.store.Events(store.EventQuery{Kinds: []string{kind}})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSpawnWithWorktree(t *testing.T) {
	f := newFixture(t)
	cj := f.withClaudeJSON(t)
	repo := gitRepo(t)
	parent := f.start(t)
	res := callTool(t, f.connect(t, parent.ID), "spawn", map[string]any{
		"role":     "coder",
		"handoff":  map[string]any{"text": "fix it", "worktrees": []store.Worktree{}},
		"worktree": map[string]any{"repo": repo, "branch": "feat/fix"},
		"wait":     false,
	})
	if res.IsError {
		t.Fatalf("spawn: %+v", res)
	}
	helper := f.helperOf(t, parent.ID)
	want := filepath.Join(f.host.Layout.Home, "worktrees", "_", short(parent.ID), "feat-fix")
	if helper.Dir != want {
		t.Fatalf("helper dir %q, want %q", helper.Dir, want)
	}
	if !strings.Contains(helper.Prompt, want) || !strings.Contains(helper.Prompt, "feat/fix") {
		t.Fatalf("prompt does not name the worktree: %q", helper.Prompt)
	}
	if out, err := exec.Command("git", "-C", want, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(out)) != "feat/fix" {
		t.Fatalf("worktree branch: %q %v", out, err)
	}
	if !trusted(t, cj, want) {
		t.Fatal("worktree not trusted")
	}
	if _, err := os.Stat(f.host.Layout.ScratchDir(helper.ID)); !os.IsNotExist(err) {
		t.Fatalf("helper got a scratch folder too: %v", err)
	}
	wts, _ := f.store.ManagedWorktrees("")
	if len(wts) != 1 || wts[0].Path != want || wts[0].AgentID != parent.ID || wts[0].State != store.WorktreeActive {
		t.Fatalf("managed worktrees: %+v", wts)
	}
	if evs := f.eventsOf(t, "worktree.created"); len(evs) != 1 || evs[0].AgentID != parent.ID {
		t.Fatalf("events: %+v", evs)
	}
}

func TestSpawnWorktreeByConfigKeyDefaultsBranchAndBase(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	exec.Command("git", "-C", repo, "branch", "dev").Run()
	f.host.Repos = map[string]Repo{"app": {Path: repo, Base: "dev"}}
	parent := f.start(t)
	res := callTool(t, f.connect(t, parent.ID), "spawn", map[string]any{
		"role":     "coder",
		"handoff":  map[string]any{"text": "x", "worktrees": []store.Worktree{}},
		"worktree": map[string]any{"repo": "app"},
		"wait":     false,
	})
	if res.IsError {
		t.Fatalf("spawn: %+v", res)
	}
	helper := f.helperOf(t, parent.ID)
	wts, _ := f.store.ManagedWorktrees("")
	if len(wts) != 1 || wts[0].RepoKey != "app" || wts[0].Base != "dev" || wts[0].Branch != "aeolian/coder-"+short(helper.ID) {
		t.Fatalf("managed: %+v", wts)
	}
}

func TestSpawnWorktreeAndHandoffWorktreeConflict(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	parent := f.start(t)
	res := callTool(t, f.connect(t, parent.ID), "spawn", map[string]any{
		"role":     "coder",
		"handoff":  map[string]any{"text": "x", "worktrees": []store.Worktree{{Repo: repo, Path: repo, Branch: "main"}}},
		"worktree": map[string]any{"repo": repo},
	})
	if !res.IsError || !strings.Contains(resultText(res), "not both") {
		t.Fatalf("spawn: %+v", res)
	}
	if n := f.keeper.startedCount(); n != 1 {
		t.Fatalf("%d sessions started", n)
	}
}

func TestSpawnWorktreeRefusesExistingBranchAndUnknownRepo(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	parent := f.start(t)
	s := f.connect(t, parent.ID)
	for _, wt := range []map[string]any{
		{"repo": repo, "branch": "main"},
		{"repo": "nope"},
		{"repo": filepath.Join(repo, "sub")},
		{"repo": repo, "branch": "bad..name"},
	} {
		res := callTool(t, s, "spawn", map[string]any{
			"role": "coder", "handoff": map[string]any{"text": "x", "worktrees": []store.Worktree{}}, "worktree": wt,
		})
		if !res.IsError {
			t.Errorf("worktree %v accepted", wt)
		}
	}
	if wts, _ := f.store.ManagedWorktrees(""); len(wts) != 0 {
		t.Fatalf("recorded: %+v", wts)
	}
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestWorktreeToolLeadOnly(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	lead := f.start(t)
	s := f.connect(t, lead.ID)
	if names := toolNames(t, s); !strings.Contains(strings.Join(names, ","), "worktree") {
		t.Fatalf("lead tools: %v", names)
	}
	res := callTool(t, s, "worktree", map[string]any{"repo": repo, "branch": "shared"})
	if res.IsError {
		t.Fatalf("worktree: %+v", res)
	}
	var out WorktreeOutput
	if err := json.Unmarshal([]byte(resultText(res)), &out); err != nil || out.Branch != "shared" || out.Repo != repo {
		t.Fatalf("out: %q %v", resultText(res), err)
	}
	if fi, err := os.Stat(out.Path); err != nil || !fi.IsDir() {
		t.Fatalf("path: %v", err)
	}

	call := f.spawn(context.Background(), t, s, "reviewer")
	helper := f.helperOf(t, lead.ID)
	for _, name := range toolNames(t, f.connect(t, helper.ID)) {
		if name == "worktree" {
			t.Error("a helper got the worktree tool")
		}
	}
	f.done(t, helper.ID, "ok")
	<-call
}

func TestConcurrentWorktreesSameRepo(t *testing.T) {
	f := newFixture(t)
	repo := gitRepo(t)
	lead := f.start(t)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, _, err := f.host.worktreeTool(lead.ID, WorktreeRequest{Repo: repo, Branch: fmt.Sprintf("b%d", i)})
			if err == nil && res.IsError {
				err = fmt.Errorf("%s", resultText(res))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	wts, _ := f.store.ManagedWorktrees("")
	if len(wts) != 10 {
		t.Fatalf("%d worktrees recorded", len(wts))
	}
	out, _ := exec.Command("git", "-C", repo, "worktree", "list").Output()
	if n := strings.Count(string(out), "\n"); n != 11 {
		t.Fatalf("git lists %d worktrees:\n%s", n, out)
	}
}

func TestPrepareTrustsHelperWorktreeNotScratch(t *testing.T) {
	f := newFixture(t)
	cj := f.withClaudeJSON(t)
	wt := t.TempDir()
	parent := f.start(t)
	call := f.spawn(context.Background(), t, f.connect(t, parent.ID), "r", store.Worktree{Repo: wt, Path: wt, Branch: "x"})
	helper := f.helperOf(t, parent.ID)
	defer func() { f.done(t, helper.ID, "ok"); <-call }()
	if !trusted(t, cj, wt) {
		t.Fatal("handoff worktree not trusted")
	}
	if trusted(t, cj, f.host.Layout.ScratchDir(helper.ID)) {
		t.Fatal("scratch trusted for a worktree helper")
	}
}
