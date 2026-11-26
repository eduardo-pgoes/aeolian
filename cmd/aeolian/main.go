package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/github"
	"github.com/eduardo-pgoes/aeolian/internal/host"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

// DefaultAddr is where the host listens unless $AEOLIAN_ADDR says
// otherwise. Agents keep the address they were launched with, so it has to
// stay the same across host restarts.
const DefaultAddr = "127.0.0.1:7431"

const usage = `usage:
  aeolian host
  aeolian run [--source S] [--ref R] [--account A] [--context-cap N [--cap-kills K]] [--token-cap N] [--dir D] <workflow.yaml> "<goal>"
  aeolian kill <run-id>
  aeolian end [--force] <run-id>
  aeolian inbox [--source S]
  aeolian ack <run-id>
  aeolian gc [--dry-run]
  aeolian accounts
  aeolian arena [--entrants N] [--judges N] [--judges-as personas.yaml] [--priors priors.yaml]
                [--final evolver|human] [--model-propose M] [--model-judge M] [--model-evolve M]
                [--timeout 25m] [--no-evolve] [--account A] [--context path]... "<question>"
  aeolian arena show <arena-id>
  aeolian arenas
  aeolian claims <run-id>
  aeolian witness-replay [--precedents DIR|none] [--match] [--model M] [--boot full|lean] [--parallel N]
                         [--timeout D] [--bar N] [--require w02,w04,w09] [--json] <cases-dir>
  aeolian digest [--since 12h|2d|YYYY-MM-DD|RFC3339] [--md] [--run <run-id>] [--max 7] [--all]
  aeolian digest dismiss <key>...
  aeolian precedent add [--scope a,b] [--role r1,r2] [--repo name] [--global] [--kind correction|convention|safety] [--review-by YYYY-MM-DD] [--source S] "<correction>"
  aeolian precedent list [--stale]
  aeolian precedent from-claim [--scope a,b] [--role r1,r2] [--repo name] [--kind K] [--review-by YYYY-MM-DD] <claim-id> "<what is true>"
  aeolian debug agent "<prompt>"
  aeolian debug agents
  aeolian debug fold <aeolian.db>   (checks a copy; never opens the file itself)
  aeolian keeper start "<prompt>"
  aeolian keeper type <session-id> [text...]   (reads stdin without text)
  aeolian keeper list
  aeolian keeper kill <session-id>
  aeolian keeper resume <session-id>
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aeolian:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 1 && args[0] == "host":
		return serve()
	case len(args) >= 3 && args[0] == "run":
		if path, req, err := runArgs(args[1:]); err == nil {
			return startRun(path, req)
		}
	case len(args) == 2 && args[0] == "kill":
		return killRun(args[1])
	case len(args) >= 2 && args[0] == "end":
		if id, force, err := endArgs(args[1:]); err == nil {
			return endRun(id, force)
		}
	case len(args) >= 1 && args[0] == "inbox":
		if source, err := inboxArgs(args[1:]); err == nil {
			return inbox(source)
		}
	case len(args) == 2 && args[0] == "ack":
		return ackRun(args[1])
	case len(args) == 1 && args[0] == "accounts":
		return listAccounts()
	case len(args) >= 1 && args[0] == "gc":
		if dry, err := gcArgs(args[1:]); err == nil {
			return gc(dry)
		}
	case len(args) == 3 && args[0] == "arena" && args[1] == "show":
		return showArena(args[2])
	case len(args) >= 2 && args[0] == "arena":
		if req, err := arenaArgs(args[1:]); err == nil {
			return startArena(req)
		} else {
			fmt.Fprintln(os.Stderr, "aeolian arena:", err)
		}
	case len(args) == 1 && args[0] == "arenas":
		return listArenas()
	case len(args) == 2 && args[0] == "claims":
		return claims(args[1])
	case len(args) >= 2 && args[0] == "witness-replay":
		if r, err := parseReplayArgs(args[1:]); err == nil {
			return witnessReplay(r)
		}
	case len(args) >= 3 && args[0] == "digest" && args[1] == "dismiss":
		return digestDismiss(args[2:])
	case len(args) >= 1 && args[0] == "digest":
		o, err := digestArgs(args[1:])
		if err == nil {
			return digestCmd(o)
		}
		fmt.Fprintln(os.Stderr, "aeolian:", err)
	case len(args) >= 2 && args[0] == "precedent":
		return precedentCmd(args[1], args[2:])
	case len(args) == 3 && args[0] == "debug" && args[1] == "agent":
		return debugAgent(args[2])
	case len(args) == 2 && args[0] == "debug" && args[1] == "agents":
		return debugAgents()
	case len(args) == 3 && args[0] == "debug" && args[1] == "fold":
		return debugFold(args[2], os.Stdout)
	case len(args) >= 2 && args[0] == "keeper":
		return keeperCmd(args[1], args[2:])
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
	return nil
}

func keeperCmd(cmd string, rest []string) error {
	switch {
	case cmd == "start" && len(rest) == 1:
		return start(rest[0])
	case cmd == "type" && len(rest) >= 1:
		return typeText(rest[0], rest[1:])
	case cmd == "list" && len(rest) == 0:
		return list()
	case cmd == "kill" && len(rest) == 1:
		return keeper.Kill(agent.SessionName(rest[0]))
	case cmd == "resume" && len(rest) == 1:
		return resume(rest[0])
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
	return nil
}

func layout() (agent.Layout, error) {
	home, err := agent.Home()
	if err != nil {
		return agent.Layout{}, err
	}
	cj, err := agent.ClaudeJSON()
	if err != nil {
		return agent.Layout{}, err
	}
	us, err := agent.UserSettingsPath()
	return agent.Layout{Home: home, ClaudeJSON: cj, UserSettings: us}, err
}

func start(prompt string) error {
	l, err := layout()
	if err != nil {
		return err
	}
	id := agent.NewSessionID()
	if err := l.Prepare(id, l.ScratchDir(id), agent.Setup{}); err != nil {
		return err
	}
	if err := l.WritePrompt(id, prompt); err != nil {
		return err
	}
	la := agent.Launch{AgentID: id, SessionID: id, Prompt: prompt}
	if err := keeper.Start(agent.SessionName(id), l.ScratchDir(id), l.Argv(la)); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func resume(id string) error {
	l, err := layout()
	if err != nil {
		return err
	}
	if live, err := keeper.Has(agent.SessionName(id)); err != nil {
		return err
	} else if live {
		return fmt.Errorf("session %s is still running", id)
	}
	if _, err := os.Stat(l.ScratchDir(id)); err != nil {
		return fmt.Errorf("no scratch folder for session %s: %w", id, err)
	}
	if err := l.Prepare(id, l.ScratchDir(id), agent.Setup{}); err != nil {
		return err
	}
	la := agent.Launch{AgentID: id, SessionID: id, Resume: true}
	if err := keeper.Start(agent.SessionName(id), l.ScratchDir(id), l.Argv(la)); err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func typeText(id string, words []string) error {
	text := strings.Join(words, " ")
	if len(words) == 0 {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return errors.New("nothing to type")
	}
	return keeper.Type(agent.SessionName(id), text)
}

func list() error {
	sessions, err := keeper.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION\tSTARTED\tFOLDER")
	for _, s := range sessions {
		started := s.Created
		var unix int64
		if _, err := fmt.Sscan(s.Created, &unix); err == nil {
			started = time.Unix(unix, 0).Format(time.DateTime)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", strings.TrimPrefix(s.Name, keeper.SessionPrefix), started, s.Dir)
	}
	return w.Flush()
}

// addr is the host's listen address, which must be a loopback IP.
func addr() (string, error) {
	a := os.Getenv("AEOLIAN_ADDR")
	if a == "" {
		a = DefaultAddr
	}
	h, _, err := net.SplitHostPort(a)
	if err != nil {
		return "", fmt.Errorf("AEOLIAN_ADDR: %w", err)
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("AEOLIAN_ADDR %q is not a loopback IP address", a)
	}
	return a, nil
}

// Config is $AEOLIAN_HOME/config.json; a missing file or field keeps the
// default.
type Config struct {
	MaxAgents       int                  `json:"max_agents"`
	MaxRuns         int                  `json:"max_runs"`
	MaxRunsBySource map[string]int       `json:"max_runs_by_source"`
	Repos           map[string]host.Repo `json:"repos"`
	GitHub          *GitHubConfig        `json:"github"`
	Linear          json.RawMessage      `json:"linear"`
	OnFinish        []string             `json:"on_finish"`
	// EndGrace, StallAfter and IdleCloseAfter are durations such as "90s"
	// or "20m"; "off" turns the stall and idle watchdogs off.
	EndGrace       Duration `json:"end_grace"`
	StallAfter     Duration `json:"stall_after"`
	IdleCloseAfter Duration `json:"idle_close_after"`
	// EventsRetentionDays is how many days a finished run keeps its hook
	// input in the event log; 0 keeps it forever.
	EventsRetentionDays int `json:"events_retention_days"`
	// WorktreeRoot is where the host makes worktrees; relative to
	// $AEOLIAN_HOME unless absolute. Empty means $AEOLIAN_HOME/worktrees.
	WorktreeRoot string `json:"worktree_root"`
	// GCAfter is how long after its ack a run's folders are kept, as a Go
	// duration. Empty means host.DefaultGCAfter.
	GCAfter string `json:"gc_after"`
	// GCAuto collects garbage at start and hourly. Off by default: run
	// aeolian gc --dry-run, then aeolian gc, by hand first.
	GCAuto  bool               `json:"gc_auto"`
	Witness host.WitnessConfig `json:"witness"`
	Boot    host.BootConfig    `json:"boot"`
	// PrecedentsDir is the folder of precedent files, absolute or relative
	// to the home; empty means <home>/precedents. Point it at a synced
	// folder (a vault subfolder) to share precedents across machines.
	PrecedentsDir  string            `json:"precedents_dir"`
	Accounts       map[string]string `json:"accounts"`
	DefaultAccount string            `json:"default_account"`
	DefaultGrants  []string          `json:"default_grants"`

	gcAfter time.Duration
}

// Duration is a time.Duration written in config.json as a string: "20m",
// or "off", which is negative.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("want a duration such as \"20m\": %w", err)
	}
	if s == "off" {
		*d = -1
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v <= 0 {
		return fmt.Errorf("duration %q is not positive; use \"off\" to turn it off", s)
	}
	*d = Duration(v)
	return nil
}

var errLinearMoved = errors.New("config.json: the host no longer reads the linear section; move it to linear-adapter.json")

type GitHubConfig struct {
	AppID          int64    `json:"app_id"`
	PrivateKeyFile string   `json:"private_key_file"`
	TokenCommand   []string `json:"token_command"`
}

func loadConfig(l agent.Layout) (Config, error) {
	cfg := Config{MaxAgents: host.DefaultMaxAgents, MaxRuns: host.DefaultMaxRuns, EventsRetentionDays: int(host.DefaultEventsRetention / (24 * time.Hour))}
	path := filepath.Join(l.Home, "config.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.EndGrace < 0 {
		return cfg, fmt.Errorf("%s: end_grace cannot be off", path)
	}
	if cfg.Linear != nil {
		return cfg, errLinearMoved
	}
	if cfg.MaxAgents < 1 {
		return cfg, fmt.Errorf("%s: max_agents must be at least 1", path)
	}
	if cfg.MaxRuns < 1 {
		return cfg, fmt.Errorf("%s: max_runs must be at least 1", path)
	}
	for src, n := range cfg.MaxRunsBySource {
		if src == "" || n < 1 {
			return cfg, fmt.Errorf("%s: max_runs_by_source needs a source name and a cap of at least 1, got %q: %d", path, src, n)
		}
	}
	if cfg.EventsRetentionDays < 0 {
		return cfg, fmt.Errorf("%s: events_retention_days must not be negative", path)
	}
	if err := cfg.Witness.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.Boot.Validate(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	if len(cfg.OnFinish) > 0 && !filepath.IsAbs(cfg.OnFinish[0]) {
		return cfg, fmt.Errorf("%s: on_finish must start with an absolute path", path)
	}
	if cfg.DefaultGrants == nil {
		cfg.DefaultGrants = host.DefaultGrants
	}
	for _, g := range cfg.DefaultGrants {
		if _, ok := workflow.ClaudeAIConnector(g); !ok {
			return cfg, fmt.Errorf("%s: default_grants takes only %s<connector> grants, got %q", path, workflow.GrantClaudeAIPrefix, g)
		}
	}
	if err := workflow.CheckGrants("default_grants", cfg.DefaultGrants); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	cfg.PrecedentsDir = resolvePrecedentsDir(l.Home, cfg.PrecedentsDir)
	if err := cfg.resolveAccounts(); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	withGitHub := 0
	for key, r := range cfg.Repos {
		owner, name, ok := strings.Cut(r.GitHub, "/")
		switch {
		case !filepath.IsAbs(r.Path):
			return cfg, fmt.Errorf("%s: repos.%s.path must be an absolute path", path, key)
		case r.GitHub != "" && (!ok || owner == "" || name == "" || strings.Contains(name, "/")):
			return cfg, fmt.Errorf("%s: repos.%s.github must be owner/name", path, key)
		case r.Base == "":
			return cfg, fmt.Errorf("%s: repos.%s.base is missing", path, key)
		}
		if r.GitHub != "" {
			withGitHub++
		}
	}
	if cfg.WorktreeRoot != "" && !filepath.IsAbs(cfg.WorktreeRoot) {
		cfg.WorktreeRoot = filepath.Join(l.Home, cfg.WorktreeRoot)
	}
	if cfg.GCAfter != "" {
		d, err := time.ParseDuration(cfg.GCAfter)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("%s: gc_after %q is not a positive duration like 168h", path, cfg.GCAfter)
		}
		cfg.gcAfter = d
	}
	if g := cfg.GitHub; g != nil {
		app := g.AppID != 0 || g.PrivateKeyFile != ""
		switch {
		case app && g.TokenCommand != nil:
			return cfg, fmt.Errorf("%s: github takes token_command or app_id and private_key_file, not both", path)
		case g.TokenCommand != nil:
			if len(g.TokenCommand) == 0 || g.TokenCommand[0] == "" {
				return cfg, fmt.Errorf("%s: github.token_command must name a program, such as [\"gh\", \"auth\", \"token\"]", path)
			}
		case !app:
			return cfg, fmt.Errorf("%s: github needs token_command, or app_id and private_key_file", path)
		case g.AppID < 1:
			return cfg, fmt.Errorf("%s: github.app_id is missing", path)
		case g.PrivateKeyFile == "":
			return cfg, fmt.Errorf("%s: github.private_key_file is missing", path)
		case withGitHub == 0:
			return cfg, fmt.Errorf("%s: github needs at least one entry under repos with a github owner/name to scope its tokens to", path)
		}
		if app && !filepath.IsAbs(g.PrivateKeyFile) {
			g.PrivateKeyFile = filepath.Join(l.Home, g.PrivateKeyFile)
		}
	}
	return cfg, nil
}

func resolveBot(gh host.GitHubAuth) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bot, err := gh.Identity(ctx)
	switch {
	case github.Permanent(err):
		return fmt.Errorf("github identity: %w", err)
	case err != nil:
		log.Printf("github identity: %v; headless launches fail until it resolves, retrying at each", err)
	default:
		log.Printf("github: headless agents commit as %s <%s>", bot.Name, bot.Email)
	}
	return nil
}

func checkUserToken(u *github.User) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := u.Token(ctx, github.ReadPermissions); err != nil {
		log.Printf("%v; github grants fail until it works", err)
		return
	}
	log.Printf("github: agents granted github or github-read get your own unscoped token from %s", strings.Join(u.TokenCommand, " "))
}

func (cfg Config) githubAuth() (host.GitHubAuth, error) {
	if cfg.GitHub == nil {
		return nil, nil
	}
	if cfg.GitHub.TokenCommand != nil {
		return &github.User{TokenCommand: cfg.GitHub.TokenCommand}, nil
	}
	app, err := cfg.githubApp()
	if err != nil {
		return nil, err
	}
	return app, nil
}

func (cfg Config) githubApp() (*github.App, error) {
	if cfg.GitHub == nil {
		return nil, nil
	}
	key, err := github.LoadKey(cfg.GitHub.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("github app key: %w", err)
	}
	var repos []string
	for _, r := range cfg.Repos {
		if r.GitHub != "" && !slices.Contains(repos, r.GitHub) {
			repos = append(repos, r.GitHub)
		}
	}
	slices.Sort(repos)
	return &github.App{ID: cfg.GitHub.AppID, Key: key, Repos: repos}, nil
}

// agentBins are the programs an agent's session runs, looked up on the
// host's PATH: tmux holds the session, systemd-run gives it a scope, claude
// is the agent and curl reports its hooks.
var agentBins = []string{"tmux", "systemd-run", "claude", "curl"}

// checkPath fails when a program in agentBins is missing from PATH. Without
// it every agent's session would end at launch and the sweep would record
// the agent killed, which hides the cause.
func checkPath() error {
	var missing []string
	for _, bin := range agentBins {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s not on PATH %q; add its directory to PATH (for a systemd unit, set Environment=PATH=...)", strings.Join(missing, ", "), os.Getenv("PATH"))
	}
	return nil
}

// serve runs the host until SIGINT or SIGTERM, sweeping for lost agents
// meanwhile. Stopping it closes the HTTP server and the database and leaves
// every agent session running.
func serve() error {
	if err := checkPath(); err != nil {
		return err
	}
	l, err := layout()
	if err != nil {
		return err
	}
	a, err := addr()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.Home, 0o755); err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(l.Home, "aeolian.db"))
	if err != nil {
		return err
	}
	defer st.Close()
	ln, err := net.Listen("tcp", a)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(l)
	if err != nil {
		return err
	}
	h := &host.Host{
		Store: st, Layout: l, Keeper: host.Tmux{}, URL: "http://" + a, MaxAgents: cfg.MaxAgents, MaxRuns: cfg.MaxRuns, MaxRunsBySource: cfg.MaxRunsBySource, OnFinish: cfg.OnFinish,
		EndGrace: time.Duration(cfg.EndGrace), StallAfter: time.Duration(cfg.StallAfter), IdleCloseAfter: time.Duration(cfg.IdleCloseAfter),
		EventsRetention: time.Duration(cfg.EventsRetentionDays) * 24 * time.Hour,
		Transcripts:     transcripts(),
		Repos:           cfg.Repos, WorktreeRoot: cfg.WorktreeRoot, GCAfter: cfg.gcAfter, AutoGC: cfg.GCAuto,
		Precedents: cfg.PrecedentsDir, DefaultGrants: cfg.DefaultGrants,
		Accounts: cfg.Accounts, DefaultAccount: cfg.DefaultAccount,
	}
	h.DoneGates = append(h.DoneGates, cfg.Witness.Gate())
	gh, err := cfg.githubAuth()
	if err != nil {
		return err
	}
	if gh != nil {
		if err := resolveBot(gh); err != nil {
			return err
		}
		if u, ok := gh.(*github.User); ok {
			checkUserToken(u)
		}
		h.GitHub = gh
	}
	// No WriteTimeout: /api/events/stream holds its response open for as
	// long as a client listens.
	srv := newServer(h.Handler(), h.StopStreams)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	h.CloseOrphanArenas()
	h.AdmitRuns()
	watching := make(chan struct{})
	go func() { h.Watch(ctx); close(watching) }()
	defer func() { stop(); <-watching }()
	log.Printf("aeolian host on http://%s, database %s, max_agents %d, max_runs %d", a, filepath.Join(l.Home, "aeolian.db"), h.MaxAgents, h.MaxRuns)
	if len(h.OnFinish) > 0 {
		log.Printf("on_finish %q", h.OnFinish)
	}
	log.Printf("precedents in %s", h.PrecedentsDir())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}

func api(method, path string, body, out any) error {
	a, err := addr()
	if err != nil {
		return err
	}
	var in io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		in = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+a+path, in)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is the host running? %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// runArgs parses run's flags, which come before the workflow path and goal.
func runArgs(args []string) (string, host.RunRequest, error) {
	var req host.RunRequest
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&req.Source, "source", "", "")
	fs.StringVar(&req.Ref, "ref", "", "")
	fs.StringVar(&req.Account, "account", "", "")
	fs.Int64Var(&req.ContextCap, "context-cap", 0, "")
	fs.IntVar(&req.CapKills, "cap-kills", 0, "")
	fs.Int64Var(&req.TokenCap, "token-cap", 0, "")
	fs.StringVar(&req.Dir, "dir", "", "")
	fs.Func("env", "", func(kv string) error {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--env %q is not K=V", kv)
		}
		if req.Env == nil {
			req.Env = map[string]string{}
		}
		req.Env[k] = v
		return nil
	})
	fs.Func("git-config", "", func(kv string) error {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("--git-config %q is not key=value", kv)
		}
		req.GitConfig = append(req.GitConfig, [2]string{k, v})
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return "", req, err
	}
	if req.Dir != "" {
		d, err := workflow.ExpandDir(req.Dir)
		if err != nil {
			return "", req, err
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return "", req, err
		}
		req.Dir = abs
	}
	if err := agent.CheckEnv(req.Env); err != nil {
		return "", req, err
	}
	if err := agent.CheckGitConfig(req.GitConfig); err != nil {
		return "", req, err
	}
	if fs.NArg() != 2 {
		return "", req, errors.New("want a workflow and a goal")
	}
	req.Goal = fs.Arg(1)
	return fs.Arg(0), req, nil
}

// startRun asks the host to start a run of the workflow at path and prints
// its id without waiting for it, noting on stderr when the run is queued.
func startRun(path string, req host.RunRequest) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	req.Workflow = string(src)
	var run store.Run
	if err := api("POST", "/api/runs", req, &run); err != nil {
		return err
	}
	fmt.Println(run.ID)
	if run.State == store.StateQueued {
		fmt.Fprintf(os.Stderr, "run %s is %s behind max_runs\n", run.ID, run.State)
	}
	return nil
}

// transcripts is where Claude Code keeps its session transcripts.
func transcripts() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(u, ".claude", "projects")
}

func listAccounts() error {
	var v host.AccountsView
	if err := api("GET", "/api/accounts", nil, &v); err != nil {
		return err
	}
	if len(v.Accounts) == 0 {
		fmt.Println("no accounts in config.json; runs use the host's own Claude Code config")
		return nil
	}
	for _, name := range v.Accounts {
		if name == v.Default {
			fmt.Println(name, "(default)")
			continue
		}
		fmt.Println(name)
	}
	return nil
}

func (c *Config) resolveAccounts() error {
	for name, dir := range c.Accounts {
		if name == "" {
			return errors.New("accounts has an empty name")
		}
		if dir == "" {
			continue
		}
		if dir == "~" || strings.HasPrefix(dir, "~/") {
			u, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("accounts.%s: %w", name, err)
			}
			dir = filepath.Join(u, dir[1:])
		}
		if !filepath.IsAbs(dir) {
			return fmt.Errorf("accounts.%s must be an absolute path or start with ~/", name)
		}
		fi, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("accounts.%s: %w; log in once with CLAUDE_CONFIG_DIR=%s claude", name, err, dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("accounts.%s: %s is not a folder", name, dir)
		}
		c.Accounts[name] = filepath.Clean(dir)
	}
	if c.DefaultAccount != "" {
		if _, ok := c.Accounts[c.DefaultAccount]; !ok {
			return fmt.Errorf("default_account %q is not under accounts", c.DefaultAccount)
		}
	}
	return nil
}

func endArgs(args []string) (string, bool, error) {
	var force bool
	fs := flag.NewFlagSet("end", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&force, "force", false, "")
	if err := fs.Parse(args); err != nil {
		return "", false, err
	}
	if fs.NArg() != 1 {
		return "", false, errors.New("want a run id")
	}
	return fs.Arg(0), force, nil
}

// endRun asks the host to end a run as done. Without force its lead is
// first asked to hand back, so the run may still be busy when this
// returns; it prints the run's state at that moment.
func endRun(id string, force bool) error {
	path := "/api/runs/" + url.PathEscape(id) + "/end"
	if force {
		path += "?force=1"
	}
	var run store.Run
	if err := api("POST", path, nil, &run); err != nil {
		return err
	}
	if run.State == store.StateBusy {
		fmt.Println(run.ID, "ending")
		return nil
	}
	fmt.Println(run.ID, run.State, run.Reason)
	return nil
}

// killRun asks the host to kill a run and every agent of it, and prints
// the run's state.
func killRun(id string) error {
	var run store.Run
	if err := api("POST", "/api/runs/"+url.PathEscape(id)+"/kill", nil, &run); err != nil {
		return err
	}
	fmt.Println(run.ID, run.State)
	return nil
}

func inboxArgs(args []string) (string, error) {
	var source string
	fs := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&source, "source", "", "")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() != 0 {
		return "", errors.New("inbox takes no arguments")
	}
	return source, nil
}

// inbox prints every finished run not yet acked, oldest first, one JSON
// object per line.
func inbox(source string) error {
	path := "/api/inbox"
	if source != "" {
		path += "?" + url.Values{"source": {source}}.Encode()
	}
	var entries []host.InboxEntry
	if err := api("GET", path, nil, &entries); err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// ackRun takes a finished run out of the inbox and prints when it was
// acked.
func ackRun(id string) error {
	var run store.Run
	if err := api("POST", "/api/runs/"+url.PathEscape(id)+"/ack", nil, &run); err != nil {
		return err
	}
	if run.AckedAt == nil {
		return fmt.Errorf("run %s came back without an ack", run.ID)
	}
	fmt.Println(run.ID, run.AckedAt.Format(time.RFC3339))
	return nil
}

func gcArgs(args []string) (bool, error) {
	var dry bool
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&dry, "dry-run", false, "")
	if err := fs.Parse(args); err != nil {
		return false, err
	}
	if fs.NArg() != 0 {
		return false, errors.New("gc takes no arguments")
	}
	return dry, nil
}

// gc asks the host to collect the folders and worktrees of runs acked
// longer than gc_after ago, and prints what went, or with dry would go.
func gc(dry bool) error {
	path := "/api/gc"
	if dry {
		path += "?dry_run=1"
	}
	var rep host.GCReport
	if err := api("POST", path, nil, &rep); err != nil {
		return err
	}
	verb := "removed"
	if rep.DryRun {
		verb = "would remove"
	}
	fmt.Printf("%s the folders of %d agents in %d runs (%d bytes)\n", verb, len(rep.Agents), len(rep.Runs), rep.Bytes)
	for _, p := range rep.Removed {
		fmt.Printf("%s worktree %s\n", verb, p)
	}
	for _, k := range rep.Kept {
		fmt.Printf("keeping worktree %s (branch %s): %s\n", k.Path, k.Branch, k.Reason)
	}
	for _, p := range rep.Logs {
		fmt.Printf("failed run log kept at %s\n", p)
	}
	if rep.Untrusted > 0 {
		fmt.Printf("removed %d trust entries from Claude Code's config\n", rep.Untrusted)
	}
	for _, e := range rep.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if len(rep.Errors) > 0 {
		return fmt.Errorf("%d errors", len(rep.Errors))
	}
	return nil
}

func debugAgent(prompt string) error {
	var a store.Agent
	if err := api("POST", "/api/agents", host.StartRequest{Prompt: prompt}, &a); err != nil {
		return err
	}
	fmt.Println(a.ID)
	return nil
}

func debugAgents() error {
	var agents []store.Agent
	if err := api("GET", "/api/agents?all=1", nil, &agents); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSTATE\tLAST HOOK\tWORKTREES\tHANDOFF")
	for _, a := range agents {
		var text string
		var worktrees []string
		if a.Handoff != nil {
			text = strings.ReplaceAll(a.Handoff.Text, "\n", " ")
			for _, wt := range a.Handoff.Worktrees {
				worktrees = append(worktrees, wt.Path+"@"+wt.Branch)
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID, a.State, a.LastHook, strings.Join(worktrees, ","), text)
	}
	return w.Flush()
}

// debugFold prints how the event log of the database at path fails to fold
// back into its tables, or that it folds. Opening a database migrates it,
// so it opens a copy, never the file itself: the live host may be writing
// to it.
func debugFold(path string, out io.Writer) error {
	dir, err := os.MkdirTemp("", "aeolian-fold-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "aeolian.db")
	// The -wal file holds commits not yet checkpointed into the main file.
	for _, suffix := range []string{"", "-wal"} {
		if err := copyFile(path+suffix, copyPath+suffix); err != nil && !(suffix != "" && errors.Is(err, os.ErrNotExist)) {
			return err
		}
	}
	st, err := store.Open(copyPath)
	if err != nil {
		return err
	}
	defer st.Close()
	diff, err := store.FoldDiff(st)
	if err != nil {
		return err
	}
	for _, d := range diff {
		fmt.Fprintln(out, d)
	}
	if len(diff) > 0 {
		return fmt.Errorf("%d differences between the event log and the tables", len(diff))
	}
	fmt.Fprintln(out, "the event log folds into the tables")
	return nil
}

func copyFile(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// pathList is a flag that may repeat; each value becomes an absolute path.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }

func (p *pathList) Set(v string) error {
	abs, err := filepath.Abs(v)
	if err != nil {
		return err
	}
	*p = append(*p, abs)
	return nil
}

// arenaArgs parses arena's flags, which come before the question, and reads
// the priors and personas files they name.
func arenaArgs(args []string) (host.ArenaRequest, error) {
	var req host.ArenaRequest
	var context pathList
	var priors, personas string
	fs := flag.NewFlagSet("arena", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.IntVar(&req.Entrants, "entrants", 0, "")
	fs.IntVar(&req.Judges, "judges", 0, "")
	fs.StringVar(&personas, "judges-as", "", "")
	fs.StringVar(&priors, "priors", "", "")
	fs.StringVar(&req.Final, "final", "", "")
	fs.StringVar(&req.ModelPropose, "model-propose", "", "")
	fs.StringVar(&req.ModelJudge, "model-judge", "", "")
	fs.StringVar(&req.ModelEvolve, "model-evolve", "", "")
	fs.StringVar(&req.Timeout, "timeout", "", "")
	fs.StringVar(&req.Account, "account", "", "")
	fs.BoolVar(&req.NoEvolve, "no-evolve", false, "")
	fs.Var(&context, "context", "")
	if err := fs.Parse(args); err != nil {
		return req, err
	}
	if fs.NArg() != 1 {
		return req, errors.New("want one question, quoted, after the flags")
	}
	req.Question, req.Context = fs.Arg(0), context
	// A lone subcommand word is a mistyped command, not a question worth
	// paying an arena for.
	switch w := strings.ToLower(strings.TrimSpace(req.Question)); w {
	case "show", "list", "ls", "help", "arenas", "kill":
		return req, fmt.Errorf("%q is a subcommand, not a question; to see an arena: aeolian arena show <id>, to list them: aeolian arenas", w)
	}
	if priors != "" {
		b, err := os.ReadFile(priors)
		if err != nil {
			return req, err
		}
		if req.Priors, err = host.ParsePriors(b); err != nil {
			return req, fmt.Errorf("%s: %w", priors, err)
		}
	}
	if personas != "" {
		b, err := os.ReadFile(personas)
		if err != nil {
			return req, err
		}
		if req.Personas, req.Rubric, err = host.ParsePersonas(b); err != nil {
			return req, fmt.Errorf("%s: %w", personas, err)
		}
	}
	return req, nil
}

// startArena asks the host to start an arena and prints its id and where
// its report will be, without waiting for it.
func startArena(req host.ArenaRequest) error {
	var a host.Arena
	if err := api("POST", "/api/arenas", req, &a); err != nil {
		return err
	}
	fmt.Println(a.ID)
	fmt.Fprintf(os.Stderr, "arena %s: %d entrants, %d judges, final %s\nreport when done: %s\nfollow: aeolian arena show %s\n",
		a.State, len(a.Entrants), len(a.Judges), a.Final, filepath.Join(a.Dir, "arena.md"), a.ID)
	return nil
}

func showArena(id string) error {
	var a host.Arena
	if err := api("GET", "/api/arenas/"+url.PathEscape(id), nil, &a); err != nil {
		return err
	}
	fmt.Printf("%s %s", a.ID, a.State)
	if a.Note != "" {
		fmt.Printf(" (%s)", a.Note)
	}
	fmt.Printf("\n%s\n\n", a.Question)
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ENTRANT\tLABEL\tSTATE\tWORDS\tNOTE")
	for _, e := range a.Entrants {
		// Word counts are hidden until judging ends, so they cannot tie an
		// anonymous entry to its lens.
		words := "-"
		if e.Words > 0 {
			words = fmt.Sprint(e.Words)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Prior, e.Label, e.State, words, e.Note)
	}
	for _, j := range a.Judges {
		fmt.Fprintf(w, "judge %d %s\t\t%s\t%d/%d pairs\t%s\n", j.Index+1, j.Persona, j.State, j.Judged, j.Pairs, j.Note)
	}
	w.Flush()
	if len(a.Standings) > 0 {
		fmt.Println()
		w = tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "RANK\tLABEL\tLENS\tSTRENGTH\tW-L")
		for _, s := range a.Standings {
			fmt.Fprintf(w, "%d\t%s\t%s\t%.3f\t%d-%d\n", s.Rank, s.Label, s.Prior, s.Score, s.Wins, s.Losses)
		}
		w.Flush()
		fmt.Printf("\n%d split pair(s)\n", len(a.Splits))
	}
	if a.Report != "" {
		fmt.Printf("\nreport: %s\n", a.Report)
	}
	return nil
}

func listArenas() error {
	var as []host.Arena
	if err := api("GET", "/api/arenas", nil, &as); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATE\tCREATED\tQUESTION")
	for _, a := range as {
		q := []rune(a.Question)
		if len(q) > 60 {
			q = append(q[:57], []rune("...")...)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, a.State, a.CreatedAt.Local().Format("01-02 15:04"), string(q))
	}
	return w.Flush()
}

// newServer serves handler with request contexts that end when Shutdown
// starts, so calls that wait on their request, such as spawn and await, give
// up instead of holding Shutdown until it times out. Each of onShutdown runs
// at Shutdown too.
func newServer(handler http.Handler, onShutdown ...func()) *http.Server {
	base, cancel := context.WithCancel(context.Background())
	srv := &http.Server{Handler: handler, BaseContext: func(net.Listener) context.Context { return base }}
	srv.RegisterOnShutdown(cancel)
	for _, f := range onShutdown {
		srv.RegisterOnShutdown(f)
	}
	return srv
}
