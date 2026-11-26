package qa

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Task struct {
	RFC      string
	Slug     string
	Dir      string
	Title    string
	Repo     string
	Base     string
	After    string
	Account  string
	Status   string
	Promoted bool
}

func (t Task) ID() string { return t.RFC + "/" + t.Slug }

func (t Task) Branch() string { return "ekky/" + t.RFC + "-" + t.Slug }

func (t Task) UpstreamBranch() string {
	if t.After == "" {
		return ""
	}
	return "ekky/" + t.RFC + "-" + t.After
}

func Scan(root string) ([]Task, error) {
	rfcs, err := os.ReadDir(filepath.Join(root, "agent-tasks"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Task
	for _, r := range rfcs {
		if !r.IsDir() || strings.HasPrefix(r.Name(), ".") {
			continue
		}
		rfcDir := filepath.Join(root, "agent-tasks", r.Name())
		promoted := promotedSlugs(filepath.Join(rfcDir, "promote.md"))
		slugs, err := os.ReadDir(filepath.Join(rfcDir, "tasks"))
		if err != nil {
			continue
		}
		for _, s := range slugs {
			if !s.IsDir() {
				continue
			}
			dir := filepath.Join(rfcDir, "tasks", s.Name())
			fields, title, err := readRecord(filepath.Join(dir, "task.md"))
			if err != nil {
				continue
			}
			out = append(out, Task{
				RFC: r.Name(), Slug: s.Name(), Dir: dir, Title: title,
				Repo: fields["repo"], Base: fields["base"], After: fields["after"], Account: fields["account"],
				Status:   status(dir),
				Promoted: promoted[s.Name()],
			})
		}
	}
	return out, nil
}

func status(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "status"))
	if err != nil {
		return "new"
	}
	return strings.TrimSpace(string(b))
}

func readRecord(path string) (map[string]string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	fields := map[string]string{}
	title := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	inFront, first := false, true
	for sc.Scan() {
		line := sc.Text()
		if first {
			first = false
			inFront = line == "---"
			continue
		}
		if inFront {
			if line == "---" {
				inFront = false
				continue
			}
			if k, v, ok := strings.Cut(line, ": "); ok {
				if _, seen := fields[k]; !seen {
					fields[k] = strings.TrimSpace(v)
				}
			}
			continue
		}
		if title == "" && strings.HasPrefix(line, "# ") {
			title = strings.TrimPrefix(line, "# ")
		}
	}
	return fields, title, sc.Err()
}

func promotedSlugs(ledger string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(ledger)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "branch ")
		if !ok {
			continue
		}
		if slug, _, ok := strings.Cut(rest, ":"); ok {
			out[strings.TrimSpace(slug)] = true
		}
	}
	return out
}

func Head(repo, branch string) (string, bool) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}
