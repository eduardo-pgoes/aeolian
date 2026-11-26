package agent

import (
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestArgvPutsPromptFirst(t *testing.T) {
	l := Layout{Home: "/h"}
	argv := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: "do it"})
	i := slices.Index(argv, "claude")
	if i < 0 || argv[i+1] != PromptArg || slices.Contains(argv, "do it") {
		t.Fatalf("prompt not right after claude: %q", argv)
	}
	for _, want := range []string{"systemd-run", "--scope", "MemoryHigh=800M", "MemoryMax=1G", "--session-id", "--strict-mcp-config", "/h/agents/a1/mcp.json", "/h/agents/a1/settings.json", "--dangerously-skip-permissions"} {
		if !slices.Contains(argv, want) {
			t.Errorf("argv lacks %q: %q", want, argv)
		}
	}
	if !slices.Equal(argv[:4], []string{"sh", "-c", PromptScript, "/h/agents/a1/prompt"}) || argv[4] != "systemd-run" {
		t.Errorf("the prompt splice must wrap the scope: %q", argv)
	}
}

func TestArgvResume(t *testing.T) {
	argv := Layout{Home: "/h"}.Argv(Launch{AgentID: "a1", SessionID: "s1", Resume: true})
	i := slices.Index(argv, "claude")
	if argv[i+1] != "--resume" || argv[i+2] != "s1" || slices.Contains(argv, "--session-id") {
		t.Fatalf("bad resume argv: %q", argv)
	}
}

func TestArgvDeniesAgent(t *testing.T) {
	argv := Layout{Home: "/h"}.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p"})
	if slices.Contains(argv, "--tools") {
		t.Fatalf("an allowlist drops tools newer Claude Code adds: %q", argv)
	}
	denied := strings.Split(argv[slices.Index(argv, "--disallowedTools")+1], ",")
	if !slices.Contains(denied, "Agent") || !slices.Contains(denied, "Task") {
		t.Fatalf("denied: %q", denied)
	}
}

func readMCP(t *testing.T, l Layout, s Setup) map[string]json.RawMessage {
	t.Helper()
	if err := l.Prepare("a1", l.ScratchDir("a1"), s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.MCPConfig("a1"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ MCPServers map[string]json.RawMessage }
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.MCPServers == nil {
		t.Fatalf("no mcpServers key: %s", b)
	}
	return cfg.MCPServers
}

func TestPrepareWithoutLinear(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if servers := readMCP(t, l, Setup{}); len(servers) != 0 {
		t.Fatalf("want no servers, got %v", servers)
	}
	if fi, err := os.Stat(l.ScratchDir("a1")); err != nil || !fi.IsDir() {
		t.Fatalf("scratch dir: %v", err)
	}
	if _, err := os.Stat(l.Settings("a1")); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareCopiesLinear(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	entry := `{"type":"http","url":"https://mcp.linear.app/mcp"}`
	os.WriteFile(l.LinearEntry(), []byte(entry), 0o600)
	if servers := readMCP(t, l, Setup{HostURL: "http://h"}); servers["linear"] != nil {
		t.Fatalf("linear without the grant: %s", servers["linear"])
	}
	servers := readMCP(t, l, Setup{Linear: true})
	var got, want any
	json.Unmarshal(servers["linear"], &got)
	json.Unmarshal([]byte(entry), &want)
	if len(servers) != 1 || got.(map[string]any)["url"] != want.(map[string]any)["url"] {
		t.Fatalf("servers: %v", servers)
	}
}

func TestPrepareCopiesMCPGrants(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	os.MkdirAll(l.Home+"/mcp", 0o700)
	entry := `{"type":"stdio","command":"slack-mcp"}`
	os.WriteFile(l.MCPEntry("slack"), []byte(entry), 0o600)
	if servers := readMCP(t, l, Setup{}); servers["slack"] != nil {
		t.Fatalf("slack without the grant: %s", servers["slack"])
	}
	servers := readMCP(t, l, Setup{MCP: []string{"slack"}})
	if len(servers) != 1 || !strings.Contains(string(servers["slack"]), "slack-mcp") {
		t.Fatalf("servers: %v", servers)
	}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{MCP: []string{"notion"}}); err == nil || !strings.Contains(err.Error(), "notion grant") {
		t.Fatalf("missing entry: %v", err)
	}
	os.WriteFile(l.MCPEntry("bad"), []byte("{"), 0o600)
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{MCP: []string{"bad"}}); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("bad entry: %v", err)
	}
}

