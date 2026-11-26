package host

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

const workflowExt = ".yaml"

// WorkflowEntry is one file in the workflows folder. Name is the file name
// without .yaml. Error is set when the file could not be read or parsed;
// Description and Steps are then empty, and Contents holds whatever was read.
type WorkflowEntry struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Steps       []string `json:"steps"`
	Contents    string   `json:"contents"`
	Error       string   `json:"error"`
}

// Workflows returns an entry for every .yaml file directly in the workflows
// folder, sorted by name; a missing folder has none.
func (h *Host) Workflows() ([]WorkflowEntry, error) {
	dir := h.Layout.Workflows()
	files, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []WorkflowEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := []WorkflowEntry{}
	for _, f := range files {
		name, ok := strings.CutSuffix(f.Name(), workflowExt)
		if !ok || name == "" || f.IsDir() {
			continue
		}
		entries = append(entries, workflowEntry(name, filepath.Join(dir, f.Name())))
	}
	slices.SortFunc(entries, func(a, b WorkflowEntry) int { return strings.Compare(a.Name, b.Name) })
	return entries, nil
}

func workflowEntry(name, path string) WorkflowEntry {
	e := WorkflowEntry{Name: name, Steps: []string{}}
	b, err := os.ReadFile(path)
	e.Contents = string(b)
	if err != nil {
		e.Error = err.Error()
		return e
	}
	wf, err := workflow.Parse(b)
	if err != nil {
		e.Error = err.Error()
		return e
	}
	e.Description = wf.Description
	for _, st := range wf.Steps {
		e.Steps = append(e.Steps, st.Name)
	}
	return e
}

func (h *Host) listWorkflows(w http.ResponseWriter, r *http.Request) {
	entries, err := h.Workflows()
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}
