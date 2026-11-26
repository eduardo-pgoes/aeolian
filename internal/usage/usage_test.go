package usage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assistant(req, msg, model, content string, in, out, read, write, write1h int64) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"uuid":"u-%s","message":{"id":%q,"model":%q,"content":%s,`+
		`"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"cache_creation":{"ephemeral_1h_input_tokens":%d}}}}`+"\n",
		req, req, msg, model, content, in, out, read, write, write1h)
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestTailDedupesByRequestID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"+
		assistant("r1", "m1", "claude-opus-5-5", `[{"type":"thinking","thinking":""}]`, 2, 10, 100, 50, 50)+
		assistant("r1", "m1", "claude-opus-5-5", `[{"type":"text","text":"first"}]`, 2, 30, 100, 50, 50)+
		`not json at all`+"\n"+
		assistant("r2", "m2", "claude-opus-5-5", `[{"type":"tool_use","name":"Bash"}]`, 1, 5, 200, 0, 0))
	tl := &Tailer{Path: path}
	d, err := tl.Tail()
	if err != nil {
		t.Fatal(err)
	}
	want := Delta{Model: "claude-opus-5-5", Tokens: Tokens{3, 35, 300, 50, 50}, Turns: 2}
	if len(d) != 1 || d[0] != want {
		t.Fatalf("got %+v, want %+v", d, want)
	}
	// r2's response goes on in a later line, a new request starts, and the
	// last line is still being written.
	appendTo(t, path, assistant("r2", "m2", "claude-opus-5-5", `[{"type":"text","text":"second"}]`, 1, 9, 200, 0, 0)+
		assistant("r3", "m3", "claude-haiku-5-5", `[{"type":"text","text":"third"}]`, 4, 4, 0, 0, 0)+
		`{"type":"assistant","requestId":"r4"`)
	d, err = tl.Tail()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 || d[0] != (Delta{Model: "claude-opus-5-5", Tokens: Tokens{Output: 4}}) || d[1] != (Delta{Model: "claude-haiku-5-5", Tokens: Tokens{Input: 4, Output: 4}, Turns: 1}) {
		t.Fatalf("second tail: %+v", d)
	}
	fi, _ := os.Stat(path)
	if tl.Offset >= fi.Size() {
		t.Fatalf("offset %d read the partial line of %d", tl.Offset, fi.Size())
	}
	if d, err := tl.Tail(); err != nil || len(d) != 0 {
		t.Fatalf("nothing new: %+v %v", d, err)
	}
	// A restarted host resumes from the offset alone.
	again := &Tailer{Path: path, Offset: tl.Offset}
	appendTo(t, path, `,"x":1}`+"\n")
	if d, err := again.Tail(); err != nil || len(d) != 0 {
		t.Fatalf("after resume: %+v %v", d, err)
	}
}

func TestTailMissingFile(t *testing.T) {
	if _, err := (&Tailer{Path: filepath.Join(t.TempDir(), "none")}).Tail(); err == nil {
		t.Fatal("no error")
	}
}

func TestLastAssistantText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, path,
		assistant("r1", "m1", "x", `[{"type":"text","text":"old"}]`, 0, 0, 0, 0, 0)+
			assistant("r2", "m2", "x", `[{"type":"text","text":"Here is what I did."}]`, 0, 0, 0, 0, 0)+
			assistant("r2", "m2", "x", `[{"type":"tool_use","name":"Bash"}]`, 0, 0, 0, 0, 0)+
			assistant("r2", "m2", "x", `[{"type":"text","text":"And more."}]`, 0, 0, 0, 0, 0)+
			`{"type":"user","message":{"content":[{"type":"tool_result","content":"ok"}]}}`)
	got, err := LastAssistantText(path)
	if err != nil || got != "Here is what I did.\n\nAnd more." {
		t.Fatalf("%q %v", got, err)
	}
	empty := filepath.Join(t.TempDir(), "e.jsonl")
	appendTo(t, empty, `{"type":"user"}`+"\n")
	if got, err := LastAssistantText(empty); err != nil || got != "" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestCost(t *testing.T) {
	c, ok := Cost("claude-opus-5-5", Tokens{Input: 1e6, Output: 1e6, CacheRead: 1e6, CacheWrite: 2e6, CacheWrite1h: 1e6})
	if want := 4 + 20 + 0.20 + 5 + 8.0; !ok || math.Abs(c-want) > 1e-9 {
		t.Fatalf("cost %v %v, want %v", c, ok, want)
	}
	if _, ok := PriceOf("claude-haiku-4-5-20251001"); !ok {
		t.Fatal("dated id")
	}
	if p, _ := PriceOf("claude-opus-5-5[1m]"); p.Input != 4 {
		t.Fatalf("suffixed id: %+v", p)
	}
	if _, ok := Cost("<synthetic>", Tokens{Input: 1}); ok {
		t.Fatal("priced an unknown model")
	}
	for id := range Prices {
		if !strings.HasPrefix(id, "claude-") {
			t.Fatal(id)
		}
	}
}

func TestTailLevel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendTo(t, path, `{"type":"user","message":{"role":"user","content":"hi"}}`+"\n"+
		assistant("r1", "m1", "claude-opus-5-5", `[{"type":"thinking","thinking":""}]`, 3, 10, 1000, 200, 0)+
		assistant("r1", "m1", "claude-opus-5-5", `[{"type":"tool_use","name":"Bash"}]`, 3, 900, 1000, 200, 0)+
		assistant("r2", "m2", "claude-opus-5-5", `[{"type":"tool_use","name":"Read"}]`, 5, 50, 1200, 300, 0))
	tl := &Tailer{Path: path}
	if _, err := tl.Tail(); err != nil {
		t.Fatal(err)
	}
	if tl.Level != 5+1200+300 {
		t.Fatalf("level after two requests: %d", tl.Level)
	}
	if _, err := tl.Tail(); err != nil || tl.Level != 1505 {
		t.Fatalf("level after an empty tail: %d %v", tl.Level, err)
	}
	appendTo(t, path, `{"type":"assistant","isSidechain":true,"requestId":"s1","message":{"model":"claude-haiku-5-5","usage":{"input_tokens":90000}}}`+"\n"+
		assistant("r3", "m3", "<synthetic>", `[{"type":"text","text":"API Error"}]`, 0, 0, 0, 0, 0))
	if _, err := tl.Tail(); err != nil || tl.Level != 1505 {
		t.Fatalf("a sidechain or synthetic line moved the level: %d %v", tl.Level, err)
	}
	appendTo(t, path, `{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"summary"}}`+"\n"+
		assistant("r4", "m4", "claude-opus-5-5", `[{"type":"text","text":"back"}]`, 2, 4000, 0, 400, 0))
	if _, err := tl.Tail(); err != nil || tl.Level != 402 {
		t.Fatalf("level after a compaction: %d %v", tl.Level, err)
	}
	resumed := &Tailer{Path: path, Offset: tl.Offset, Level: 777}
	if _, err := resumed.Tail(); err != nil || resumed.Level != 777 {
		t.Fatalf("a resumed tailer lost its seed: %d %v", resumed.Level, err)
	}
}