func TestPrepareWithHost(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{HostURL: "http://127.0.0.1:7431"}); err != nil {
		t.Fatal(err)
	}
	var mcp struct {
		MCPServers map[string]struct{ Type, URL string }
	}
	b, _ := os.ReadFile(l.MCPConfig("a1"))
	if err := json.Unmarshal(b, &mcp); err != nil {
		t.Fatal(err)
	}
	if got := mcp.MCPServers["aeolian"]; got.Type != "http" || got.URL != "http://127.0.0.1:7431/mcp/a1" {
		t.Fatalf("aeolian server: %s", b)
	}

	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Type, Command string }
		}
	}
	b, _ = os.ReadFile(l.Settings("a1"))
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	for _, event := range HookEvents {
		h := settings.Hooks[event]
		if len(h) != 1 || len(h[0].Hooks) != 1 {
			t.Fatalf("%s hooks: %s", event, b)
		}
		want := "curl -s -m 3 --data-binary @- 'http://127.0.0.1:7431/hooks/a1/" + event + "'"
		if c := h[0].Hooks[0]; c.Type != "command" || c.Command != want {
			t.Errorf("%s hook: %+v", event, c)
		}
	}
}

func TestPrepareWithoutHostHasNoHooks(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(l.Settings("a1")); strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("settings: %s", b)
	}
}

func TestNewSessionIDIsUUIDv4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if id := NewSessionID(); !re.MatchString(id) {
		t.Fatal(id)
	}
}

func TestPrepareTrustsScratch(t *testing.T) {
	cj := t.TempDir() + "/.claude.json"
	if err := os.WriteFile(cj, []byte(`{"userID":"u","projects":{"/other":{"hasTrustDialogAccepted":true,"x":[1]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Layout{Home: t.TempDir(), ClaudeJSON: cj}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{}); err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		UserID   string
		Projects map[string]map[string]any
	}
	b, _ := os.ReadFile(cj)
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.UserID != "u" || cfg.Projects["/other"]["x"] == nil {
		t.Fatalf("lost existing keys: %s", b)
	}
	if cfg.Projects[l.ScratchDir("a1")]["hasTrustDialogAccepted"] != true {
		t.Fatalf("scratch not trusted: %s", b)
	}
	if fi, _ := os.Stat(cj); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed to %v", fi.Mode())
	}
	before, _ := os.Stat(cj)
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(cj)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("rewrote an already trusted entry")
	}
	if _, err := os.Stat(cj + ".lock"); !os.IsNotExist(err) {
		t.Fatal("lock left behind")
	}
}

func TestArgvModel(t *testing.T) {
	l := Layout{Home: "/h"}
	if argv := l.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p"}); slices.Contains(argv, "--model") {
		t.Fatalf("model without one set: %q", argv)
	}
	argv := l.Argv(Launch{AgentID: "a", SessionID: "s", Resume: true, Model: "sonnet"})
	if i := slices.Index(argv, "--model"); i < 0 || argv[i+1] != "sonnet" {
		t.Fatalf("argv: %q", argv)
	}
}

func TestArgvHeadless(t *testing.T) {
	l := Layout{Home: "/h"}
	prompt := `it's "$(touch pwned)" $0`
	argv := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: prompt, Headless: true})
	if !slices.Equal(argv[:5], []string{"sh", "-c", HeadlessPromptScript, "/h/agents/a1/prompt", "/h/agents/a1/out.log"}) || argv[5] != "systemd-run" {
		t.Fatalf("headless argv must run the scope under sh: %q", argv)
	}
	i := slices.Index(argv, "claude")
	if argv[i+1] != "-p" || argv[i+2] != PromptArg || argv[i+3] != "--session-id" || argv[i+4] != "s1" {
		t.Fatalf("prompt not right after -p: %q", argv)
	}
	interactive := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: prompt})
	if !slices.Equal(argv[5:i+1], interactive[4:slices.Index(interactive, "claude")+1]) || !slices.Equal(argv[i+3:], interactive[slices.Index(interactive, "claude")+2:]) {
		t.Fatalf("headless flags differ from interactive:\n%q\n%q", argv, interactive)
	}
	resume := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Resume: true, Headless: true})
	if resume[0] != "systemd-run" || resume[slices.Index(resume, "claude")+1] != "--resume" {
		t.Fatalf("resume runs headless: %q", resume)
	}
}

