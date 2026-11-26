package qa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Source = "ekky-qa"

type Watcher struct {
	HostURL    string
	EkkyRoot   string
	Workflow   string
	BundleRoot string
	WorkRepos  map[string]bool
	Accounts   map[string]bool
	Interval   time.Duration
	HTTP       *http.Client
	Logf       func(string, ...any)
}

type Start struct {
	Task Task
	Head string
	Ref  string
	Goal string
}

func (w *Watcher) Ref(t Task, head string) string { return t.ID() + "@" + head[:12] }

func (w *Watcher) Bundle(t Task) string {
	return filepath.Join(w.BundleRoot, "ekky-"+t.RFC+"-"+t.Slug)
}

func (w *Watcher) Goal(t Task, head string) string {
	from := "origin/" + t.Base
	upstream := "none"
	if t.After != "" {
		from = t.UpstreamBranch()
		upstream = t.RFC + "/" + t.After
	}
	var b strings.Builder
	fmt.Fprintf(&b, "QA the ekky task %s: %s\n\n", t.ID(), t.Title)
	fmt.Fprintf(&b, "Task: %s\n", t.ID())
	fmt.Fprintf(&b, "Title: %s\n", t.Title)
	fmt.Fprintf(&b, "Record: %s\n", filepath.Join(t.Dir, "task.md"))
	fmt.Fprintf(&b, "Result: %s\n", filepath.Join(t.Dir, "result.md"))
	fmt.Fprintf(&b, "Repo: %s\n", t.Repo)
	fmt.Fprintf(&b, "Base: %s\n", t.Base)
	fmt.Fprintf(&b, "Branch: %s\n", t.Branch())
	fmt.Fprintf(&b, "Head: %s\n", head)
	fmt.Fprintf(&b, "Upstream task: %s\n", upstream)
	fmt.Fprintf(&b, "Diff: git -C %s diff %s...%s\n", t.Repo, from, head)
	fmt.Fprintf(&b, "Ledger: %s\n", filepath.Join(w.EkkyRoot, "agent-tasks", t.RFC, "promote.md"))
	fmt.Fprintf(&b, "Bundle: %s\n", w.Bundle(t))
	return b.String()
}

func (w *Watcher) Due(tasks []Task, existing map[string]bool) []Start {
	var out []Start
	for _, t := range tasks {
		if t.Status != "done" || t.Promoted || !w.WorkRepos[filepath.Clean(t.Repo)] {
			continue
		}
		head, ok := Head(t.Repo, t.Branch())
		if !ok {
			continue
		}
		ref := w.Ref(t, head)
		if existing[ref] {
			continue
		}
		out = append(out, Start{Task: t, Head: head, Ref: ref, Goal: w.Goal(t, head)})
	}
	return out
}

func (w *Watcher) Poll(ctx context.Context) {
	for {
		if err := w.Once(ctx); err != nil {
			w.Logf("poll: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(w.Interval):
		}
	}
}

func (w *Watcher) Once(ctx context.Context) error {
	tasks, err := Scan(w.EkkyRoot)
	if err != nil {
		return err
	}
	existing, err := w.Existing(ctx)
	if err != nil {
		return err
	}
	due := w.Due(tasks, existing)
	if len(due) == 0 {
		return nil
	}
	wf, err := os.ReadFile(w.Workflow)
	if err != nil {
		return err
	}
	for _, s := range due {
		req := map[string]string{"workflow": string(wf), "goal": s.Goal, "source": Source, "ref": s.Ref}
		if w.Accounts[s.Task.Account] {
			req["account"] = s.Task.Account
		}
		var run struct {
			ID    string `json:"id"`
			State string `json:"state"`
		}
		if err := w.do(ctx, http.MethodPost, "/api/runs", req, &run); err != nil {
			w.Logf("%s: start: %v", s.Ref, err)
			continue
		}
		w.Logf("%s: run %s %s", s.Ref, run.ID, run.State)
	}
	return nil
}

func (w *Watcher) Existing(ctx context.Context) (map[string]bool, error) {
	var runs []struct {
		Ref string `json:"ref"`
	}
	if err := w.do(ctx, http.MethodGet, "/api/runs?"+url.Values{"source": {Source}}.Encode(), nil, &runs); err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, r := range runs {
		existing[r.Ref] = true
	}
	return existing, nil
}

func (w *Watcher) do(ctx context.Context, method, path string, body, out any) error {
	var in io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.HostURL, "/")+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c := w.HTTP
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("is the host running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
