package qa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type fakeHost struct {
	mu     sync.Mutex
	starts []map[string]string
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/runs":
		if r.URL.Query().Get("source") != Source {
			http.Error(w, "wrong source", http.StatusBadRequest)
			return
		}
		var runs []map[string]string
		for _, s := range f.starts {
			runs = append(runs, map[string]string{"ref": s["ref"]})
		}
		json.NewEncoder(w).Encode(runs)
	case r.Method == http.MethodPost && r.URL.Path == "/api/runs":
		var req map[string]string
		json.NewDecoder(r.Body).Decode(&req)
		f.starts = append(f.starts, req)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": "r", "state": "queued"})
	default:
		http.NotFound(w, r)
	}
}

func TestWatcherStartsOneRunPerWorkTaskHead(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	perso := filepath.Join(tmp, "perso")
	for _, repo := range []string{work, perso} {
		os.MkdirAll(repo, 0o755)
		git(t, repo, "init", "-q", "-b", "develop")
		git(t, repo, "commit", "-q", "--allow-empty", "-m", "base")
	}
	git(t, work, "branch", "ekky/batch-01-api")
	git(t, work, "branch", "ekky/batch-02-ui", "ekky/batch-01-api")
	git(t, work, "branch", "ekky/batch-03-shipped")
	git(t, perso, "branch", "ekky/side-01-thing")

	root := filepath.Join(tmp, "ekky")
	task := func(rfc, slug, repo, status, extra string) {
		dir := filepath.Join(root, "agent-tasks", rfc, "tasks", slug)
		write(t, filepath.Join(dir, "task.md"), "---\nrepo: "+repo+"\nbase: develop\naccount: work\n"+extra+"---\n# Title of "+slug+"\n\n## Done looks like\nit works\n")
		write(t, filepath.Join(dir, "status"), status+"\n")
	}
	task("batch", "01-api", work, "done", "")
	task("batch", "02-ui", work, "done", "after: 01-api\n")
	task("batch", "03-shipped", work, "done", "")
	task("batch", "04-running", work, "running", "")
	task("side", "01-thing", perso, "done", "")
	write(t, filepath.Join(root, "agent-tasks", "batch", "promote.md"), "mode: task\nbranch 03-shipped: feat/shipped\n")
	write(t, filepath.Join(tmp, "qa-ekky.yaml"), "steps:\n  - name: test\n    prompt: \"{{.Goal}}\"\n")

	host := &fakeHost{}
	srv := httptest.NewServer(host)
	defer srv.Close()
	w := &Watcher{
		HostURL: srv.URL, EkkyRoot: root, Workflow: filepath.Join(tmp, "qa-ekky.yaml"), BundleRoot: filepath.Join(tmp, "qa-runs"),
		WorkRepos: map[string]bool{work: true}, Accounts: map[string]bool{"work": true}, Interval: time.Hour, Logf: t.Logf,
	}
	ctx := context.Background()
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.starts) != 2 {
		t.Fatalf("want runs for 01-api and 02-ui once each, got %d: %v", len(host.starts), host.starts)
	}
	head := git(t, work, "rev-parse", "ekky/batch-01-api")
	first := host.starts[0]
	if first["ref"] != "batch/01-api@"+head[:12] || first["account"] != "work" || first["source"] != Source {
		t.Fatalf("first start: %v", first)
	}
	if !strings.Contains(first["goal"], "Diff: git -C "+work+" diff origin/develop..."+head) {
		t.Fatalf("independent task diffs from its base: %s", first["goal"])
	}
	if !strings.Contains(host.starts[1]["goal"], "diff ekky/batch-01-api...") || !strings.Contains(host.starts[1]["goal"], "Upstream task: batch/01-api") {
		t.Fatalf("dependent task diffs from its upstream: %s", host.starts[1]["goal"])
	}
	if !strings.Contains(first["goal"], "Bundle: "+filepath.Join(tmp, "qa-runs", "ekky-batch-01-api")) {
		t.Fatalf("bundle: %s", first["goal"])
	}

	git(t, work, "commit", "-q", "--allow-empty", "-m", "rework")
	git(t, work, "branch", "-f", "ekky/batch-01-api", "HEAD")
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(host.starts) != 3 {
		t.Fatalf("a reworked head gets a fresh run: %v", host.starts)
	}
	newHead := git(t, work, "rev-parse", "ekky/batch-01-api")
	if host.starts[2]["ref"] != "batch/01-api@"+newHead[:12] {
		t.Fatalf("rework start: %v", host.starts[2])
	}
}