func TestHeadlessShellKeepsArgsLiteral(t *testing.T) {
	dir := t.TempDir()
	l := Layout{Home: dir}
	os.MkdirAll(l.ConfigDir("a1"), 0o755)
	prompt := `it's "$(touch pwned)" $0 ` + "`touch pwned`"
	if err := l.WritePrompt("a1", prompt); err != nil {
		t.Fatal(err)
	}
	argv := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: prompt, Headless: true})
	cmd := exec.Command(argv[0], append(argv[1:5], "sh", "-c", `printf '%s\n' "$1"; echo oops >&2; exit 3`, "x", PromptArg)...)
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatal("exit status lost")
	}
	b, err := os.ReadFile(l.Log("a1"))
	if err != nil || string(b) != prompt+"\noops\n" {
		t.Fatalf("log %q: %v", b, err)
	}
	if _, err := os.Stat(dir + "/pwned"); !os.IsNotExist(err) {
		t.Fatal("prompt ran as shell")
	}
}

func TestPrepareHeadlessRaisesToolTimeout(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	read := func(s Setup) map[string]any {
		t.Helper()
		if err := l.Prepare("a1", l.ScratchDir("a1"), s); err != nil {
			t.Fatal(err)
		}
		var settings map[string]any
		b, _ := os.ReadFile(l.Settings("a1"))
		if err := json.Unmarshal(b, &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}
	got := read(Setup{HostURL: "http://127.0.0.1:7431", Headless: true})
	if env, _ := got["env"].(map[string]any); env["MCP_TOOL_TIMEOUT"] != "86400000" || got["hooks"] == nil {
		t.Fatalf("headless settings: %v", got)
	}
	if got := read(Setup{HostURL: "http://127.0.0.1:7431"}); got["env"] != nil {
		t.Fatalf("interactive settings: %v", got)
	}
}

func TestPrepareLinearGrantNeedsEntry(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{Linear: true}); err == nil || !strings.Contains(err.Error(), "linear.json") {
		t.Fatalf("err %v", err)
	}
}

func readSettings(t *testing.T, l Layout, id string) (map[string]any, string) {
	t.Helper()
	b, err := os.ReadFile(l.Settings(id))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	return settings, string(b)
}

func TestPrepareGitHubToken(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	os.MkdirAll(l.ConfigDir("a1"), 0o755)
	os.WriteFile(l.Settings("a1"), []byte("{}"), 0o644)
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{HostURL: "http://h", Headless: true, GHToken: "ghs_secret"}); err != nil {
		t.Fatal(err)
	}
	settings, _ := readSettings(t, l, "a1")
	env, _ := settings["env"].(map[string]any)
	want := map[string]any{
		"MCP_TOOL_TIMEOUT":    "86400000",
		"SSH_AUTH_SOCK":       "",
		"GIT_SSH_COMMAND":     NoSSH,
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"GH_CONFIG_DIR":       l.GHConfigDir("a1"),
		"GH_TOKEN":            "ghs_secret",
		"GITHUB_TOKEN":        "",
		"GIT_CONFIG_COUNT":    "3",
		"GIT_CONFIG_KEY_0":    "credential.helper",
		"GIT_CONFIG_VALUE_0":  "",
		"GIT_CONFIG_KEY_1":    "credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_1":  "",
		"GIT_CONFIG_KEY_2":    "credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_2":  "!gh auth git-credential",
	}
	if !maps.Equal(env, want) || settings["hooks"] == nil {
		t.Fatalf("settings: %v", settings)
	}
	for _, path := range []string{l.Settings("a1"), l.MCPConfig("a1")} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", path, fi.Mode(), err)
		}
	}
	if fi, err := os.Stat(l.GHConfigDir("a1")); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("gh config dir: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(l.ConfigDir("a1"))
	if len(entries) != 3 {
		t.Fatalf("temp files left: %v", entries)
	}

	if err := l.Prepare("a2", l.ScratchDir("a2"), Setup{HostURL: "http://h", Headless: true}); err != nil {
		t.Fatal(err)
	}
	settings, raw := readSettings(t, l, "a2")
	env, _ = settings["env"].(map[string]any)
	maps.DeleteFunc(want, func(k string, _ any) bool { return strings.HasPrefix(k, "GIT_CONFIG_") })
	want["GH_TOKEN"], want["GH_CONFIG_DIR"] = "", l.GHConfigDir("a2")
	want["GIT_CONFIG_COUNT"] = "2"
	want["GIT_CONFIG_KEY_0"], want["GIT_CONFIG_VALUE_0"] = "credential.helper", ""
	want["GIT_CONFIG_KEY_1"], want["GIT_CONFIG_VALUE_1"] = "credential.https://github.com.helper", ""
	if !maps.Equal(env, want) || strings.Contains(raw, "ghs_secret") || strings.Contains(raw, BotHelper) {
		t.Fatalf("ungranted settings: %s", raw)
	}

	if err := l.Prepare("a3", l.ScratchDir("a3"), Setup{HostURL: "http://h", GHToken: "ghs_secret"}); err != nil {
		t.Fatal(err)
	}
	settings, raw = readSettings(t, l, "a3")
	env, _ = settings["env"].(map[string]any)
	if !maps.Equal(env, map[string]any{
		"GH_TOKEN":           "ghs_secret",
		"GIT_CONFIG_COUNT":   "2",
		"GIT_CONFIG_KEY_0":   "credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0": "",
		"GIT_CONFIG_KEY_1":   "credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_1": "!gh auth git-credential",
	}) {
		t.Fatalf("interactive granted settings: %s", raw)
	}
	if _, err := os.Stat(l.GHConfigDir("a3")); !os.IsNotExist(err) {
		t.Fatalf("interactive agent got a gh config dir: %v", err)
	}
}

