package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestBootCost runs the real claude once per boot profile, with the flags
// and settings Prepare and Argv give a headless agent, and logs the token
// usage of each run's first model call, which is the boot. The lean run is
// also read-confined, so the prompt has it try the reads confinement should
// stop. It costs two real calls, so it only runs with AEOLIAN_BOOT_COST=1:
//
//	AEOLIAN_BOOT_COST=1 go test ./internal/agent -run TestBootCost -v
func TestBootCost(t *testing.T) {
	if os.Getenv("AEOLIAN_BOOT_COST") != "1" {
		t.Skip("set AEOLIAN_BOOT_COST=1 to run the real claude twice")
	}
	us, err := UserSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	write := func(rel, body string) string {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	other := write("agents/other/settings.json", `{"env":{"GH_TOKEN":"SECRET-A"}}`)
	sibling := write("scratch/other/witness-input.md", "SECRET-B\n")
	prompt := "This is a boot test. Do exactly these four things, one tool call each, and do not retry or work around a refusal:\n" +
		"1. Read ./input.md.\n2. Read " + other + ".\n3. Grep for SECRET in " + filepath.Dir(sibling) + ".\n4. Run the Bash command: cat " + sibling + "\n" +
		"Then reply with one line per step: the step number, then ok and the text you saw, or blocked and the reason in five words."

	for _, boot := range []string{BootFull, BootLean} {
		l := Layout{Home: home, UserSettings: us}
		id := NewSessionID()
		write("scratch/"+id+"/input.md", "OWN-C\n")
		s := Setup{Boot: boot, Headless: true}
		if boot == BootLean {
			s.ReadRoots = []string{l.ScratchDir(id)}
		}
		if err := l.Prepare(id, l.ScratchDir(id), s); err != nil {
			t.Fatal(err)
		}
		argv := l.Argv(Launch{AgentID: id, SessionID: id, Prompt: prompt, Headless: true, Model: "sonnet", Boot: boot})
		argv = argv[slices.Index(argv, "claude"):]
		argv[slices.Index(argv, PromptArg)] = prompt
		argv = append(argv, "--output-format", "stream-json", "--verbose", "--no-session-persistence")
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = l.ScratchDir(id)
		cmd.Stdin = strings.NewReader("")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v\n%s", boot, err, out)
		}
		first, final := boot1(t, out)
		t.Logf("%s boot call: input %d, cache write %d, cache read %d, total %d", boot, first.Input, first.CacheWrite, first.CacheRead, first.Input+first.CacheWrite+first.CacheRead)
		t.Logf("%s run: cost $%.4f, %d turns, result:\n%s", boot, final.Cost, final.Turns, final.Result)
	}
}

type usage struct {
	Input      int `json:"input_tokens"`
	CacheWrite int `json:"cache_creation_input_tokens"`
	CacheRead  int `json:"cache_read_input_tokens"`
}

type result struct {
	Cost   float64 `json:"total_cost_usd"`
	Turns  int     `json:"num_turns"`
	Result string  `json:"result"`
}

func boot1(t *testing.T, out []byte) (first usage, final result) {
	seen := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Usage usage `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if !seen {
				first, seen = ev.Message.Usage, true
			}
		case "result":
			json.Unmarshal(sc.Bytes(), &final)
		}
	}
	if !seen {
		t.Fatalf("no assistant message in:\n%s", out)
	}
	return first, final
}
