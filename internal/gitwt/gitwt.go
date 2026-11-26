// Package gitwt makes and removes git worktrees by running git. It never
// forces a removal and never deletes a branch: a worktree with changes in
// it stays, and commits stay on their branch.
package gitwt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	// ErrBranchExists is returned when the branch to create is there
	// already.
	ErrBranchExists = errors.New("branch exists")
	// ErrDirty is returned when removing a worktree that has uncommitted
	// or untracked files.
	ErrDirty = errors.New("worktree has uncommitted or untracked files")
	// ErrNotRepo is returned when a path is not the top of a git checkout.
	ErrNotRepo = errors.New("not the top of a git checkout")
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// The host's own environment must not point git elsewhere.
	cmd.Env = cleanEnv()
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR":
			continue
		}
		env = append(env, kv)
	}
	return env
}

// Toplevel returns the top of the checkout dir is in.
func Toplevel(dir string) (string, error) {
	return git(dir, "rev-parse", "--show-toplevel")
}

func MainCheckout(dir string) (string, error) {
	common, err := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return common, nil
}

// IsToplevel reports whether path is the top of a git checkout.
func IsToplevel(path string) error {
	top, err := Toplevel(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, ErrNotRepo)
	}
	a, err1 := filepath.EvalSymlinks(top)
	b, err2 := filepath.EvalSymlinks(path)
	if err1 != nil || err2 != nil || a != b {
		return fmt.Errorf("%s: %w", path, ErrNotRepo)
	}
	return nil
}

// CheckBranch fails when name is not a valid new branch name.
func CheckBranch(repo, name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("branch %q is not a valid name", name)
	}
	if _, err := git(repo, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("branch %q is not a valid name", name)
	}
	return nil
}

// BranchExists reports whether refs/heads/name exists in repo.
func BranchExists(repo, name string) bool {
	_, err := git(repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
	return err == nil
}

// Add creates branch from base and checks it out in a new worktree at
// path. It refuses a branch that exists and a path that exists.
func Add(repo, path, branch, base string) error {
	if err := CheckBranch(repo, branch); err != nil {
		return err
	}
	if BranchExists(repo, branch) {
		return fmt.Errorf("%s: %w", branch, ErrBranchExists)
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s exists already", path)
	}
	if base == "" {
		base = "HEAD"
	}
	if _, err := git(repo, "rev-parse", "--verify", "--quiet", base+"^{commit}"); err != nil {
		return fmt.Errorf("base %q is not a commit in %s", base, repo)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := git(repo, "worktree", "add", "-b", branch, "--", path, base)
	return err
}

// Dirty reports whether the worktree at path has uncommitted or untracked
// files.
func Dirty(path string) (bool, error) {
	out, err := git(path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Remove removes the worktree at path from repo, never with --force: a
// dirty worktree returns ErrDirty and stays. Its branch always stays.
//
// A worktree whose folder is gone already is pruned from repo's list.
func Remove(repo, path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		_, err := git(repo, "worktree", "prune")
		return err
	}
	dirty, err := Dirty(path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%s: %w", path, ErrDirty)
	}
	if _, err := git(repo, "worktree", "remove", "--", path); err != nil {
		return err
	}
	_, err = git(repo, "worktree", "prune")
	return err
}
