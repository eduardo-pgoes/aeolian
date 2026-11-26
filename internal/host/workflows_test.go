package host

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (f *fixture) workflows(t *testing.T) (int, string) {
	t.Helper()
	resp, err := http.Get(f.srv.URL + "/api/workflows")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func writeWorkflow(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowsMissingFolder(t *testing.T) {
	f := newFixture(t)
	code, body := f.workflows(t)
	if code != http.StatusOK || strings.TrimSpace(body) != "[]" {
		t.Fatalf("workflows: %d %q", code, body)
	}
}

func TestWorkflowsListsFolder(t *testing.T) {
	f := newFixture(t)
	dir := f.host.Layout.Workflows()
	if err := os.MkdirAll(filepath.Join(dir, "nested.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	described := "description: Build then check.\nsteps:\n  - name: build\n    prompt: Build.\n  - name: check\n    prompt: Check.\n"
	plain := "steps:\n  - name: only\n    prompt: Go.\n"
	broken := "steps: [\n"
	writeWorkflow(t, dir, "zeta.yaml", plain)
	writeWorkflow(t, dir, "alpha.yaml", described)
	writeWorkflow(t, dir, "mid.yaml", broken)
	writeWorkflow(t, dir, "alpha-two.yaml", plain)
	writeWorkflow(t, dir, "notes.txt", plain)
	writeWorkflow(t, dir, "other.yml", plain)

	code, body := f.workflows(t)
	if code != http.StatusOK {
		t.Fatalf("workflows: %d %s", code, body)
	}
	if !strings.Contains(body, `"steps":[]`) || strings.Contains(body, "null") {
		t.Fatalf("broken entry steps must be []: %s", body)
	}
	var got []WorkflowEntry
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("want 4 entries, got %+v", got)
	}
	if got[1].Name != "alpha-two" {
		t.Fatalf("alpha-two sorts after alpha: %+v", got)
	}
	a, m, z := got[0], got[2], got[3]
	if a.Name != "alpha" || m.Name != "mid" || z.Name != "zeta" {
		t.Fatalf("order: %s %s %s", a.Name, m.Name, z.Name)
	}
	if a.Description != "Build then check." || strings.Join(a.Steps, ",") != "build,check" || a.Contents != described || a.Error != "" {
		t.Fatalf("alpha: %+v", a)
	}
	if z.Description != "" || strings.Join(z.Steps, ",") != "only" || z.Error != "" {
		t.Fatalf("zeta: %+v", z)
	}
	if m.Error == "" || m.Contents != broken || len(m.Steps) != 0 {
		t.Fatalf("mid: %+v", m)
	}
}

func TestWorkflowsUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	f := newFixture(t)
	dir := f.host.Layout.Workflows()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkflow(t, dir, "ok.yaml", "steps:\n  - name: only\n    prompt: Go.\n")
	writeWorkflow(t, dir, "locked.yaml", "steps: []\n")
	if err := os.Chmod(filepath.Join(dir, "locked.yaml"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := f.host.Workflows()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "locked" || !strings.Contains(got[0].Error, "permission denied") || got[1].Error != "" {
		t.Fatalf("workflows: %+v", got)
	}
}

func TestWorkflowsFolderUnreadable(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.host.Layout.Workflows(), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, body := f.workflows(t); code != http.StatusInternalServerError {
		t.Fatalf("workflows over a file: %d %s", code, body)
	}
}