func fakeEngineerGit(t *testing.T) (string, []string) {
	t.Helper()
	for _, bin := range []string{"gh", "git"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	dir := t.TempDir()
	global := dir + "/gitconfig"
	os.WriteFile(global, []byte(`[credential]
	helper = "!f() { echo username=engineer; echo password=ENGINEER; }; f"
[credential "https://github.com"]
	helper = "!f() { echo username=engineer; echo password=ENGINEER-GH; }; f"
[core]
	sshCommand = "echo ENGINEER-SSH >&2; exit 1"
`), 0o600)
	env := []string{"GIT_CONFIG_GLOBAL=" + global, "GIT_CONFIG_NOSYSTEM=1", "HOME=" + dir}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(k, "GIT_") && !strings.HasPrefix(k, "GH_") && k != "GITHUB_TOKEN" && k != "HOME" {
			env = append(env, kv)
		}
	}
	return dir, env
}

func withEnv(env []string, extra map[string]string) []string {
	out := slices.Clone(env)
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

func TestHeadlessEnvResetsEngineerCredentials(t *testing.T) {
	dir, env := fakeEngineerGit(t)
	gh := dir + "/gh"
	os.Mkdir(gh, 0o700)
	fill := func(extra map[string]string) (string, error) {
		cmd := exec.Command("git", "credential", "fill")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/api.git\n\n")
		cmd.Env = withEnv(env, extra)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, _ := fill(nil); !strings.Contains(out, "password=ENGINEER") {
		t.Fatalf("fake engineer helper not in effect: %s", out)
	}
	out, err := fill(HeadlessEnv(gh, ""))
	if err == nil || strings.Contains(out, "ENGINEER") || !strings.Contains(out, "terminal prompts disabled") {
		t.Fatalf("ungranted fill: %v %s", err, out)
	}
	out, err = fill(HeadlessEnv(gh, "ghs_secret"))
	if err != nil || !strings.Contains(out, "password=ghs_secret\n") || strings.Contains(out, "ENGINEER") {
		t.Fatalf("granted fill: %v %s", err, out)
	}

	cmd := exec.Command("git", "ls-remote", "git@github.com:acme/api.git")
	cmd.Dir = dir
	cmd.Env = withEnv(env, HeadlessEnv(gh, "ghs_secret"))
	b, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(b), "aeolian: git over ssh is off") || strings.Contains(string(b), "ENGINEER-SSH") {
		t.Fatalf("ssh not blocked: %v %s", err, b)
	}
}

func TestMemoryLimits(t *testing.T) {
	for _, c := range []struct{ in, high, limit string }{
		{"", "800M", "1G"},
		{"4G", "3276M", "4G"},
		{"3G", "2457M", "3G"},
		{"1536M", "1228M", "1536M"},
		{"512M", "409M", "512M"},
	} {
		high, limit, err := MemoryLimits(c.in)
		if err != nil || high != c.high || limit != c.limit {
			t.Errorf("MemoryLimits(%q) = %q %q %v", c.in, high, limit, err)
		}
	}
	for _, bad := range []string{"4", "4g", "4GB", "0G", "256M", "-1G", "1.5G", "04G", "infinity", "50%"} {
		if _, _, err := MemoryLimits(bad); err == nil {
			t.Errorf("MemoryLimits(%q) accepted", bad)
		}
	}
	argv := Layout{Home: "/h"}.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p", Memory: "4G"})
	if !slices.Contains(argv, "MemoryHigh=3276M") || !slices.Contains(argv, "MemoryMax=4G") {
		t.Fatalf("argv: %q", argv)
	}
}

func TestGitHubEnvHandsGitTheToken(t *testing.T) {
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not installed")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	global := dir + "/gitconfig"
	os.WriteFile(global, []byte(`[credential]
	helper = "!f() { echo username=engineer; echo password=ENGINEER; }; f"
[credential "https://github.com"]
	helper = "!f() { echo username=engineer; echo password=ENGINEER-GH; }; f"
`), 0o600)
	cmd := exec.Command("git", "credential", "fill")
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=acme/api.git\n\n")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+global, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GH_CONFIG_DIR="+dir)
	for k, v := range GitHubEnv("ghs_secret") {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "password=ghs_secret\n") {
		t.Fatalf("git got: %s", out)
	}
}

