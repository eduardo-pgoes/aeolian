package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestCheckEnvRefusesReservedKeys(t *testing.T) {
	plain := []string{
		"GH_TOKEN", "GITHUB_TOKEN", "GH_CONFIG_DIR", "SSH_AUTH_SOCK", "GIT_SSH_COMMAND", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS",
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "CLAUDE_CONFIG_DIR", "MCP_TOOL_TIMEOUT",
	}
	for _, k := range plain {
		err := CheckEnv(map[string]string{"AYA_ITEM": "x", k: "y"})
		if err == nil || err.Error() != "env: "+k+" is reserved by aeolian" {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_7", "GIT_CONFIG_KEY_99"} {
		err := CheckEnv(map[string]string{k: "y"})
		if err == nil || err.Error() != "env: "+k+" is reserved by aeolian; pass git settings as git_config" {
			t.Errorf("%s: %v", k, err)
		}
	}
	for _, k := range []string{"", "A=B", "A\x00"} {
		if err := CheckEnv(map[string]string{k: "y"}); err == nil {
			t.Errorf("%q accepted", k)
		}
	}
	if err := CheckEnv(map[string]string{"AYA_ITEM": "x", "GIT_DIR_X": "y"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckEnv(nil); err != nil {
		t.Fatal(err)
	}
}

func TestReservedEnvCoversWhatTheHostSets(t *testing.T) {
	l := Layout{Home: t.TempDir(), ClaudeDir: "/acct"}
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{HostURL: "http://h", Headless: true, GHToken: "t", Bot: Committer{Name: "b", Email: "b@x"}}); err != nil {
		t.Fatal(err)
	}
	settings, _ := readSettings(t, l, "a1")
	env, _ := settings["env"].(map[string]any)
	for k := range env {
		if CheckEnv(map[string]string{k: "y"}) == nil {
			t.Errorf("%s is set by the host but not reserved", k)
		}
	}
}

func TestCheckGitConfig(t *testing.T) {
	cases := map[string]bool{
		"remote.origin.pushurl":                 true,
		"credential.https://github.com.helpers": true,
		"credential.https://gitlab.com.helper":  true,
		"user.name":                             true,
		"url.https://github.com/.insteadOf":     true,
		"core.a-b":                              true,
		"pushurl":                               false,
		"bad key.x":                             false,
		"remote..x":                             false,
		"a.1b":                                  false,
		"a.":                                    false,
		".a":                                    false,
		"a.b_c":                                 false,
		"remote.ori\ngin.url":                   false,
		"user.na\nme":                           false,
	}
	for k, ok := range cases {
		err := CheckGitConfig([][2]string{{"user.name", "x"}, {k, "v"}})
		if ok && err != nil {
			t.Errorf("%q: %v", k, err)
		}
		if !ok && (err == nil || err.Error() != "git_config: "+k+" is not a git config key") {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", " "} {
		if err := CheckGitConfig([][2]string{{k, "v"}}); err == nil || err.Error() != "git_config: empty key" {
			t.Errorf("%q: %v", k, err)
		}
	}
}

func TestCheckGitConfigRefusesHostKeys(t *testing.T) {
	for _, k := range []string{
		"credential.helper",
		"Credential.Helper",
		"CREDENTIAL.HELPER",
		"credential.https://github.com.helper",
		"Credential.https://github.com.Helper",
		"credential.https://github.com.HELPER",
	} {
		err := CheckGitConfig([][2]string{{"user.name", "x"}, {k, "v"}})
		if err == nil || err.Error() != "git_config: "+k+" is set by aeolian" {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{
		"remote.origin.pushurl",
		"credential.https://GitHub.com.helper",
		"credential.https://github.com.username",
		"url.https://github.com/.insteadOf",
		"url.https://x-access-token@github.com/.insteadof",
	} {
		if err := CheckGitConfig([][2]string{{k, "v"}}); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	if err := CheckGitConfig([][2]string{{"credential", "v"}}); err == nil || err.Error() != "git_config: credential is not a git config key" {
		t.Errorf("shape not checked first: %v", err)
	}
}

func TestHostGitConfigCoversWhatTheHostSets(t *testing.T) {
	for name, s := range map[string]Setup{
		"headless granted":    {HostURL: "http://h", Headless: true, GHToken: "t", Bot: Committer{Name: "b", Email: "b@x"}},
		"headless":            {HostURL: "http://h", Headless: true},
		"interactive granted": {HostURL: "http://h", GHToken: "t"},
	} {
		l := Layout{Home: t.TempDir(), ClaudeDir: "/acct"}
		if err := l.Prepare("a1", l.ScratchDir("a1"), s); err != nil {
			t.Fatal(err)
		}
		settings, _ := readSettings(t, l, "a1")
		env, _ := settings["env"].(map[string]any)
		n := 0
		for k, v := range env {
			if !strings.HasPrefix(k, "GIT_CONFIG_KEY_") {
				continue
			}
			n++
			key, _ := v.(string)
			if err := CheckGitConfig([][2]string{{key, "v"}}); err == nil || err.Error() != "git_config: "+key+" is set by aeolian" {
				t.Errorf("%s: %s is written by the host but not refused: %v", name, key, err)
			}
		}
		if n == 0 {
			t.Errorf("%s: no git config written", name)
		}
	}
}

func TestGitConfigParameterKeys(t *testing.T) {
	got := gitConfigParameterKeys(`'credential.helper'='' 'url.https://github.com/.insteadOf'='git@github.com:' 'core.x=y' 'a.b'`)
	want := []string{"credential.helper", "url.https://github.com/.insteadOf", "core.x", "a.b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPrepareRunEnvAndGitConfig(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.json")
	os.WriteFile(user, []byte(`{"env":{"AYA_ITEM":"user","KEEP":"k"}}`), 0o644)
	l := Layout{Home: dir, UserSettings: user}
	run := Setup{
		HostURL:   "http://h",
		Env:       map[string]string{"AYA_ITEM": "x"},
		GitConfig: [][2]string{{"remote.origin.pushurl", "/nonexistent"}, {"url.https://github.com/.insteadOf", "git@github.com:"}},
	}
	cases := map[string]struct {
		setup Setup
		host  [][2]string
	}{
		"headless": {Setup{Headless: true, GHToken: "t", Boot: BootLean}, [][2]string{
			{"credential.helper", ""},
			{"credential.https://github.com.helper", ""},
			{"credential.https://github.com.helper", BotHelper},
		}},
		"headless ungranted": {Setup{Headless: true}, [][2]string{
			{"credential.helper", ""},
			{"credential.https://github.com.helper", ""},
		}},
		"interactive granted": {Setup{GHToken: "t", Boot: BootLean}, [][2]string{
			{"credential.https://github.com.helper", ""},
			{"credential.https://github.com.helper", BotHelper},
		}},
		"interactive": {Setup{}, nil},
	}
	for name, c := range cases {
		s := c.setup
		s.HostURL, s.Env, s.GitConfig = run.HostURL, run.Env, run.GitConfig
		settings := settingsOf(t, l, s)
		env, _ := settings["env"].(map[string]any)
		if env["AYA_ITEM"] != "x" {
			t.Errorf("%s: AYA_ITEM %v", name, env["AYA_ITEM"])
		}
		if s.Boot == BootLean && env["KEEP"] != "k" {
			t.Errorf("%s: carried env lost: %v", name, env)
		}
		want := append(slices.Clone(c.host), run.GitConfig...)
		if env["GIT_CONFIG_COUNT"] != strconv.Itoa(len(want)) {
			t.Errorf("%s: count %v, want %d", name, env["GIT_CONFIG_COUNT"], len(want))
		}
		for i, e := range want {
			if env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] != e[0] || env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] != e[1] {
				t.Errorf("%s: entry %d is %v=%v, want %v", name, i, env["GIT_CONFIG_KEY_"+strconv.Itoa(i)], env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)], e)
			}
		}
	}
}

func TestRunGitConfigReachesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	l := Layout{Home: t.TempDir()}
	settings := settingsOf(t, l, Setup{
		Headless:  true,
		GHToken:   "t",
		Env:       map[string]string{"AYA_ITEM": "x"},
		GitConfig: [][2]string{{"remote.origin.pushurl", "/nonexistent"}},
	})
	env, _ := settings["env"].(map[string]any)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v.(string))
		}
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}
	if got := git("config", "remote.origin.pushurl"); got != "/nonexistent" {
		t.Errorf("pushurl %q", got)
	}
	if got := git("config", "--get-all", "credential.https://github.com.helper"); got != BotHelper {
		t.Errorf("credential helpers %q", got)
	}
	if got := git("config", "--get-all", "credential.helper"); got != "" {
		t.Errorf("credential.helper %q", got)
	}
}
