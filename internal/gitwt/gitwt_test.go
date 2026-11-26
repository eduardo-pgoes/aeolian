package gitwt

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newRepo makes a repo with one commit on main.
func newRepo(t *testing.T) string {
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

func TestAddAndRemoveClean(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt", "a")
	if err := Add(repo, path, "feat/a", "main"); err != nil {
		t.Fatal(err)
	}
	if !BranchExists(repo, "feat/a") {
		t.Fatal("branch not created")
	}
	if err := IsToplevel(path); err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	if !BranchExists(repo, "feat/a") {
		t.Fatal("remove deleted the branch")
	}
}

func TestAddRefusesExistingBranch(t *testing.T) {
	repo := newRepo(t)
	err := Add(repo, filepath.Join(t.TempDir(), "x"), "main", "main")
	if !errors.Is(err, ErrBranchExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestAddRefusesBadName(t *testing.T) {
	repo := newRepo(t)
	for _, name := range []string{"", "a..b", "-x", "a b", "x.lock"} {
		if err := Add(repo, filepath.Join(t.TempDir(), "x"), name, "main"); err == nil {
			t.Errorf("branch %q accepted", name)
		}
	}
}

func TestAddRefusesBadBase(t *testing.T) {
	repo := newRepo(t)
	if err := Add(repo, filepath.Join(t.TempDir(), "x"), "b", "nope"); err == nil {
		t.Fatal("bad base accepted")
	}
}

func TestRemoveDirtyKeeps(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "a")
	if err := Add(repo, path, "a", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Remove(repo, path); !errors.Is(err, ErrDirty) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "new.txt")); err != nil {
		t.Fatal("dirty worktree lost its file")
	}
}

func TestIsToplevel(t *testing.T) {
	repo := newRepo(t)
	sub := filepath.Join(repo, "sub")
	os.Mkdir(sub, 0o755)
	if err := IsToplevel(sub); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("subdir: %v", err)
	}
	if err := IsToplevel(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("non-repo: %v", err)
	}
}
