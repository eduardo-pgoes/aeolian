package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func settingsOf(t *testing.T, l Layout, s Setup) map[string]any {
	t.Helper()
	if err := l.Prepare("a1", l.ScratchDir("a1"), s); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(l.Settings("a1"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArgvBootProfiles(t *testing.T) {
	l := Layout{Home: "/h"}
	full := l.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p"})
	for _, f := range LeanArgs {
		if slices.Contains(full, f) {
			t.Errorf("full argv has %q: %q", f, full)
		}
	}
	if !slices.Equal(full, l.Argv(Launch{AgentID: "a", SessionID: "s", Prompt: "p", Boot: BootFull})) {
		t.Error("empty boot and full differ")
	}
	for _, la := range []Launch{
		{AgentID: "a", SessionID: "s", Prompt: "p", Boot: BootLean},
		{AgentID: "a", SessionID: "s", Prompt: "p", Boot: BootLean, Headless: true, Model: "sonnet"},
		{AgentID: "a", SessionID: "s", Resume: true, Boot: BootLean},
	} {
		argv := l.Argv(la)
		i := slices.Index(argv, "--setting-sources")
		if i < 0 || argv[i+1] != "project,local" || !slices.Contains(argv, "--disable-slash-commands") || !slices.Contains(argv, "--exclude-dynamic-system-prompt-sections") {
			t.Errorf("lean argv %+v: %q", la, argv)
		}
		c := slices.Index(argv, "claude")
		switch {
		case la.Resume:
			if argv[c+1] != "--resume" {
				t.Errorf("resume moved: %q", argv)
			}
		case la.Headless:
			if argv[c+1] != "-p" || argv[c+2] != PromptArg {
				t.Errorf("headless prompt moved: %q", argv)
			}
		default:
			if argv[c+1] != PromptArg {
				t.Errorf("prompt moved: %q", argv)
			}
		}
		if argv[len(argv)-2] != "--disallowedTools" {
			t.Errorf("--disallowedTools must stay last: %q", argv)
		}
	}
}

func TestPrepareLeanSettings(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user-settings.json")
	os.WriteFile(user, []byte(`{"outputStyle":"gentle","modelSettings":{"m":{"effortLevel":"medium"}},"enabledPlugins":{"x@y":true},
		"statusLine":{"type":"command"},"env":{"FOO":"bar","GH_TOKEN":"user"},"apiKeyHelper":"/bin/key","hooks":{"Stop":[]}}`), 0o644)
	l := Layout{Home: dir, UserSettings: user}

	full := settingsOf(t, l, Setup{HostURL: "http://h", GHToken: "tok"})
	for _, k := range []string{"outputStyle", "modelSettings", "skipDangerousModePermissionPrompt", "autoMemoryEnabled", "permissions"} {
		if _, ok := full[k]; ok {
			t.Errorf("full settings have %s: %v", k, full)
		}
	}

	lean := settingsOf(t, l, Setup{HostURL: "http://h", GHToken: "tok", Boot: BootLean})
	if lean["outputStyle"] != "default" || lean["autoMemoryEnabled"] != false || lean["skipDangerousModePermissionPrompt"] != true {
		t.Fatalf("lean keys: %v", lean)
	}
	if lean["apiKeyHelper"] != "/bin/key" || lean["modelSettings"] == nil {
		t.Fatalf("lean dropped carried keys: %v", lean)
	}
	for _, k := range []string{"enabledPlugins", "statusLine"} {
		if _, ok := lean[k]; ok {
			t.Errorf("lean carried %s", k)
		}
	}
	env := lean["env"].(map[string]any)
	if env["FOO"] != "bar" || env["GH_TOKEN"] != "tok" {
		t.Fatalf("env: user keys carried, host's must win: %v", env)
	}
	hooks, ok := lean["hooks"].(map[string]any)
	if !ok || len(hooks) != len(HookEvents) {
		t.Fatalf("lean hooks are the host's: %v", lean["hooks"])
	}
	if _, ok := readMCP(t, l, Setup{HostURL: "http://h", Boot: BootLean})["aeolian"]; !ok {
		t.Fatal("lean lost the aeolian MCP server")
	}

	if s := settingsOf(t, Layout{Home: dir, UserSettings: filepath.Join(dir, "none.json")}, Setup{Boot: BootLean}); s["outputStyle"] != "default" {
		t.Fatalf("missing user settings: %v", s)
	}
	os.WriteFile(user, []byte(`{`), 0o644)
	if err := l.Prepare("a1", l.ScratchDir("a1"), Setup{Boot: BootLean}); err == nil {
		t.Fatal("broken user settings passed")
	}
}

func TestPrepareReadRoots(t *testing.T) {
	l := Layout{Home: t.TempDir()}
	if s := settingsOf(t, l, Setup{}); s["permissions"] != nil {
		t.Fatalf("unconfined agent has permissions: %v", s)
	}
	s := settingsOf(t, l, Setup{ReadRoots: []string{"/s", "/w", "", "/s"}})
	p := s["permissions"].(map[string]any)
	if p["blockReadsOutsideWorkingDirectories"] != true {
		t.Fatalf("no read block: %v", p)
	}
	var dirs, deny []string
	for _, d := range p["additionalDirectories"].([]any) {
		dirs = append(dirs, d.(string))
	}
	for _, d := range p["deny"].([]any) {
		deny = append(deny, d.(string))
	}
	if !slices.Equal(dirs, []string{"/s", "/w"}) {
		t.Fatalf("roots: %q", dirs)
	}
	if !slices.Equal(deny, ReadDeny(l.Home)) || deny[0] != "Read(/"+l.Home+"/agents/**)" {
		t.Fatalf("deny: %q", deny)
	}
}

func TestCheckBoot(t *testing.T) {
	for _, ok := range []string{"", BootFull, BootLean} {
		if CheckBoot(ok) != nil {
			t.Errorf("%q refused", ok)
		}
	}
	if CheckBoot("bare") == nil {
		t.Error("bare passed")
	}
}
