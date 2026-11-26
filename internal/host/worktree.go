package host

import (
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/gitwt"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// WorktreeRequest asks the host for a new git worktree on a new branch.
type WorktreeRequest struct {
	Repo   string `json:"repo" jsonschema:"a repos key from the host config, or the absolute path of a git checkout"`
	Branch string `json:"branch,omitempty" jsonschema:"new branch name; default aeolian/<run>/<role>-<id>"`
	Base   string `json:"base,omitempty" jsonschema:"commit-ish to branch from; default the repo's configured base, else its HEAD"`
}

// worktreePlan is a WorktreeRequest resolved to paths, not yet made.
type worktreePlan struct {
	repo, repoKey, path, branch, base string
}

func (p worktreePlan) worktree() store.Worktree {
	return store.Worktree{Repo: p.repo, Path: p.path, Branch: p.branch}
}

// worktreeRoot is where the host makes worktrees.
func (h *Host) worktreeRoot() string {
	if h.WorktreeRoot != "" {
		return h.WorktreeRoot
	}
	return filepath.Join(h.Layout.Home, "worktrees")
}

var unsafePathChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// pathSafe turns s into one path element.
func pathSafe(s string) string {
	s = strings.Trim(unsafePathChars.ReplaceAllString(s, "-"), "-.")
	if s == "" {
		return "x"
	}
	return s
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// resolveRepo finds the main checkout req names: a key of config's repos,
// or the absolute path of a checkout's top.
func (h *Host) resolveRepo(name string) (path, key, base string, err error) {
	if r, ok := h.Repos[name]; ok {
		return r.Path, name, r.Base, nil
	}
	if !filepath.IsAbs(name) {
		keys := make([]string, 0, len(h.Repos))
		for k := range h.Repos {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		return "", "", "", fmt.Errorf("repo %q is neither a configured repo (%s) nor an absolute path", name, strings.Join(keys, ", "))
	}
	path = filepath.Clean(name)
	if err := gitwt.IsToplevel(path); err != nil {
		return "", "", "", err
	}
	for k, r := range h.Repos {
		if filepath.Clean(r.Path) == path {
			return path, k, r.Base, nil
		}
	}
	return path, "", "", nil
}

// planWorktree resolves req for requester. label names the branch when req
// has none: aeolian/<run>/<label>.
func (h *Host) planWorktree(requester store.Agent, label string, req WorktreeRequest) (worktreePlan, error) {
	if req.Repo == "" {
		return worktreePlan{}, errors.New("worktree needs a repo")
	}
	repo, key, base, err := h.resolveRepo(req.Repo)
	if err != nil {
		return worktreePlan{}, err
	}
	if req.Base != "" {
		base = req.Base
	}
	if base == "" {
		base = "HEAD"
	}
	branch := req.Branch
	if branch == "" {
		branch = "aeolian/" + pathSafe(label)
		if requester.RunID != "" {
			branch = "aeolian/" + short(requester.RunID) + "/" + pathSafe(label)
		}
	}
	if err := gitwt.CheckBranch(repo, branch); err != nil {
		return worktreePlan{}, err
	}
	if gitwt.BranchExists(repo, branch) {
		return worktreePlan{}, fmt.Errorf("branch %s exists in %s; pick another name", branch, repo)
	}
	dir := filepath.Join(h.worktreeRoot(), "_", short(requester.ID))
	if requester.RunID != "" {
		dir = filepath.Join(h.worktreeRoot(), short(requester.RunID))
	}
	return worktreePlan{repo: repo, repoKey: key, path: filepath.Join(dir, pathSafe(branch)), branch: branch, base: base}, nil
}

// repoLock is the mutex that serializes git worktree commands on one repo:
// two at once race on .git/worktrees and index.lock.
func (h *Host) repoLock(repo string) *sync.Mutex {
	m, _ := h.repoLocks.LoadOrStore(repo, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// makeWorktree creates p for requester, trusts it, records it as the
// host's and emits worktree.created. It runs git outside h.mu.
func (h *Host) makeWorktree(requester store.Agent, p worktreePlan) (store.ManagedWorktree, error) {
	mu := h.repoLock(p.repo)
	mu.Lock()
	err := gitwt.Add(p.repo, p.path, p.branch, p.base)
	mu.Unlock()
	if err != nil {
		return store.ManagedWorktree{}, err
	}
	if lay, err := h.agentLayout(requester); err != nil {
		log.Printf("trust worktree %s: %v", p.path, err)
	} else if lay.ClaudeJSON != "" {
		if err := agent.Trust(lay.ClaudeJSON, p.path); err != nil {
			log.Printf("trust worktree %s: %v", p.path, err)
		}
	}
	w, err := h.Store.CreateManagedWorktree(store.ManagedWorktree{
		RunID: requester.RunID, AgentID: requester.ID, Repo: p.repo, RepoKey: p.repoKey,
		Path: p.path, Branch: p.branch, Base: p.base,
	})
	if err != nil {
		return store.ManagedWorktree{}, fmt.Errorf("worktree %s was made but not recorded: %w", p.path, err)
	}
	return w, nil
}
