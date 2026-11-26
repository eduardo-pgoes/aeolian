package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareInWorktreeMakesNoScratch(t *testing.T) {
	cj := filepath.Join(t.TempDir(), ".claude.json")
	l := Layout{Home: t.TempDir(), ClaudeJSON: cj}
	wt := t.TempDir()
	if err := l.Prepare("a1", wt, Setup{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.ScratchDir("a1")); !os.IsNotExist(err) {
		t.Fatalf("scratch made for a worktree agent: %v", err)
	}
	if _, err := os.Stat(l.MCPConfig("a1")); err != nil {
		t.Fatal(err)
	}
	projects := readProjects(t, cj)
	if projects[wt]["hasTrustDialogAccepted"] != true {
		t.Fatalf("worktree not trusted: %v", projects)
	}
	if _, ok := projects[l.ScratchDir("a1")]; ok {
		t.Fatal("scratch trusted for a worktree agent")
	}
}

func TestUntrustOnlyOwnDirs(t *testing.T) {
	home := t.TempDir()
	cj := filepath.Join(t.TempDir(), ".claude.json")
	own := filepath.Join(home, "scratch", "a1")
	other := "/home/someone/project"
	sneaky := home + "-evil/x"
	for _, d := range []string{own, other, sneaky} {
		if err := Trust(cj, d); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := os.ReadFile(cj)
	var cfg map[string]json.RawMessage
	json.Unmarshal(raw, &cfg)
	cfg["userID"] = json.RawMessage(`"u"`)
	b, _ := json.Marshal(cfg)
	os.WriteFile(cj, b, 0o600)

	n, err := Untrust(cj, home, []string{own, other, sneaky, home, filepath.Join(home, "..", "x")})
	if err != nil || n != 1 {
		t.Fatalf("untrust: %d %v", n, err)
	}
	projects := readProjects(t, cj)
	if _, ok := projects[own]; ok {
		t.Fatal("own dir kept")
	}
	if _, ok := projects[other]; !ok {
		t.Fatal("foreign dir removed")
	}
	if _, ok := projects[sneaky]; !ok {
		t.Fatal("sibling-prefix dir removed")
	}
	b, _ = os.ReadFile(cj)
	if err := json.Unmarshal(b, &cfg); err != nil || string(cfg["userID"]) != `"u"` {
		t.Fatalf("lost other keys: %s", b)
	}
	if n, err := Untrust(cj, home, []string{own}); n != 0 || err != nil {
		t.Fatalf("second untrust: %d %v", n, err)
	}
}

func readProjects(t *testing.T, cj string) map[string]map[string]any {
	t.Helper()
	var cfg struct{ Projects map[string]map[string]any }
	b, err := os.ReadFile(cj)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Projects
}

func TestTrustAddsRealPathOfSymlinkedDir(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "work", "repo")
	if err := os.MkdirAll(filepath.Join(real, "wt"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "repo")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	realRoot, _ := filepath.EvalSymlinks(real)
	cj := filepath.Join(t.TempDir(), ".claude.json")
	if err := Trust(cj, filepath.Join(link, "wt")); err != nil {
		t.Fatal(err)
	}
	projects := readProjects(t, cj)
	for _, d := range []string{filepath.Join(link, "wt"), filepath.Join(realRoot, "wt")} {
		if projects[d]["hasTrustDialogAccepted"] != true {
			t.Fatalf("%s not trusted: %v", d, projects)
		}
	}
	if len(projects) != 2 {
		t.Fatalf("trusted more than the dir and its real path: %v", projects)
	}
	plain := t.TempDir()
	if err := Trust(cj, plain); err != nil {
		t.Fatal(err)
	}
	if n := len(readProjects(t, cj)); n != 3 {
		t.Fatalf("a dir without symlinks got %d entries", n-2)
	}
}

func TestRealPathOfRemovedDir(t *testing.T) {
	root := t.TempDir()
	realRoot, _ := filepath.EvalSymlinks(root)
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got := RealPath(filepath.Join(root, "link", "gone", "deeper"))
	if want := filepath.Join(realRoot, "real", "gone", "deeper"); got != want {
		t.Fatalf("RealPath = %s, want %s", got, want)
	}
	if got := RealPath("/nonexistent-aeolian/x"); got != "/nonexistent-aeolian/x" {
		t.Fatalf("unresolvable path changed: %s", got)
	}
}

func TestUntrustRemovesRealTwinEvenAfterRemoval(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real-home")
	if err := os.MkdirAll(filepath.Join(realHome, "scratch", "a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(home, "scratch", "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	cj := filepath.Join(t.TempDir(), ".claude.json")
	own := filepath.Join(home, "scratch", "a1")
	for _, d := range []string{own, escape} {
		if err := Trust(cj, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(filepath.Join(realHome, "scratch", "a1")); err != nil {
		t.Fatal(err)
	}
	n, err := Untrust(cj, home, []string{own, escape})
	if err != nil || n != 3 {
		t.Fatalf("untrust: %d %v", n, err)
	}
	projects := readProjects(t, cj)
	realOutside, _ := filepath.EvalSymlinks(outside)
	if _, ok := projects[realOutside]; !ok || len(projects) != 1 {
		t.Fatalf("want only the target outside home left, got %v", projects)
	}
}
