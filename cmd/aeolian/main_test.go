package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/github"
	"github.com/eduardo-pgoes/aeolian/internal/host"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

const exampleConfig = `{
  "max_agents": 5,
  "repos": {
    "api": {
      "path": "/home/me/src/api",
      "github": "acme/api",
      "base": "main",
      "description": "Go service behind the mobile app"
    },
    "web": {
      "path": "/home/me/src/web",
      "github": "acme/web",
      "base": "develop",
      "description": "Next.js storefront"
    }
  },
  "github": {
    "app_id": 123456,
    "private_key_file": "github-app.pem"
  }
}`

func writeConfig(t *testing.T, src string) agent.Layout {
	t.Helper()
	l := agent.Layout{Home: t.TempDir()}
	if err := os.WriteFile(filepath.Join(l.Home, "config.json"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return l
}

func writeKey(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, mode)
}

func TestLoadConfigReposAndGitHub(t *testing.T) {
	l := writeConfig(t, exampleConfig)
	cfg, err := loadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if r := cfg.Repos["web"]; r.Path != "/home/me/src/web" || r.GitHub != "acme/web" || r.Base != "develop" || r.Description == "" {
		t.Fatalf("web: %+v", r)
	}
	if cfg.GitHub.AppID != 123456 || cfg.GitHub.PrivateKeyFile != filepath.Join(l.Home, "github-app.pem") {
		t.Fatalf("github: %+v", cfg.GitHub)
	}
	if _, err := cfg.githubApp(); err == nil {
		t.Fatal("app without a key file")
	}
	writeKey(t, cfg.GitHub.PrivateKeyFile, 0o644)
	if _, err := cfg.githubApp(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("loose key: %v", err)
	}
	os.Chmod(cfg.GitHub.PrivateKeyFile, 0o600)
	app, err := cfg.githubApp()
	if err != nil {
		t.Fatal(err)
	}
	if app.ID != 123456 || !slices.Equal(app.Repos, []string{"acme/api", "acme/web"}) {
		t.Fatalf("app: %+v", app)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"max_agents": 3}`))
	if err != nil || cfg.MaxAgents != 3 || cfg.MaxRuns != host.DefaultMaxRuns || cfg.EventsRetentionDays != 30 || cfg.GitHub != nil || len(cfg.Repos) != 0 {
		t.Fatalf("%+v %v", cfg, err)
	}
	if app, err := cfg.githubApp(); app != nil || err != nil {
		t.Fatalf("app %v %v", app, err)
	}
}

func TestLoadConfigBoot(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"boot": {"default": "lean", "roles": {"reviewer": "full"}, "confine": []}}`))
	if err != nil || cfg.Boot.Default != "lean" || cfg.Boot.Roles["reviewer"] != "full" || cfg.Boot.Confine == nil || len(cfg.Boot.Confine) != 0 {
		t.Fatalf("%+v %v", cfg.Boot, err)
	}
}

func TestParseReplayArgsBoot(t *testing.T) {
	r, err := parseReplayArgs([]string{"cases"})
	if err != nil || r.boot != "lean" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := parseReplayArgs([]string{"--boot", "full", "cases"}); err != nil || r.boot != "full" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := parseReplayArgs([]string{"--boot", "bare", "cases"}); err == nil {
		t.Fatal("--boot bare passed")
	}
}

