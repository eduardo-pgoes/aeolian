package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/gitwt"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// ErrGone is returned when resuming an agent whose folders garbage
// collection removed.
var ErrGone = errors.New("its folders were garbage collected")

const (
	// DefaultGCAfter is how long after its ack a run is kept when GCAfter
	// is unset.
	DefaultGCAfter = 7 * 24 * time.Hour
	// GCInterval is how often AutoGC collects.
	GCInterval = time.Hour
)

func (h *Host) gcAfter() time.Duration {
	if h.GCAfter > 0 {
		return h.GCAfter
	}
	return DefaultGCAfter
}

// GCReport says what one GC pass removed, or with DryRun would remove.
type GCReport struct {
	DryRun bool     `json:"dry_run"`
	Runs   []string `json:"runs"`
	// Agents are the agents whose folders went.
	Agents  []string `json:"agents"`
	Removed []string `json:"removed"`
	// Kept are the worktrees left in place, with why.
	Kept      []KeptWorktree `json:"kept"`
	Logs      []string       `json:"logs"`
	Untrusted int            `json:"untrusted"`
	Bytes     int64          `json:"bytes"`
	Errors    []string       `json:"errors"`
}

type KeptWorktree struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Reason string `json:"reason"`
}

// GC removes what aeolian keeps for runs that are finished, acked, and
// were acked more than GCAfter before now: each agent's scratch and config
// folders, their trust entries, and the run's managed worktrees. A failed
// run's agent logs move to $AEOLIAN_HOME/logs first. A worktree with
// uncommitted or untracked files is kept; branches are never deleted; no
// path outside $AEOLIAN_HOME/scratch and $AEOLIAN_HOME/agents is ever
// removed, except managed worktrees through git. A run with an agent still
// live, one resumed since, is skipped. dryRun reports and changes nothing.
func (h *Host) GC(now time.Time, dryRun bool) (GCReport, error) {
	rep := GCReport{DryRun: dryRun, Runs: []string{}, Agents: []string{}, Removed: []string{}, Kept: []KeptWorktree{}, Logs: []string{}, Errors: []string{}}
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		return rep, err
	}
	cutoff := now.Add(-h.gcAfter())
	var due []store.Run
	for _, r := range runs {
		if r.AckedAt != nil && r.AckedAt.Before(cutoff) && slices.Contains(runFinishedStates, r.State) {
			due = append(due, r)
		}
	}
	if len(due) == 0 {
		return rep, nil
	}
	ids := make([]string, len(due))
	for i, r := range due {
		ids[i] = r.ID
	}
	agents, err := h.Store.AgentsOfRuns(ids)
	if err != nil {
		return rep, err
	}
	byRun := map[string][]store.Agent{}
	for _, a := range agents {
		byRun[a.RunID] = append(byRun[a.RunID], a)
	}
	untrust := map[string][]string{}
	for _, r := range due {
		if slices.ContainsFunc(byRun[r.ID], func(a store.Agent) bool { return !store.Finished(a.State) }) {
			continue
		}
		claudeJSON := h.Layout.ClaudeJSON
		if lay, err := h.accountLayout(r.Account); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("run %s: %v", r.ID, err))
		} else {
			claudeJSON = lay.ClaudeJSON
		}
		swept, bytes := 0, int64(0)
		for _, a := range byRun[r.ID] {
			if a.GCAt != nil {
				continue
			}
			n, dirs := h.gcAgent(&rep, r, a, now, dryRun)
			bytes += n
			untrust[claudeJSON] = append(untrust[claudeJSON], dirs...)
			swept++
		}
		wts, err := h.Store.ManagedWorktrees(r.ID)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("run %s worktrees: %v", r.ID, err))
			continue
		}
		for _, w := range wts {
			if w.State == store.WorktreeRemoved {
				continue
			}
			if h.gcWorktree(&rep, w, now, dryRun) {
				untrust[claudeJSON] = append(untrust[claudeJSON], w.Path)
			}
		}
		if swept == 0 {
			continue
		}
		rep.Runs = append(rep.Runs, r.ID)
		rep.Bytes += bytes
		if !dryRun {
			h.emit(r.ID, "", store.KindGCSwept, store.GCSwept{V: store.EventVersion, Agents: swept, Bytes: bytes})
		}
	}
	for _, path := range slices.Sorted(maps.Keys(untrust)) {
		if dryRun || path == "" || len(untrust[path]) == 0 {
			continue
		}
		n, err := agent.Untrust(path, h.Layout.Home, untrust[path])
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("untrust %s: %v", path, err))
		}
		rep.Untrusted += n
	}
	return rep, nil
}

var runFinishedStates = []string{store.StateDone, store.StateBlocked, store.StateFailed, store.StateKilled}

