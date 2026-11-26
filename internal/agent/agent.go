// Package agent lays out an agent's folders under $AEOLIAN_HOME and builds
// the Claude Code command line that runs it.
package agent

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/keeper"
)

const (
	MemoryHigh = "800M"
	MemoryMax  = "1G"
	MemoryMin  = 512
)

var memorySize = regexp.MustCompile(`^([1-9][0-9]{0,6})([MG])$`)

func MemoryLimits(size string) (high, limit string, err error) {
	if size == "" {
		return MemoryHigh, MemoryMax, nil
	}
	m := memorySize.FindStringSubmatch(size)
	if m == nil {
		return "", "", fmt.Errorf("memory %q is not a size like 4G or 1536M", size)
	}
	mib, _ := strconv.Atoi(m[1])
	if m[2] == "G" {
		mib *= 1024
	}
	if mib < MemoryMin {
		return "", "", fmt.Errorf("memory %q is under %dM", size, MemoryMin)
	}
	return strconv.Itoa(mib*4/5) + "M", size, nil
}

// DeniedTools are the built-in tools a session is denied: Agent and its old
// name Task, so helpers can only be started through the host's spawn. Every
// other built-in tool stays on, so tools that newer Claude Code versions add
// (the claude.ai Artifact tools, for one) reach sessions without a change here.
var DeniedTools = []string{"Agent", "Task"}

// Home is $AEOLIAN_HOME, or ~/.local/share/aeolian when unset.
func Home() (string, error) {
	if h := os.Getenv("AEOLIAN_HOME"); h != "" {
		return filepath.Abs(h)
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".local", "share", "aeolian"), nil
}

type Layout struct {
	Home       string
	ClaudeJSON string
	// UserSettings is the user's Claude Code settings.json. A lean boot
	// skips it, so Prepare carries the few keys in CarriedUserSettings from
	// it into the agent's own settings. Empty carries none.
	UserSettings string
	ClaudeDir    string
}

func ClaudeJSONIn(dir string) string { return filepath.Join(dir, ".claude.json") }

func UserSettingsIn(dir string) string { return filepath.Join(dir, "settings.json") }

func TranscriptsIn(dir string) string { return filepath.Join(dir, "projects") }

func (l Layout) ForAccount(dir string) Layout {
	if dir == "" {
		return l
	}
	l.ClaudeDir = dir
	l.ClaudeJSON = ClaudeJSONIn(dir)
	l.UserSettings = UserSettingsIn(dir)
	return l
}

// UserSettingsPath is the user's Claude Code settings.json, under
// $CLAUDE_CONFIG_DIR when it is set.
func UserSettingsPath() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return UserSettingsIn(d), nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".claude", "settings.json"), nil
}

// Boot profiles decide how much of the user's own Claude Code setup an
// agent starts with. BootFull is all of it: the user's settings, CLAUDE.md,
// output style, skills, plugins and custom agents. BootLean starts from
// project and local settings only, with slash commands and skills off, the
// default output style, no auto memory, and the per-machine prompt sections
// moved out of the system prompt so agents share its cache. Auth, the
// host's MCP server and hooks are the same in both.
const (
	BootFull = "full"
	BootLean = "lean"
)

var BootProfiles = []string{BootFull, BootLean}

// CheckBoot refuses a boot profile that is neither empty nor in
// BootProfiles.
func CheckBoot(p string) error {
	if p == "" || slices.Contains(BootProfiles, p) {
		return nil
	}
	return fmt.Errorf("boot %q is not one of %s", p, strings.Join(BootProfiles, ", "))
}

// LeanArgs are the flags a lean boot adds to claude's command line.
var LeanArgs = []string{
	"--setting-sources", "project,local",
	"--disable-slash-commands",
	"--exclude-dynamic-system-prompt-sections",
}

// CarriedUserSettings are the keys of the user's settings.json a lean boot
// still honours: auth, model choice and environment. Everything else, such
// as outputStyle, hooks, enabledPlugins and statusLine, stays behind.
var CarriedUserSettings = []string{"apiKeyHelper", "env", "forceLoginMethod", "forceLoginOrgUUID", "model", "modelSettings", "effortLevel"}

