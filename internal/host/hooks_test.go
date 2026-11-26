package host

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

func (f *fixture) hook(t *testing.T, agentID, event string, body any) {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(f.srv.URL+"/hooks/"+agentID+"/"+event, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("%s hook: %s", event, resp.Status)
	}
}

func (f *fixture) events(t *testing.T, agentID string, kinds ...string) []store.Event {
	t.Helper()
	evs, err := f.store.Events(store.EventQuery{AgentID: agentID, Kinds: kinds})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func decode[T any](t *testing.T, e store.Event) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(e.Data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPostToolUseBecomesToolUsed(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.hook(t, a.ID, "PostToolUse", map[string]any{
		"hook_event_name": "PostToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": strings.Repeat("x", 4000)},
		"tool_response":   map[string]any{"stdout": strings.Repeat("y", 100000), "is_error": true},
	})
	evs := f.events(t, a.ID, store.KindToolUsed, store.KindHook)
	if len(evs) != 1 || evs[0].Kind != store.KindToolUsed {
		t.Fatalf("events: %+v", evs)
	}
	u := decode[store.ToolUsed](t, evs[0])
	if u.Tool != "Bash" || u.OK || len(u.Input) > store.ToolInputLimit || !strings.Contains(u.Input, `"command"`) {
		t.Fatalf("tool.used: %+v (input %d bytes)", u, len(u.Input))
	}
	if len(evs[0].Data) > 2*store.ToolInputLimit {
		t.Fatalf("tool.used kept %d bytes: the response must not be stored", len(evs[0].Data))
	}
	if got := f.agent(t, a.ID); got.LastHook != "PostToolUse" {
		t.Fatalf("last hook %q", got.LastHook)
	}
}

func TestHookPayloadIsTruncated(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	f.hook(t, a.ID, "Notification", map[string]any{"hook_event_name": "Notification", "message": strings.Repeat("z", 2<<20)})
	evs := f.events(t, a.ID, store.KindHook)
	last := evs[len(evs)-1]
	h := decode[store.HookData](t, last)
	if h.Event != "Notification" || len(h.Payload) > store.HookPayloadLimit || !strings.Contains(string(h.Payload), "bytes]") {
		t.Fatalf("hook kept %d bytes: %.200s", len(h.Payload), h.Payload)
	}
}

func TestUserPromptSubmitSaysWhoTyped(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	prompt := func(text string) {
		f.hook(t, a.ID, "UserPromptSubmit", map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": text})
	}
	by := func() string {
		evs := f.events(t, a.ID, store.KindHook)
		return decode[store.HookData](t, evs[len(evs)-1]).By
	}

	prompt(a.Prompt) // the first prompt is the host's
	if got := by(); got != store.ByHost {
		t.Fatalf("first prompt by %q", got)
	}

	go f.host.tell(a.ID, "the helper finished\n")
	<-f.keeper.typed
	waitEvent(t, f, a.ID, store.KindHandoffTyped)
	prompt("the helper finished")
	if got := by(); got != store.ByHost {
		t.Fatalf("typed prompt by %q", got)
	}
	// The same text again was not typed by the host this time.
	prompt("the helper finished")
	prompt("no, use the other branch")
	if got := by(); got != store.ByHuman {
		t.Fatalf("person's prompt by %q", got)
	}
	evs := f.events(t, a.ID, store.KindHumanMessage)
	if len(evs) != 2 {
		t.Fatalf("human messages: %d", len(evs))
	}
	if m := decode[store.HumanMessage](t, evs[1]); m.Text != "no, use the other branch" {
		t.Fatalf("human message %q", m.Text)
	}

	// Claude Code's own notice that a background task finished is no
	// person's message.
	prompt("<task-notification>\n<task-id>b1x9</task-id>\n<status>completed</status>\n<summary>Background command \"sleep 40; wc -l cap.go\" completed (exit code 0)</summary>\n</task-notification>")
	if got := by(); got != store.ByClaude {
		t.Fatalf("task notification by %q", got)
	}
	if evs := f.events(t, a.ID, store.KindHumanMessage); len(evs) != 2 {
		t.Fatalf("a task notification became a human message: %d", len(evs))
	}
}

func waitEvent(t *testing.T, f *fixture, agentID, kind string) []store.Event {
	t.Helper()
	for range 200 {
		if evs := f.events(t, agentID, kind); len(evs) > 0 {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no %s event for %s", kind, agentID)
	return nil
}

func assistantLine(req, model string, in, out int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"message":{"model":%q,"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%d,"output_tokens":%d}}}`+"\n",
		req, model, in, out)
}

func TestStopTailsTranscriptUsage(t *testing.T) {
	f := newFixture(t)
	a := f.start(t)
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// One response written as two lines repeating its usage counts once.
	lines := assistantLine("r1", "claude-opus-5-5", 10, 5) + assistantLine("r1", "claude-opus-5-5", 10, 7) + assistantLine("r2", "claude-opus-5-5", 3, 1)
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	f.hook(t, a.ID, "Stop", map[string]any{"hook_event_name": "Stop", "transcript_path": path})
	evs := waitEvent(t, f, a.ID, store.KindUsage)
	u := decode[store.Usage](t, evs[0])
	if len(evs) != 1 || u.Model != "claude-opus-5-5" || u.Input != 13 || u.Output != 8 || u.Turns != 2 || u.Offset != int64(len(lines)) {
		t.Fatalf("usage: %d events, %+v", len(evs), u)
	}

	// A restarted host resumes from the last usage event's offset.
	f.host.hooks = hookState{}
	more := assistantLine("r3", "claude-haiku-5-5", 2, 2)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString(more)
	fh.Close()
	f.hook(t, a.ID, "SessionEnd", map[string]any{"hook_event_name": "SessionEnd", "transcript_path": path, "reason": "prompt_input_exit"})
	for range 200 {
		if evs = f.events(t, a.ID, store.KindUsage); len(evs) > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(evs) != 2 {
		t.Fatalf("usage events after resume: %d", len(evs))
	}
	if u := decode[store.Usage](t, evs[1]); u.Model != "claude-haiku-5-5" || u.Input != 2 || u.Turns != 1 {
		t.Fatalf("resumed usage: %+v", u)
	}
}