func TestLoadConfigMaxRuns(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"max_runs": 3}`))
	if err != nil || cfg.MaxRuns != 3 {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestLoadConfigEventsRetention(t *testing.T) {
	for src, want := range map[string]int{`{"events_retention_days": 7}`: 7, `{"events_retention_days": 0}`: 0} {
		cfg, err := loadConfig(writeConfig(t, src))
		if err != nil || cfg.EventsRetentionDays != want {
			t.Fatalf("%s: %+v %v", src, cfg, err)
		}
	}
}

func TestLoadConfigRefuses(t *testing.T) {
	repo := `"r": {"path": "/src/r", "github": "acme/r", "base": "main"}`
	for name, src := range map[string]string{
		"relative path":  `{"repos": {"r": {"path": "src/r", "github": "acme/r", "base": "main"}}}`,
		"bad github":     `{"repos": {"r": {"path": "/src/r", "github": "acme", "base": "main"}}}`,
		"nested github":  `{"repos": {"r": {"path": "/src/r", "github": "a/b/c", "base": "main"}}}`,
		"no base":        `{"repos": {"r": {"path": "/src/r", "github": "acme/r"}}}`,
		"no app id":      `{"repos": {` + repo + `}, "github": {"private_key_file": "k.pem"}}`,
		"no key file":    `{"repos": {` + repo + `}, "github": {"app_id": 1}}`,
		"github no repo": `{"github": {"app_id": 1, "private_key_file": "k.pem"}}`,
		"zero max_runs":  `{"max_runs": 0}`,
		"negative runs":  `{"max_runs": -1}`,
		"relative hook":  `{"on_finish": ["bin/notify"]}`,
		"empty hook":     `{"on_finish": [""]}`,
		"negative days":  `{"events_retention_days": -1}`,
		"bad boot":       `{"boot": {"default": "bare"}}`,
		"bad role boot":  `{"boot": {"roles": {"judge": "slim"}}}`,
	} {
		if _, err := loadConfig(writeConfig(t, src)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}

func TestLoadConfigOnFinish(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"on_finish": ["/usr/local/bin/notify", "--channel", "runs"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.OnFinish, []string{"/usr/local/bin/notify", "--channel", "runs"}) {
		t.Fatalf("on_finish %q", cfg.OnFinish)
	}
}

func TestLoadConfigRefusesLinear(t *testing.T) {
	const want = "config.json: the host no longer reads the linear section; move it to linear-adapter.json"
	for _, src := range []string{
		`{"linear": {"token_file": "linear.key", "assignee": "me@acme.dev", "ticket_workflow": "ticket-to-pr.yaml"}}`,
		`{"linear": {}}`,
		`{"linear": null}`,
	} {
		if _, err := loadConfig(writeConfig(t, src)); err == nil || err.Error() != want {
			t.Errorf("%s: %v", src, err)
		}
	}
}

func TestResolveBotRefusesOnlyBadConfig(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		app, user int
		refuse    bool
	}{
		"resolved":       {http.StatusOK, http.StatusOK, false},
		"bad app key":    {http.StatusUnauthorized, 0, true},
		"no bot user":    {http.StatusOK, http.StatusNotFound, true},
		"rate limited":   {http.StatusOK, http.StatusForbidden, false},
		"github is down": {http.StatusBadGateway, 0, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/app" {
					w.WriteHeader(c.app)
					w.Write([]byte(`{"slug":"acme-agent"}`))
					return
				}
				w.WriteHeader(c.user)
				w.Write([]byte(`{"login":"acme-agent[bot]","id":55501,"type":"Bot"}`))
			}))
			defer srv.Close()
			err := resolveBot(&github.App{ID: 1, Key: k, API: srv.URL})
			if (err != nil) != c.refuse {
				t.Fatalf("err %v, want refusal %v", err, c.refuse)
			}
		})
	}
}

func TestRunArgsRefuseReservedEnv(t *testing.T) {
	_, _, err := runArgs([]string{"--env", "GH_TOKEN=y", "wf.yaml", "g"})
	if err == nil || err.Error() != "env: GH_TOKEN is reserved by aeolian" {
		t.Fatalf("got %v", err)
	}
	_, _, err = runArgs([]string{"--git-config", "pushurl=/x", "wf.yaml", "g"})
	if err == nil || err.Error() != "git_config: pushurl is not a git config key" {
		t.Fatalf("got %v", err)
	}
	_, _, err = runArgs([]string{"--git-config", "credential.helper=store", "wf.yaml", "g"})
	if err == nil || err.Error() != "git_config: credential.helper is set by aeolian" {
		t.Fatalf("got %v", err)
	}
}

func TestRunArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		path string
		req  host.RunRequest
	}{
		{[]string{"wf.yaml", "the goal"}, "wf.yaml", host.RunRequest{Goal: "the goal"}},
		{[]string{"--source", "slack-docs", "--ref", "t1", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", Source: "slack-docs", Ref: "t1"}},
		{[]string{"--ref=t1", "wf.yaml", "-g"}, "wf.yaml", host.RunRequest{Goal: "-g", Ref: "t1"}},
		{[]string{"--account", "work", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", Account: "work"}},
		{[]string{"--context-cap", "150000", "--cap-kills", "2", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", ContextCap: 150000, CapKills: 2}},
		{[]string{"--context-cap=90000", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", ContextCap: 90000}},
		{[]string{"--token-cap", "2000000", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", TokenCap: 2000000}},
		{[]string{"--dir", "/srv/repo", "wf.yaml", "g"}, "wf.yaml", host.RunRequest{Goal: "g", Dir: "/srv/repo"}},
		{
			[]string{"--env", "AYA_ITEM=x", "--env", "B=c=d", "--git-config", "remote.origin.pushurl=/nonexistent", "--git-config", "user.name=a=b", "wf.yaml", "g"},
			"wf.yaml",
			host.RunRequest{
				Goal:      "g",
				Env:       map[string]string{"AYA_ITEM": "x", "B": "c=d"},
				GitConfig: [][2]string{{"remote.origin.pushurl", "/nonexistent"}, {"user.name", "a=b"}},
			},
		},
	} {
		path, req, err := runArgs(c.args)
		if err != nil || path != c.path || !reflect.DeepEqual(req, c.req) {
			t.Fatalf("%q: %q %+v %v", c.args, path, req, err)
		}
	}
	for _, bad := range [][]string{
		{"wf.yaml"},
		{"wf.yaml", "g", "extra"},
		{"--sauce", "x", "wf.yaml", "g"},
		{"--source", "x", "wf.yaml"},
		{"--env", "NOEQUALS", "wf.yaml", "g"},
		{"--git-config", "nokey", "wf.yaml", "g"},
		{"--git-config", "=v", "wf.yaml", "g"},
		{"--env", "=v", "wf.yaml", "g"},
		{"--context-cap", "lots", "wf.yaml", "g"},
	} {
		if _, _, err := runArgs(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestRunArgsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for args, want := range map[string]string{
		"--dir=~/repo":  filepath.Join(home, "repo"),
		"--dir ~/repo/": filepath.Join(home, "repo"),
		"--dir rel/x":   filepath.Join(cwd, "rel/x"),
		"--dir /srv/a":  "/srv/a",
	} {
		_, req, err := runArgs(append(strings.Fields(args), "wf.yaml", "g"))
		if err != nil || req.Dir != want {
			t.Fatalf("%s: %q %v, want %q", args, req.Dir, err, want)
		}
	}
}

func TestInboxArgs(t *testing.T) {
	for args, want := range map[string]string{"": "", "--source linear": "linear", "--source=slack-docs": "slack-docs"} {
		got, err := inboxArgs(strings.Fields(args))
		if err != nil || got != want {
			t.Fatalf("%q: %q %v", args, got, err)
		}
	}
	for _, bad := range []string{"extra", "--sauce x", "--source"} {
		if _, err := inboxArgs(strings.Fields(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestEndArgs(t *testing.T) {
	for args, want := range map[string]struct {
		id    string
		force bool
	}{"r1": {"r1", false}, "--force r1": {"r1", true}, "-force=true r2": {"r2", true}} {
		id, force, err := endArgs(strings.Fields(args))
		if err != nil || id != want.id || force != want.force {
			t.Fatalf("%q: %q %v %v", args, id, force, err)
		}
	}
	for _, bad := range []string{"", "a b", "--hard r1", "r1 --force"} {
		if _, _, err := endArgs(strings.Fields(bad)); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestDigestArgs(t *testing.T) {
	o, err := digestArgs(nil)
	if err != nil || o.since != "12h" || o.max != 7 || o.md || o.all {
		t.Fatalf("defaults %+v %v", o, err)
	}
	o, err = digestArgs([]string{"--since", "2d", "--md", "--run", "r1", "--max", "0", "--all"})
	if err != nil || o.since != "2d" || !o.md || o.run != "r1" || o.max != 0 || !o.all {
		t.Fatalf("flags %+v %v", o, err)
	}
	for _, bad := range [][]string{{"extra"}, {"--since", "yesterday"}, {"--max", "-1"}} {
		if _, err := digestArgs(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

func TestLoadConfigDurations(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, `{"end_grace": "2m", "stall_after": "15m", "idle_close_after": "off"}`))
	if err != nil {
		t.Fatal(err)
	}
	if time.Duration(cfg.EndGrace) != 2*time.Minute || time.Duration(cfg.StallAfter) != 15*time.Minute || cfg.IdleCloseAfter >= 0 {
		t.Fatalf("config: %+v", cfg)
	}
	cfg, err = loadConfig(writeConfig(t, `{}`))
	if err != nil || cfg.EndGrace != 0 || cfg.StallAfter != 0 || cfg.IdleCloseAfter != 0 {
		t.Fatalf("defaults: %+v %v", cfg, err)
	}
	for _, bad := range []string{`{"stall_after": 20}`, `{"stall_after": "soon"}`, `{"stall_after": "0s"}`, `{"end_grace": "off"}`} {
		if _, err := loadConfig(writeConfig(t, bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestLoadConfigReposWithoutGitHub(t *testing.T) {
	l := writeConfig(t, `{"repos": {"local": {"path": "/src/local", "base": "main"}}, "worktree_root": "wt", "gc_after": "48h", "gc_auto": true}`)
	cfg, err := loadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repos["local"].Path != "/src/local" || cfg.WorktreeRoot != filepath.Join(l.Home, "wt") || cfg.gcAfter.Hours() != 48 || !cfg.GCAuto {
		t.Fatalf("cfg: %+v", cfg)
	}
}

func TestLoadConfigGitHubNeedsAGitHubRepo(t *testing.T) {
	l := writeConfig(t, `{"repos": {"local": {"path": "/src/local", "base": "main"}}, "github": {"app_id": 1, "private_key_file": "k.pem"}}`)
	if _, err := loadConfig(l); err == nil || !strings.Contains(err.Error(), "github owner/name") {
		t.Fatalf("err = %v", err)
	}
	l = writeConfig(t, `{"repos": {"local": {"path": "/src/local", "base": "main"}, "gh": {"path": "/src/gh", "github": "acme/gh", "base": "main"}}, "github": {"app_id": 1, "private_key_file": "k.pem"}}`)
	cfg, err := loadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, cfg.GitHub.PrivateKeyFile, 0o600)
	app, err := cfg.githubApp()
	if err != nil || !slices.Equal(app.Repos, []string{"acme/gh"}) {
		t.Fatalf("app repos: %+v %v", app, err)
	}
}

func TestLoadConfigRefusesBadGCAfter(t *testing.T) {
	for _, v := range []string{"soon", "-1h", "0s"} {
		l := writeConfig(t, `{"gc_after": "`+v+`"}`)
		if _, err := loadConfig(l); err == nil {
			t.Errorf("gc_after %q accepted", v)
		}
	}
}

func TestDebugFoldReadsACopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aeolian.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAgent(store.Agent{ID: "a1", State: store.StateBusy}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	before, _ := os.ReadFile(path)
	var out strings.Builder
	if err := debugFold(path, &out); err != nil || !strings.Contains(out.String(), "folds into the tables") {
		t.Fatalf("%q %v", out.String(), err)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatal("debug fold changed the database it was given")
	}
	if err := debugFold(filepath.Join(t.TempDir(), "missing.db"), &out); err == nil {
		t.Fatal("folded a missing database")
	}
}

func TestGCArgs(t *testing.T) {
	if dry, err := gcArgs(nil); err != nil || dry {
		t.Fatalf("plain: %v %v", dry, err)
	}
	if dry, err := gcArgs([]string{"--dry-run"}); err != nil || !dry {
		t.Fatalf("dry: %v %v", dry, err)
	}
	if _, err := gcArgs([]string{"x"}); err == nil {
		t.Fatal("extra arg accepted")
	}
}
func TestArenaArgs(t *testing.T) {
	dir := t.TempDir()
	personas := filepath.Join(dir, "p.yaml")
	priors := filepath.Join(dir, "pr.yaml")
	os.WriteFile(personas, []byte("personas:\n  - id: ceo\n    briefing: you fund things\nrubric: [shorter wins]\n"), 0o644)
	os.WriteFile(priors, []byte("- name: a\n  text: ta\n- name: b\n  text: tb\n"), 0o644)
	req, err := arenaArgs([]string{"--judges", "2", "--context", "rel", "--context", "/abs", "--final", "human",
		"--judges-as", personas, "--priors", priors, "--timeout", "5m", "what now?"})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if req.Question != "what now?" || req.Judges != 2 || req.Final != "human" || req.Timeout != "5m" ||
		!slices.Equal(req.Context, []string{filepath.Join(wd, "rel"), "/abs"}) ||
		len(req.Personas) != 1 || req.Personas[0].ID != "ceo" || len(req.Rubric) != 1 || len(req.Priors) != 2 {
		t.Fatalf("%+v", req)
	}
	for _, bad := range [][]string{
		{},
		{"q", "extra"},
		{"--judges-as", filepath.Join(dir, "missing.yaml"), "q"},
		{"--priors", personas, "q"},
		{"--entrants", "x", "q"},
		{"show"},
		{" Show "},
		{"--judges", "2", "list"},
	} {
		if _, err := arenaArgs(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPrecedentArgs(t *testing.T) {
	o, err := precedentArgs("add", []string{"--scope", "dev-ledger, branch", "--review-by", "2027-02-01", "dev-ledger is a project branch"})
	if err != nil || !slices.Equal(o.req.Scope, []string{"dev-ledger", "branch"}) || o.req.ReviewBy != "2027-02-01" || o.req.Text != "dev-ledger is a project branch" {
		t.Fatalf("add %+v %v", o, err)
	}
	o, err = precedentArgs("from-claim", []string{"run-lead-3", "it is a project branch"})
	if err != nil || o.req.ClaimID != "run-lead-3" || o.req.Text != "it is a project branch" || o.req.Scope != nil {
		t.Fatalf("from-claim %+v %v", o, err)
	}
	o, err = precedentArgs("add", []string{"--global", "never push to main"})
	if err != nil || !o.req.Global || o.req.Scope != nil {
		t.Fatalf("add --global %+v %v", o, err)
	}
	o, err = precedentArgs("add", []string{"--role", "lead, qa-shard", "--repo", "aeolian", "ask before merging"})
	if err != nil || !slices.Equal(o.req.Roles, []string{"lead", "qa-shard"}) || !slices.Equal(o.req.Repos, []string{"aeolian"}) {
		t.Fatalf("add --role %+v %v", o, err)
	}
	if _, err := precedentArgs("from-claim", []string{"--global", "c", "t"}); err == nil {
		t.Fatal("from-claim took --global")
	}
	if o, err := precedentArgs("list", []string{"--stale"}); err != nil || !o.stale {
		t.Fatalf("list %+v %v", o, err)
	}
	for sub, bad := range map[string][]string{
		"add":        {"no scope given"},
		"from-claim": {"only-an-id"},
		"list":       {"extra"},
		"rm":         {},
	} {
		if _, err := precedentArgs(sub, bad); err == nil {
			t.Errorf("%s %q: no error", sub, bad)
		}
	}
}

func TestLoadConfigPrecedentsDir(t *testing.T) {
	l := writeConfig(t, `{"precedents_dir": "shared/precedents"}`)
	cfg, err := loadConfig(l)
	if err != nil || cfg.PrecedentsDir != filepath.Join(l.Home, "shared/precedents") {
		t.Fatalf("%q %v", cfg.PrecedentsDir, err)
	}
	l = writeConfig(t, `{"precedents_dir": "/vault/aeolian/precedents"}`)
	if cfg, _ := loadConfig(l); cfg.PrecedentsDir != "/vault/aeolian/precedents" {
		t.Fatalf("%q", cfg.PrecedentsDir)
	}
}

func TestLoadConfigAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := filepath.Join(home, "claude-work")
	abs := t.TempDir()
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(writeConfig(t, `{"accounts": {"work": "~/claude-work/", "home": "`+abs+`", "host": ""}, "default_account": "work"}`))
	if err != nil {
		t.Fatal(err)
	}
	if dir, ok := cfg.Accounts["host"]; !ok || dir != "" {
		t.Fatalf("empty account dir: %q %v", dir, ok)
	}
	if cfg.Accounts["work"] != work || cfg.Accounts["home"] != abs || cfg.DefaultAccount != "work" {
		t.Fatalf("accounts %+v default %q", cfg.Accounts, cfg.DefaultAccount)
	}
	if cfg, err := loadConfig(writeConfig(t, `{}`)); err != nil || len(cfg.Accounts) != 0 || cfg.DefaultAccount != "" {
		t.Fatalf("no accounts: %+v %v", cfg, err)
	}
	for src, want := range map[string]string{
		`{"accounts": {"work": "claude-work"}}`:                           "absolute path",
		`{"accounts": {"work": "~/missing"}}`:                             "log in once",
		`{"accounts": {"work": "~/claude-work"}, "default_account": "x"}`: `default_account "x"`,
		`{"default_account": "x"}`:                                        `default_account "x"`,
		`{"accounts": {"": "~/claude-work"}}`:                             "empty name",
	} {
		if _, err := loadConfig(writeConfig(t, src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", src, err, want)
		}
	}
}

func TestLoadConfigGitHubUserToken(t *testing.T) {
	l := writeConfig(t, `{"repos": {"local": {"path": "/src/local", "base": "main"}}, "github": {"token_command": ["gh", "auth", "token"]}}`)
	cfg, err := loadConfig(l)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.GitHub.TokenCommand, []string{"gh", "auth", "token"}) || cfg.GitHub.PrivateKeyFile != "" {
		t.Fatalf("github: %+v", cfg.GitHub)
	}
	gh, err := cfg.githubAuth()
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := gh.(*github.User); !ok || !slices.Equal(u.TokenCommand, cfg.GitHub.TokenCommand) {
		t.Fatalf("auth: %#v", gh)
	}
}

func TestLoadConfigGitHubForms(t *testing.T) {
	for name, cfg := range map[string]string{
		"both":          `{"github": {"token_command": ["gh", "auth", "token"], "app_id": 1, "private_key_file": "k.pem"}}`,
		"both, no key":  `{"github": {"token_command": ["gh", "auth", "token"], "app_id": 1}}`,
		"neither":       `{"github": {}}`,
		"empty command": `{"github": {"token_command": []}}`,
		"blank program": `{"github": {"token_command": [""]}}`,
	} {
		if _, err := loadConfig(writeConfig(t, cfg)); err == nil || !strings.Contains(err.Error(), "github") {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func TestGitHubAuthApp(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, exampleConfig))
	if err != nil {
		t.Fatal(err)
	}
	writeKey(t, cfg.GitHub.PrivateKeyFile, 0o600)
	gh, err := cfg.githubAuth()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gh.(*github.App); !ok {
		t.Fatalf("auth: %#v", gh)
	}
	if gh, err := (Config{}).githubAuth(); gh != nil || err != nil {
		t.Fatalf("no github: %#v %v", gh, err)
	}
}

func TestResolveBotUser(t *testing.T) {
	dir := t.TempDir()
	git := filepath.Join(dir, "git")
	write := func(body string) {
		if err := os.WriteFile(git, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(`echo "me"`)
	if err := resolveBot(&github.User{Git: git}); err != nil {
		t.Fatal(err)
	}
	write(`exit 1`)
	if err := resolveBot(&github.User{Git: git}); err == nil || !strings.Contains(err.Error(), "user.email") {
		t.Fatalf("unset identity: %v", err)
	}
}

func TestDigestAllAndDismissReachTheHost(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.Query().Get("all")+" "+strings.TrimSpace(string(b)))
		w.Write(b)
		if r.Method == "GET" {
			w.Write([]byte(`{"groups":[],"clean":[]}`))
		}
	}))
	defer srv.Close()
	t.Setenv("AEOLIAN_ADDR", srv.Listener.Addr().String())
	if err := run([]string{"digest", "dismiss", "dead:r1", "blocked:r2:work"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"digest", "--all"}); err != nil {
		t.Fatal(err)
	}
	want := []string{`POST /api/digest/dismiss? {"keys":["dead:r1","blocked:r2:work"]}`, "GET /api/digest?1 "}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q", got)
	}
}

func TestShutdownEndsWaitingRequests(t *testing.T) {
	waiting := make(chan struct{})
	gaveUp := make(chan struct{})
	srv := newServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(waiting)
		<-r.Context().Done()
		close(gaveUp)
	}))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	go http.Get("http://" + ln.Addr().String())
	<-waiting
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-gaveUp:
	default:
		t.Fatal("handler still waiting after shutdown")
	}
}

func TestLoadConfigDefaultGrants(t *testing.T) {
	for src, want := range map[string][]string{
		`{}`:                                     {"claudeai:claude_docs"},
		`{"default_grants": []}`:                 {},
		`{"default_grants": ["claudeai:slack"]}`: {"claudeai:slack"},
	} {
		cfg, err := loadConfig(writeConfig(t, src))
		if err != nil || !slices.Equal(cfg.DefaultGrants, want) {
			t.Errorf("%s: %q %v", src, cfg.DefaultGrants, err)
		}
	}
	for _, src := range []string{`{"default_grants": ["linear"]}`, `{"default_grants": ["claudeai:Bad Name"]}`} {
		if _, err := loadConfig(writeConfig(t, src)); err == nil {
			t.Errorf("%s loaded", src)
		}
	}
}