// gcAgent removes one agent's folders and returns the bytes they held and
// the dirs whose trust entries can go.
func (h *Host) gcAgent(rep *GCReport, r store.Run, a store.Agent, now time.Time, dryRun bool) (int64, []string) {
	home := h.Layout.Home
	scratch, config := h.Layout.ScratchDir(a.ID), h.Layout.ConfigDir(a.ID)
	bytes := dirSize(scratch) + dirSize(config)
	rep.Agents = append(rep.Agents, a.ID)
	if r.State == store.StateFailed {
		if _, err := os.Stat(h.Layout.Log(a.ID)); err == nil {
			rep.Logs = append(rep.Logs, h.Layout.KeptLog(a.ID))
			if !dryRun {
				if err := keepLog(h.Layout.Log(a.ID), h.Layout.KeptLog(a.ID)); err != nil {
					rep.Errors = append(rep.Errors, fmt.Sprintf("agent %s log: %v", a.ID, err))
					return 0, nil
				}
			}
		}
	}
	if dryRun {
		return bytes, nil
	}
	for _, d := range []struct{ root, path string }{
		{filepath.Join(home, "scratch"), scratch},
		{filepath.Join(home, "agents"), config},
	} {
		if err := removeUnder(d.root, d.path); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("agent %s: %v", a.ID, err))
			return 0, nil
		}
	}
	if err := h.Store.MarkGC(a.ID, now); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("agent %s: %v", a.ID, err))
	}
	return bytes, []string{scratch}
}

// gcWorktree removes one managed worktree through git, never forced, and
// reports whether it went. One that will not go is recorded kept the first
// time, which appends worktree.kept.
func (h *Host) gcWorktree(rep *GCReport, w store.ManagedWorktree, now time.Time, dryRun bool) bool {
	if dryRun {
		if _, err := os.Stat(w.Path); err == nil {
			if dirty, err := gitwt.Dirty(w.Path); err != nil || dirty {
				rep.Kept = append(rep.Kept, KeptWorktree{Path: w.Path, Branch: w.Branch, Reason: store.WorktreeDirty})
				return false
			}
		}
		rep.Removed = append(rep.Removed, w.Path)
		return false
	}
	mu := h.repoLock(w.Repo)
	mu.Lock()
	err := gitwt.Remove(w.Repo, w.Path)
	mu.Unlock()
	if err == nil {
		if err := h.Store.SetManagedWorktree(w.ID, store.WorktreeRemoved, "", now); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("worktree %s: %v", w.Path, err))
		}
		rep.Removed = append(rep.Removed, w.Path)
		return true
	}
	reason := store.WorktreeRemovalFailed
	if errors.Is(err, gitwt.ErrDirty) {
		reason = store.WorktreeDirty
	}
	rep.Kept = append(rep.Kept, KeptWorktree{Path: w.Path, Branch: w.Branch, Reason: reason})
	if w.State == store.WorktreeKept && w.Reason == reason {
		return false
	}
	log.Printf("gc: keeping worktree %s: %v", w.Path, err)
	if err := h.Store.SetManagedWorktree(w.ID, store.WorktreeKept, reason, now); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("worktree %s: %v", w.Path, err))
	}
	return false
}

// removeUnder removes path, which must be strictly inside root. A symlink
// is removed itself, never followed.
func removeUnder(root, path string) error {
	if !agent.Inside(root, path) {
		return fmt.Errorf("refusing to remove %s: not inside %s", path, root)
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}

// keepLog moves a failed agent's log out of its config folder.
func keepLog(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// tidyRunLater runs tidyRun once the run's last sessions have had their
// grace to close.
func (h *Host) tidyRunLater(runID string) {
	grace := h.KillGrace
	if grace == 0 {
		grace = DefaultKillGrace
	}
	time.Sleep(2 * grace)
	h.tidyRun(runID)
}

// tidyRun removes the scratch folders a run's finished agents left empty.
// It is cheap and safe: Resume makes the folder again.
func (h *Host) tidyRun(runID string) {
	agents, err := h.Store.AgentsOfRuns([]string{runID})
	if err != nil {
		log.Printf("tidy run %s: %v", runID, err)
		return
	}
	for _, a := range agents {
		if !store.Finished(a.State) || a.Dir != h.Layout.ScratchDir(a.ID) {
			continue
		}
		// os.Remove takes only an empty folder.
		if err := os.Remove(a.Dir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			log.Printf("tidy run %s: %v", runID, err)
		}
	}
}

// gcLoop collects now and then every GCInterval until ctx ends.
func (h *Host) gcLoop(ctx context.Context) {
	t := time.NewTicker(GCInterval)
	defer t.Stop()
	for {
		rep, err := h.GC(time.Now(), false)
		switch {
		case err != nil:
			log.Printf("gc: %v", err)
		case len(rep.Runs) > 0 || len(rep.Errors) > 0:
			log.Printf("gc: %d runs, %d agents, %d bytes, %d worktrees removed, %d kept, %d errors",
				len(rep.Runs), len(rep.Agents), rep.Bytes, len(rep.Removed), len(rep.Kept), len(rep.Errors))
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

func (h *Host) postGC(w http.ResponseWriter, r *http.Request) {
	dry := r.URL.Query().Get("dry_run")
	rep, err := h.GC(time.Now(), dry == "1" || dry == "true")
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}