func TestPrepareBotCommitsHeadlessOnly(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	bot := Committer{Name: "acme-agent[bot]", Email: "55501+acme-agent[bot]@users.noreply.github.com"}
	idents := []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"}

	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{HostURL: "http://h", Headless: true, GHToken: "ghs_secret", Bot: bot}); err != nil {
		t.Fatal(err)
	}
	settings, _ := readSettings(t, l, "a1")
	env, _ := settings["env"].(map[string]any)
	for _, k := range idents {
		want := bot.Name
		if strings.HasSuffix(k, "_EMAIL") {
			want = bot.Email
		}
		if env[k] != want {
			t.Errorf("headless %s = %v, want %q", k, env[k], want)
		}
	}
	if env["GH_TOKEN"] != "ghs_secret" || env["MCP_TOOL_TIMEOUT"] != "86400000" || env["GIT_SSH_COMMAND"] != NoSSH || env["GIT_CONFIG_COUNT"] != "3" {
		t.Fatalf("bot identity displaced the headless env: %v", env)
	}

	for id, s := range map[string]Setup{
		"a2": {HostURL: "http://h", Headless: true},
		"a3": {HostURL: "http://h", Bot: bot},
		"a4": {HostURL: "http://h", GHToken: "ghs_secret", Bot: bot},
	} {
		if err := l.Prepare(id, l.ScratchDir(id), s); err != nil {
			t.Fatal(err)
		}
		_, raw := readSettings(t, l, id)
		for _, k := range idents {
			if strings.Contains(raw, k) {
				t.Errorf("%s (%+v) has %s: %s", id, s, k, raw)
			}
		}
	}
}

func TestCommitterEnvLeavesGitConfigToTheEngineer(t *testing.T) {
	dir, env := fakeEngineerGit(t)
	os.WriteFile(dir+"/gitconfig", []byte("[user]\n\tname = Engi Neer\n\temail = engi@example.com\n"), 0o600)
	bot := Committer{Name: "acme-agent[bot]", Email: "55501+acme-agent[bot]@users.noreply.github.com"}
	env = withEnv(env, CommitterEnv(bot))
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "repo")
	dir += "/repo"
	name, email := git("config", "user.name"), git("config", "user.email")
	if name != "Engi Neer" || email != "engi@example.com" {
		t.Fatalf("git config gave %q <%s>", name, email)
	}
	git("commit", "-q", "--allow-empty", "-m", "feat: x\n\nCo-authored-by: "+name+" <"+email+">")
	got := git("log", "-1", "--format=%an <%ae>|%cn <%ce>|%(trailers:key=Co-authored-by,valueonly)")
	want := bot.Name + " <" + bot.Email + ">|" + bot.Name + " <" + bot.Email + ">|Engi Neer <engi@example.com>"
	if got != want {
		t.Fatalf("log %q, want %q", got, want)
	}
}

