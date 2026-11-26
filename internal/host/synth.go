package host

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// transcriptTail is how much of the end of a transcript is read for the
// last message: transcripts grow to megabytes, and only the end matters.
const transcriptTail = 4 << 20

// synthTextLimit caps the last message a synthesized handoff carries.
const synthTextLimit = 16 << 10

// transcript is the path of a's Claude Code transcript, or "" when there is
// none: Transcripts is unset or no project folder holds the session.
func (h *Host) transcript(a store.Agent) string {
	dir := h.transcriptsOf(a)
	if dir == "" || a.SessionID == "" {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", a.SessionID+".jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	return matches[0]
}

// synthesizeHandoff is the handoff the host writes for a, which ended
// without done: why, then a's last message from its transcript, if any.
// It never fails: a transcript that cannot be read leaves the message out.
func (h *Host) synthesizeHandoff(a store.Agent, why string) store.Handoff {
	text := lastAssistantText(h.transcript(a))
	if text == "" {
		return store.Handoff{Text: "(" + why + " and left no message)", Worktrees: []store.Worktree{}, Synthesized: true}
	}
	return store.Handoff{Text: "(" + why + "; this is its last message)\n\n" + text, Worktrees: []store.Worktree{}, Synthesized: true}
}

// lastAssistantText returns the text of the last assistant message in the
// transcript at path, or "" when there is none. The transcript is JSON
// lines; an assistant line's message content is a string or a list of
// blocks, of which only text blocks count.
func lastAssistantText(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("transcript %s: %v", path, err)
		}
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > transcriptTail {
		if _, err := f.Seek(fi.Size()-transcriptTail, io.SeekStart); err != nil {
			return ""
		}
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), transcriptTail)
	var last string
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil || entry.Type != "assistant" {
			continue
		}
		if t := contentText(entry.Message.Content); t != "" {
			last = t
		}
	}
	if len(last) > synthTextLimit {
		last = "…" + last[len(last)-synthTextLimit:]
	}
	return last
}

func contentText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, strings.TrimSpace(b.Text))
		}
	}
	return strings.Join(parts, "\n\n")
}