// leanSettings are set in a lean agent's settings.json over what it
// carries from the user's. skipDangerousModePermissionPrompt stands in for
// the consent that lives in the user's settings, which a lean boot does not
// read: without it an interactive agent stops at the bypass dialog.
var leanSettings = map[string]any{
	"outputStyle":                       "default",
	"autoMemoryEnabled":                 false,
	"skipDangerousModePermissionPrompt": true,
}

func carriedSettings(path string) (map[string]any, error) {
	out := map[string]any{}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var user map[string]any
	if err := json.Unmarshal(b, &user); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, k := range CarriedUserSettings {
		if v, ok := user[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func ClaudeJSON() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return ClaudeJSONIn(d), nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(u, ".claude.json"), nil
}

const lockStale = 10 * time.Second

func lockConfig(path string) (func(), error) {
	lock := path + ".lock"
	deadline := time.Now().Add(2 * lockStale)
	for {
		err := os.Mkdir(lock, 0o755)
		if err == nil {
			return func() { os.Remove(lock) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if fi, err := os.Stat(lock); err == nil && time.Since(fi.ModTime()) > lockStale {
			os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("%s is held", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Trust marks dir as trusted in Claude Code's config, so a session started
// there skips the trust dialog.
func Trust(configPath, dir string) error {
	dirs := []string{dir}
	if real := RealPath(dir); real != dir {
		dirs = append(dirs, real)
	}
	return editProjects(configPath, func(projects map[string]map[string]json.RawMessage) bool {
		changed := false
		for _, d := range dirs {
			project := projects[d]
			if string(project["hasTrustDialogAccepted"]) == "true" {
				continue
			}
			if project == nil {
				project = map[string]json.RawMessage{}
			}
			project["hasTrustDialogAccepted"] = json.RawMessage("true")
			projects[d] = project
			changed = true
		}
		return changed
	})
}

func RealPath(path string) string {
	path = filepath.Clean(path)
	var rest []string
	for p := path; ; p = filepath.Dir(p) {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		if filepath.Dir(p) == p {
			return path
		}
		rest = append([]string{filepath.Base(p)}, rest...)
	}
}

// Untrust deletes the projects entries of dirs from Claude Code's config in
// one locked write, but only of the dirs inside home: a folder aeolian does
// not own keeps its entry. It returns how many entries it deleted.
func Untrust(configPath, home string, dirs []string) (int, error) {
	realHome := RealPath(home)
	var own []string
	for _, d := range dirs {
		if !Inside(home, d) {
			continue
		}
		own = append(own, d)
		if real := RealPath(d); real != d && (Inside(home, real) || Inside(realHome, real)) {
			own = append(own, real)
		}
	}
	if len(own) == 0 {
		return 0, nil
	}
	n := 0
	err := editProjects(configPath, func(projects map[string]map[string]json.RawMessage) bool {
		for _, d := range own {
			if _, ok := projects[d]; ok {
				delete(projects, d)
				n++
			}
		}
		return n > 0
	})
	return n, err
}

// Inside reports whether path is strictly below dir, both cleaned and
// absolute.
func Inside(dir, path string) bool {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

// editProjects runs edit on the projects map of Claude Code's config under
// its lock, and writes the config back atomically when edit reports a
// change. Every other key of the config is kept as it was.
func editProjects(configPath string, edit func(map[string]map[string]json.RawMessage) bool) error {
	unlock, err := lockConfig(configPath)
	if err != nil {
		return err
	}
	defer unlock()
	cfg := map[string]json.RawMessage{}
	mode := os.FileMode(0o600)
	b, err := os.ReadFile(configPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return fmt.Errorf("%s: %w", configPath, err)
		}
		if fi, err := os.Stat(configPath); err == nil {
			mode = fi.Mode().Perm()
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	projects := map[string]map[string]json.RawMessage{}
	if raw, ok := cfg["projects"]; ok {
		if err := json.Unmarshal(raw, &projects); err != nil {
			return fmt.Errorf("%s projects: %w", configPath, err)
		}
	}
	if !edit(projects) {
		return nil
	}
	raw, err := json.Marshal(projects)
	if err != nil {
		return err
	}
	cfg["projects"] = raw
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(configPath), ".claude.json.aeolian-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), configPath)
}

func (l Layout) ScratchDir(agentID string) string {
	return filepath.Join(l.Home, "scratch", agentID)
}

func (l Layout) ConfigDir(agentID string) string {
	return filepath.Join(l.Home, "agents", agentID)
}

func (l Layout) MCPConfig(agentID string) string {
	return filepath.Join(l.ConfigDir(agentID), "mcp.json")
}

func (l Layout) Settings(agentID string) string {
	return filepath.Join(l.ConfigDir(agentID), "settings.json")
}

func (l Layout) Log(agentID string) string {
	return filepath.Join(l.ConfigDir(agentID), "out.log")
}

func (l Layout) PromptFile(agentID string) string {
	return filepath.Join(l.ConfigDir(agentID), "prompt")
}

func (l Layout) WritePrompt(agentID, prompt string) error {
	return os.WriteFile(l.PromptFile(agentID), []byte(prompt), 0o600)
}

const PromptArg = "@aeolian-prompt@"

const spliceScript = `p=$(cat -- "$0" && echo .) || exit 1; p=${p%.}; ` +
	`for a do shift; if [ "$a" = '` + PromptArg + `' ]; then set -- "$@" "$p"; else set -- "$@" "$a"; fi; done; exec "$@"`

const (
	PromptScript         = spliceScript
	HeadlessPromptScript = `exec >"$1" 2>&1; shift; ` + spliceScript
)

func (l Layout) LinearEntry() string { return filepath.Join(l.Home, "linear.json") }

func (l Layout) MCPEntry(name string) string { return filepath.Join(l.Home, "mcp", name+".json") }

func (l Layout) Workflows() string { return filepath.Join(l.Home, "workflows") }

// HookEvents are the Claude Code hooks that report to the host. Their
// input goes into the event log: PostToolUse as tool.used, the rest as
// hook events; SessionEnd's reason says how the session closed.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "Stop", "Notification", "PostToolUse", "PreCompact", "SessionEnd"}

// HookTimeout is the curl --max-time of every hook, in seconds: PostToolUse
// runs after every tool call, so a wedged host must not stall the agent.
const HookTimeout = 3

// MCPURL is the host's conductor endpoint for one agent.
func MCPURL(hostURL, agentID string) string { return hostURL + "/mcp/" + agentID }

// HookURL is where the agent's hook for event posts its input.
func HookURL(hostURL, agentID, event string) string {
	return hostURL + "/hooks/" + agentID + "/" + event
}

const HeadlessToolTimeout = 24 * time.Hour

type Setup struct {
	HostURL           string
	Headless          bool
	Linear            bool
	MCP               []string
	Connectors        []string
	DefaultConnectors []string
	GHToken           string
	Bot               Committer
	// Boot is the boot profile; empty means BootFull.
	Boot string
	// ReadRoots, when not nil, confine the agent's file reads (Read, Grep,
	// Glob) to its working directory and these folders, and deny it the
	// host's own files under Home.
	ReadRoots []string
	Env       map[string]string
	GitConfig [][2]string
}

// HostFiles are the paths under $AEOLIAN_HOME a read-confined agent is
// denied even when a read root would let it in: other agents' settings
// (which hold their tokens), the store, and the host's config.
var HostFiles = []string{"agents/**", "aeolian.db*", "*.json", "workflows/**", "vault/**"}

// ReadDeny are the permission deny rules for HostFiles under home.
func ReadDeny(home string) []string {
	rules := make([]string, len(HostFiles))
	for i, f := range HostFiles {
		rules[i] = "Read(/" + filepath.Join(home, f) + ")"
	}
	return rules
}

type Committer struct {
	Name  string
	Email string
}

func CommitterEnv(c Committer) map[string]string {
	return map[string]string{
		"GIT_AUTHOR_NAME":     c.Name,
		"GIT_AUTHOR_EMAIL":    c.Email,
		"GIT_COMMITTER_NAME":  c.Name,
		"GIT_COMMITTER_EMAIL": c.Email,
	}
}

const (
	BotHelper = "!gh auth git-credential"
	NoSSH     = `echo "aeolian: git over ssh is off for headless agents; fetch and push over https://github.com/<owner>/<name>" >&2; exit 128;`
)

func (l Layout) GHConfigDir(agentID string) string {
	return filepath.Join(l.ConfigDir(agentID), "gh")
}

func gitConfigEnv(entries [][2]string) map[string]string {
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(entries))}
	for i, e := range entries {
		n := strconv.Itoa(i)
		env["GIT_CONFIG_KEY_"+n] = e[0]
		env["GIT_CONFIG_VALUE_"+n] = e[1]
	}
	return env
}

func appendGitConfig(env map[string]string, entries [][2]string) {
	if len(entries) == 0 {
		return
	}
	n, err := strconv.Atoi(env["GIT_CONFIG_COUNT"])
	if err != nil || n < 0 {
		n = 0
	}
	for i, e := range entries {
		k := strconv.Itoa(n + i)
		env["GIT_CONFIG_KEY_"+k] = e[0]
		env["GIT_CONFIG_VALUE_"+k] = e[1]
	}
	env["GIT_CONFIG_COUNT"] = strconv.Itoa(n + len(entries))
}

var gitEnvPrefixes = []string{"GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_"}

var gitEnvKeys = []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"}

func reservedEnv() map[string]bool {
	keys := map[string]bool{}
	for _, env := range []map[string]string{
		HeadlessEnv("x", "x"),
		GitHubEnv("x"),
		CommitterEnv(Committer{Name: "x", Email: "x"}),
	} {
		for k := range env {
			keys[k] = true
		}
	}
	argv := Layout{ClaudeDir: "x"}.Argv(Launch{})
	for i, a := range argv {
		if a != "env" {
			continue
		}
		for _, kv := range argv[i+1:] {
			k, _, ok := strings.Cut(kv, "=")
			if !ok {
				break
			}
			keys[k] = true
		}
	}
	return keys
}

func hostGitConfig() map[string]bool {
	keys := map[string]bool{}
	for _, env := range []map[string]string{HeadlessEnv("x", "x"), HeadlessEnv("x", ""), GitHubEnv("x")} {
		for k, v := range env {
			if strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
				keys[canonicalGitKey(v)] = true
			}
		}
		for _, k := range gitConfigParameterKeys(env["GIT_CONFIG_PARAMETERS"]) {
			keys[canonicalGitKey(k)] = true
		}
	}
	return keys
}

func gitConfigParameterKeys(params string) []string {
	var keys []string
	var cur strings.Builder
	quoted, sawEq, done := false, false, false
	flush := func() {
		if cur.Len() > 0 || sawEq {
			k := cur.String()
			if !sawEq {
				k, _, _ = strings.Cut(k, "=")
			}
			keys = append(keys, k)
		}
		cur.Reset()
		sawEq, done = false, false
	}
	for i := 0; i < len(params); i++ {
		c := params[i]
		switch {
		case c == '\'':
			quoted = !quoted
		case quoted:
			if !done {
				cur.WriteByte(c)
			}
		case c == '\\' && i+1 < len(params):
			i++
			if !done {
				cur.WriteByte(params[i])
			}
		case c == '=':
			if !done {
				sawEq, done = true, true
			}
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			if !done {
				cur.WriteByte(c)
			}
		}
	}
	flush()
	return keys
}

func canonicalGitKey(k string) string {
	section, rest, ok := strings.Cut(k, ".")
	if !ok {
		return strings.ToLower(k)
	}
	i := strings.LastIndexByte(rest, '.')
	if i < 0 {
		return strings.ToLower(section) + "." + strings.ToLower(rest)
	}
	return strings.ToLower(section) + "." + rest[:i] + "." + strings.ToLower(rest[i+1:])
}

func CheckEnv(env map[string]string) error {
	reserved := reservedEnv()
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("env: %q is not a variable name", k)
		}
		git := slices.Contains(gitEnvKeys, k) || slices.ContainsFunc(gitEnvPrefixes, func(p string) bool { return strings.HasPrefix(k, p) })
		if git {
			return fmt.Errorf("env: %s is reserved by aeolian; pass git settings as git_config", k)
		}
		if reserved[k] {
			return fmt.Errorf("env: %s is reserved by aeolian", k)
		}
	}
	return nil
}

func CheckGitConfig(entries [][2]string) error {
	host := hostGitConfig()
	for _, e := range entries {
		if strings.TrimSpace(e[0]) == "" {
			return errors.New("git_config: empty key")
		}
		if !gitConfigKey(e[0]) {
			return fmt.Errorf("git_config: %s is not a git config key", e[0])
		}
		if host[canonicalGitKey(e[0])] {
			return fmt.Errorf("git_config: %s is set by aeolian", e[0])
		}
	}
	return nil
}

func gitConfigKey(k string) bool {
	if strings.ContainsAny(k, "\n\x00") {
		return false
	}
	section, rest, ok := strings.Cut(k, ".")
	if !ok {
		return false
	}
	i := strings.LastIndexByte(rest, '.')
	name := rest[i+1:]
	if i == 0 {
		return false
	}
	alnum := func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, func(r rune) bool {
			return !(r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
		})
	}
	letter := name != "" && (name[0] >= 'a' && name[0] <= 'z' || name[0] >= 'A' && name[0] <= 'Z')
	return alnum(section) && alnum(name) && letter
}

func GitHubEnv(token string) map[string]string {
	env := gitConfigEnv([][2]string{
		{"credential.https://github.com.helper", ""},
		{"credential.https://github.com.helper", BotHelper},
	})
	env["GH_TOKEN"] = token
	return env
}

func HeadlessEnv(ghConfigDir, token string) map[string]string {
	helpers := [][2]string{
		{"credential.helper", ""},
		{"credential.https://github.com.helper", ""},
	}
	if token != "" {
		helpers = append(helpers, [2]string{"credential.https://github.com.helper", BotHelper})
	}
	env := gitConfigEnv(helpers)
	maps.Copy(env, map[string]string{
		"SSH_AUTH_SOCK":       "",
		"GIT_SSH_COMMAND":     NoSSH,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"GH_CONFIG_DIR":       ghConfigDir,
		"GH_TOKEN":            token,
		"GITHUB_TOKEN":        "",
		"MCP_TOOL_TIMEOUT":    strconv.FormatInt(HeadlessToolTimeout.Milliseconds(), 10),
	})
	return env
}

// KeptLog is where garbage collection keeps a failed agent's Log once it
// removes the agent's config folder.
func (l Layout) KeptLog(agentID string) string {
	return filepath.Join(l.Home, "logs", agentID+".log")
}

// Prepare writes the agent's config folder and trusts dir, the folder its
// session starts in. The scratch folder is made only when dir is it: an
// agent started in a worktree gets no empty scratch folder.
func (l Layout) Prepare(agentID, dir string, s Setup) error {
	dirs := []string{l.ConfigDir(agentID)}
	if dir == l.ScratchDir(agentID) {
		dirs = append(dirs, dir)
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if l.ClaudeJSON != "" && dir != "" {
		if err := Trust(l.ClaudeJSON, dir); err != nil {
			return fmt.Errorf("trust %s: %w", dir, err)
		}
	}
	servers := map[string]json.RawMessage{}
	entries := map[string]string{}
	if s.Linear {
		entries["linear"] = l.LinearEntry()
	}
	for _, name := range s.MCP {
		entries[name] = l.MCPEntry(name)
	}
	for name, path := range entries {
		entry, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%s grant: %w", name, err)
		}
		if !json.Valid(entry) {
			return fmt.Errorf("%s is not valid JSON", path)
		}
		servers[name] = entry
	}
	settings := map[string]any{}
	env := map[string]string{}
	if s.Boot == BootLean {
		carried, err := carriedSettings(l.UserSettings)
		if err != nil {
			return fmt.Errorf("user settings: %w", err)
		}
		if e, ok := carried["env"].(map[string]any); ok {
			for k, v := range e {
				if v, ok := v.(string); ok {
					env[k] = v
				}
			}
		}
		delete(carried, "env")
		maps.Copy(settings, carried)
		maps.Copy(settings, leanSettings)
	}
	maps.Copy(env, s.Env)
	if s.ReadRoots != nil {
		roots := []string{}
		for _, r := range s.ReadRoots {
			if r != "" && !slices.Contains(roots, r) {
				roots = append(roots, r)
			}
		}
		settings["permissions"] = map[string]any{
			"blockReadsOutsideWorkingDirectories": true,
			"additionalDirectories":               roots,
			"deny":                                ReadDeny(l.Home),
		}
	}
	switch {
	case s.Headless:
		if err := os.MkdirAll(l.GHConfigDir(agentID), 0o700); err != nil {
			return err
		}
		maps.Copy(env, HeadlessEnv(l.GHConfigDir(agentID), s.GHToken))
		if s.Bot != (Committer{}) {
			maps.Copy(env, CommitterEnv(s.Bot))
		}
	case s.GHToken != "":
		maps.Copy(env, GitHubEnv(s.GHToken))
	}
	appendGitConfig(env, s.GitConfig)
	if len(env) > 0 {
		settings["env"] = env
	}
	if s.HostURL != "" {
		conductor, err := json.Marshal(map[string]string{"type": "http", "url": MCPURL(s.HostURL, agentID)})
		if err != nil {
			return err
		}
		servers["aeolian"] = conductor
		hooks := map[string]any{}
		for _, event := range HookEvents {
			command := fmt.Sprintf("curl -s -m %d --data-binary @- '%s'", HookTimeout, HookURL(s.HostURL, agentID, event))
			hooks[event] = []any{map[string]any{
				"hooks": []any{map[string]string{"type": "command", "command": command}},
			}}
		}
		settings["hooks"] = hooks
	}
	if err := writeJSON(l.MCPConfig(agentID), map[string]any{"mcpServers": servers}); err != nil {
		return err
	}
	return writeJSON(l.Settings(agentID), settings)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Launch describes one run of an agent's terminal. A non-empty Prompt starts
// a new conversation with SessionID; Resume continues the existing one.
// Headless runs the new conversation with claude -p, which exits when it
// ends; Resume is always interactive.
type Launch struct {
	AgentID   string
	SessionID string
	Prompt    string
	Resume    bool
	Model     string
	Headless  bool
	Memory    string
	// Boot is the boot profile; empty means BootFull.
	Boot       string
	Connectors bool
	Deny       []string
}

// SessionName is the tmux session that holds a Claude Code session.
func SessionName(sessionID string) string { return keeper.SessionPrefix + sessionID }

// Argv is the full command for the tmux pane: claude inside a memory-limited
// user scope. The prompt must stay the first argument after claude, or after
// -p, because --mcp-config and --disallowedTools take several values and
// swallow a trailing one. A headless launch runs under sh, which sends its stdout and
// stderr to the agent's Log; the prompt reaches claude through "$@", never
// through the script's text.
func (l Layout) Argv(la Launch) []string {
	var argv []string
	headless := la.Headless && !la.Resume
	switch {
	case headless:
		argv = []string{"sh", "-c", HeadlessPromptScript, l.PromptFile(la.AgentID), l.Log(la.AgentID)}
	case !la.Resume:
		argv = []string{"sh", "-c", PromptScript, l.PromptFile(la.AgentID)}
	}
	high, limit, err := MemoryLimits(la.Memory)
	if err != nil {
		high, limit = MemoryHigh, MemoryMax
	}
	argv = append(argv,
		"systemd-run", "--user", "--scope", "--quiet", "--expand-environment=no",
		"-p", "MemoryHigh="+high, "-p", "MemoryMax="+limit,
		"--",
	)
	if l.ClaudeDir != "" {
		argv = append(argv, "env", "CLAUDE_CONFIG_DIR="+l.ClaudeDir)
	}
	argv = append(argv, "claude")
	switch {
	case la.Resume:
		argv = append(argv, "--resume", la.SessionID)
	case headless:
		argv = append(argv, "-p", PromptArg, "--session-id", la.SessionID)
	default:
		argv = append(argv, PromptArg, "--session-id", la.SessionID)
	}
	if la.Model != "" {
		argv = append(argv, "--model", la.Model)
	}
	if la.Boot == BootLean {
		argv = append(argv, LeanArgs...)
	}
	argv = append(argv,
		"--dangerously-skip-permissions",
		"--settings", l.Settings(la.AgentID),
	)
	if !la.Connectors {
		argv = append(argv, "--strict-mcp-config")
	}
	return append(argv,
		"--mcp-config", l.MCPConfig(la.AgentID),
		"--disallowedTools", strings.Join(slices.Concat(DeniedTools, la.Deny), ","),
	)
}

// NewSessionID returns a random version 4 UUID.
func NewSessionID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