func TestForAccount(t *testing.T) {
	base := Layout{Home: "/h", ClaudeJSON: "/u/.claude.json", UserSettings: "/u/.claude/settings.json"}
	if got := base.ForAccount(""); got != base {
		t.Fatalf("empty account changed the layout: %+v", got)
	}
	got := base.ForAccount("/acct/work")
	want := Layout{Home: "/h", ClaudeJSON: "/acct/work/.claude.json", UserSettings: "/acct/work/settings.json", ClaudeDir: "/acct/work"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestArgvSetsAccountConfigDir(t *testing.T) {
	plain := Layout{Home: "/h"}.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p"})
	if slices.ContainsFunc(plain, func(a string) bool { return strings.HasPrefix(a, "CLAUDE_CONFIG_DIR=") }) {
		t.Fatalf("no account, yet a config dir: %q", plain)
	}
	for _, la := range []Launch{
		{AgentID: "a", SessionID: "s", Prompt: "p"},
		{AgentID: "a", SessionID: "s", Prompt: "p", Headless: true},
		{AgentID: "a", SessionID: "s", Resume: true},
	} {
		argv := Layout{Home: "/h"}.ForAccount("/acct/work").Argv(la)
		c := slices.Index(argv, "claude")
		if c < 2 || argv[c-2] != "env" || argv[c-1] != "CLAUDE_CONFIG_DIR=/acct/work" || argv[c-3] != "--" {
			t.Fatalf("config dir not set right before claude: %q", argv)
		}
		if la.Prompt != "" && !la.Headless && argv[c+1] != PromptArg {
			t.Fatalf("prompt moved: %q", argv)
		}
	}
}

func TestPrepareTrustsInAccount(t *testing.T) {
	home, acct := t.TempDir(), t.TempDir()
	if err := os.WriteFile(UserSettingsIn(acct), []byte(`{"apiKeyHelper":"work-key","hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l := Layout{Home: home, ClaudeJSON: home + "/host.json"}.ForAccount(acct)
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{Boot: BootLean}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ClaudeJSONIn(acct))
	if err != nil || !strings.Contains(string(b), l.ScratchDir("a1")) {
		t.Fatalf("account .claude.json: %s %v", b, err)
	}
	if _, err := os.Stat(home + "/host.json"); !os.IsNotExist(err) {
		t.Fatalf("host .claude.json touched: %v", err)
	}
	s, _ := os.ReadFile(l.Settings("a1"))
	if !strings.Contains(string(s), "work-key") || strings.Contains(string(s), `"hooks": {}`) {
		t.Fatalf("lean settings did not carry from the account: %s", s)
	}
}

func TestPromptSpliceDeliversPromptVerbatim(t *testing.T) {
	l := Layout{Home: t.TempDir(), ClaudeDir: "/acct"}
	if err := os.MkdirAll(l.ConfigDir("a1"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := "-p it's \"$(touch pwned)\" $0 `touch pwned` " + PromptArg + "\n" + strings.Repeat("x", 64<<10) + "\n\n"
	if err := l.WritePrompt("a1", prompt); err != nil {
		t.Fatal(err)
	}
	argv := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: prompt})
	if n := len(strings.Join(argv, " ")); n > 4096 {
		t.Fatalf("argv is %d bytes; the prompt rides the tmux command line", n)
	}
	s, c := slices.Index(argv, "systemd-run"), slices.Index(argv, "claude")
	want := slices.Clone(argv[c+1:])
	want[0] = prompt
	argv = append(append(slices.Clone(argv[:s]), "sh", "-c", `printf '%s\0' "$@"`, "claude"), argv[c+1:]...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = l.Home
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if !slices.Equal(got, want) {
		t.Fatalf("claude got %d args, first %q", len(got), got[0][:min(len(got[0]), 80)])
	}
	if _, err := os.Stat(l.Home + "/pwned"); !os.IsNotExist(err) {
		t.Fatal("prompt ran as shell")
	}
}

func TestPromptSpliceFailsWithoutFile(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	argv := l.Argv(Launch{AgentID: "a1", SessionID: "s1", Prompt: "p"})
	s := slices.Index(argv, "systemd-run")
	cmd := exec.Command(argv[0], append(slices.Clone(argv[1:s]), "touch", "ran")...)
	cmd.Dir = l.Home
	if err := cmd.Run(); err == nil {
		t.Fatal("ran without a prompt file")
	}
	if _, err := os.Stat(l.Home + "/ran"); !os.IsNotExist(err) {
		t.Fatal("command ran without its prompt")
	}
}
