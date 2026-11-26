// Package usage reads token use and the last reply out of Claude Code
// transcripts. The transcript format is not a public contract, so every
// reader fails soft: a line it cannot parse is skipped.
package usage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

// Tokens is one model's token use.
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	// CacheWrite1h is the part of CacheWrite written to the one-hour
	// cache, which costs more than the five-minute one.
	CacheWrite1h int64 `json:"cache_write_1h,omitempty"`
}

func (t Tokens) IsZero() bool { return t == Tokens{} }

func (t Tokens) Level() int64 { return t.Input + t.CacheRead + t.CacheWrite }

func (t Tokens) sub(u Tokens) Tokens {
	return Tokens{t.Input - u.Input, t.Output - u.Output, t.CacheRead - u.CacheRead, t.CacheWrite - u.CacheWrite, t.CacheWrite1h - u.CacheWrite1h}
}

func (t Tokens) add(u Tokens) Tokens {
	return Tokens{t.Input + u.Input, t.Output + u.Output, t.CacheRead + u.CacheRead, t.CacheWrite + u.CacheWrite, t.CacheWrite1h + u.CacheWrite1h}
}

// Delta is what a transcript gained since the last Tail, for one model.
// Turns counts the API requests seen for the first time.
type Delta struct {
	Model string
	Tokens
	Turns int
}

// Tailer reads one transcript forward from Offset. Claude Code writes one
// API response as several lines, one per content block, each repeating the
// response's usage; the Tailer counts each requestId once, by its last
// line, so the sum is right however the lines fall across Tail calls.
type Tailer struct {
	Path   string
	Offset int64
	Level  int64
	seen   map[string]counted
}

type counted struct {
	model string
	Tokens
}

type line struct {
	Type      string `json:"type"`
	Sidechain bool   `json:"isSidechain"`
	RequestID string `json:"requestId"`
	UUID      string `json:"uuid"`
	Message   struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			Input      int64 `json:"input_tokens"`
			Output     int64 `json:"output_tokens"`
			CacheRead  int64 `json:"cache_read_input_tokens"`
			CacheWrite int64 `json:"cache_creation_input_tokens"`
			Cache      struct {
				OneHour int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// Tail reads the complete lines written since Offset, moves Offset past
// them, and returns the token use they add, one Delta per model, in the
// order the models first appear. A transcript shorter than Offset was
// rewritten, so it is read again from the start.
func (t *Tailer) Tail() ([]Delta, error) {
	f, err := os.Open(t.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() < t.Offset {
		t.Offset, t.seen = 0, nil
	}
	if _, err := f.Seek(t.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	if t.seen == nil {
		t.seen = map[string]counted{}
	}
	r := bufio.NewReaderSize(f, 64<<10)
	last := map[string]counted{}
	var order []string
	for {
		b, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a partial last line is read next time
		}
		if err != nil {
			return nil, err
		}
		t.Offset += int64(len(b))
		if !bytes.Contains(b, []byte(`"assistant"`)) {
			continue
		}
		var l line
		if json.Unmarshal(b, &l) != nil || l.Type != "assistant" || l.Message.Usage == nil {
			continue
		}
		key := l.RequestID
		if key == "" {
			key = l.Message.ID
		}
		if key == "" {
			key = l.UUID
		}
		u := l.Message.Usage
		c := counted{model: l.Message.Model, Tokens: Tokens{u.Input, u.Output, u.CacheRead, u.CacheWrite, u.Cache.OneHour}}
		if lv := c.Level(); lv > 0 && !l.Sidechain {
			t.Level = lv
		}
		if _, ok := last[key]; !ok {
			order = append(order, key)
		}
		last[key] = c
	}
	var deltas []Delta
	byModel := map[string]int{}
	for _, key := range order {
		c := last[key]
		prev, seen := t.seen[key]
		t.seen[key] = c
		d := c.Tokens.sub(prev.Tokens)
		if d.IsZero() && seen {
			continue
		}
		i, ok := byModel[c.model]
		if !ok {
			i = len(deltas)
			byModel[c.model] = i
			deltas = append(deltas, Delta{Model: c.model})
		}
		deltas[i].Tokens = deltas[i].Tokens.add(d)
		if !seen {
			deltas[i].Turns++
		}
	}
	out := deltas[:0]
	for _, d := range deltas {
		if !d.IsZero() || d.Turns > 0 {
			out = append(out, d)
		}
	}
	return out, nil
}

// LastAssistantText returns the text of the last assistant message in the
// transcript at path that has any, with its text blocks joined; empty when
// there is none.
func LastAssistantText(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var text []string
	var msgID string
	for {
		b, err := r.ReadBytes('\n')
		if len(b) > 0 && bytes.Contains(b, []byte(`"assistant"`)) {
			var l line
			if json.Unmarshal(b, &l) == nil && l.Type == "assistant" {
				if t := textOf(l.Message.Content); t != "" {
					if l.Message.ID == "" || l.Message.ID != msgID {
						text = nil
					}
					msgID = l.Message.ID
					text = append(text, t)
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return strings.Join(text, "\n\n"), nil
}

func textOf(content json.RawMessage) string {
	var s string
	if json.Unmarshal(content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
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
