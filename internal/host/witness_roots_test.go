package host

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRepo makes a folder that looks like a git repository's top level
// with file rel in it holding n numbered lines, and returns both paths.
func fakeRepo(t *testing.T, rel string, n int) (repo, file string) {
	t.Helper()
	repo = t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	file = filepath.Join(repo, rel)
	writeFile(t, file, b.String())
	return repo, file
}

func TestWitnessReadsTheRepoItsEvidenceIsIn(t *testing.T) {
	f := newFixture(t)
	witnessHost(f, WitnessConfig{})
	repo, file := fakeRepo(t, "internal/host/cap.go", 300)
	// Another agent's scratch, under the host's own folder: never readable.
	other := filepath.Join(f.host.Layout.ScratchDir("someone-else"), "secret.md")
	writeFile(t, other, "x\ny\n")
	claims := []any{
		map[string]any{"id": "far", "text": "line 150 says line 150", "kind": "fact", "evidence": []string{file + ":150-170"}},
		map[string]any{"id": "home", "text": "x", "kind": "fact", "evidence": []string{other + ":1"}},
		map[string]any{"id": "near", "text": "the cap is 2", "kind": "fact", "evidence": []string{"notes.md:2"}},
	}
	_, a := f.startWitnessed(t, claims)
	w := f.witnessOf(t, a.ID)

	s := readSettings(t, f.host, w.ID)
	perms := s["permissions"].(map[string]any)
	var dirs []string
	for _, d := range perms["additionalDirectories"].([]any) {
		dirs = append(dirs, d.(string))
	}
	if !slices.Contains(dirs, repo) {
		t.Fatalf("read roots %q lack the evidence's repo %s", dirs, repo)
	}
	for _, d := range dirs {
		if within(other, d) && d != a.Dir {
			t.Fatalf("read roots %q reach another agent's scratch via %s", dirs, d)
		}
	}
	if !strings.Contains(w.Prompt, "You may open files under:") || !strings.Contains(w.Prompt, repo) {
		t.Fatalf("prompt does not name what it may read:\n%s", w.Prompt)
	}
	// The excerpt reaches 40 lines past the cited range.
	for _, want := range []string{"  150* line 150", "  210  line 210", "  110  line 110"} {
		if !strings.Contains(w.Prompt, want) {
			t.Fatalf("prompt lacks %q", want)
		}
	}
	if strings.Contains(w.Prompt, "  211  line 211") || strings.Contains(w.Prompt, "  109  line 109") {
		t.Fatal("excerpt runs past its window")
	}
}

func TestEvidenceReadRootsRefuses(t *testing.T) {
	userHome := t.TempDir()
	home := filepath.Join(userHome, ".local", "share", "aeolian")
	own := filepath.Join(home, "scratch", "producer")
	ownWT := filepath.Join(home, "worktrees", "run", "fix")
	otherScratch := filepath.Join(home, "scratch", "other")
	loose := filepath.Join(userHome, "loose")
	for _, p := range []string{
		filepath.Join(own, "a.md"), filepath.Join(ownWT, "b.go"), filepath.Join(otherScratch, "c.md"),
		filepath.Join(home, "aeolian.db"), filepath.Join(userHome, "dotfile"), filepath.Join(loose, "d.md"),
	} {
		writeFile(t, p, "1\n")
	}
	// The user's home is itself a repository (dotfiles): its top level is
	// refused, so a loose file falls back to its own folder.
	if err := os.Mkdir(filepath.Join(userHome, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo, file := fakeRepo(t, "x/y.go", 3)
	ref := func(p string) WitnessClaim { return WitnessClaim{Evidence: []WitnessEvidence{{Ref: p + ":1"}}} }
	claims := []WitnessClaim{
		ref(file), ref(filepath.Join(own, "a.md")), ref(filepath.Join(ownWT, "b.go")),
		ref(filepath.Join(otherScratch, "c.md")), ref(filepath.Join(home, "aeolian.db")),
		ref(filepath.Join(userHome, "dotfile")), ref(filepath.Join(loose, "d.md")),
		{Evidence: []WitnessEvidence{{Ref: "cmd: cat /etc/passwd"}, {Ref: "https://x/y:1"}}},
		ref("/etc/hostname"),
	}
	got := evidenceReadRoots(claims, nil, home, userHome, []string{own, ownWT})
	want := []string{repo, own, ownWT, loose}
	if _, err := os.Stat("/etc/hostname"); err == nil {
		want = append(want, "/etc")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("roots\n got %q\nwant %q", got, want)
	}
	for _, bad := range []string{"/", userHome, home, otherScratch, filepath.Dir(userHome)} {
		if slices.Contains(got, bad) {
			t.Fatalf("refused root %s granted", bad)
		}
	}
}

func TestExcerptWindow(t *testing.T) {
	_, small := fakeRepo(t, "s.go", 20)
	x := excerpt(small+":5", nil)
	if n := strings.Count(x, "\n") + 1; n != 20 || !strings.Contains(x, "    5* line 5") || !strings.Contains(x, "    6  line 6") {
		t.Fatalf("a small file is excerpted whole, with the cited line marked:\n%s", x)
	}
	_, big := fakeRepo(t, "b.go", 1000)
	x = excerpt(big+":500-510", nil)
	lines := strings.Split(x, "\n")
	if len(lines) != 11+80 || !strings.HasPrefix(lines[0], "  460  ") || !strings.HasPrefix(lines[len(lines)-1], "  550  ") {
		t.Fatalf("window: %d lines, %q … %q", len(lines), lines[0], lines[len(lines)-1])
	}
	x = excerpt(big+":10-900", nil)
	if lines := strings.Split(x, "\n"); len(lines) != excerptSpan+40+9 {
		t.Fatalf("a long range is capped: %d lines", len(lines))
	}
	if c := capExcerpt(strings.Repeat("abcdefghi\n", 100), 55); !strings.HasSuffix(c, excerptCut) || len(c) > 55+len(excerptCut)+1 {
		t.Fatalf("cap: %q", c)
	}
}
