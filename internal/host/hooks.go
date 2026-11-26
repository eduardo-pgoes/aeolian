package host

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/usage"
)

// maxHookBody is the most of a hook's input the host reads; the rest is
// discarded unread.
const maxHookBody = 1 << 20

// typedWindow is how long a message the host typed into a terminal can
// still be matched to the UserPromptSubmit it causes.
const typedWindow = 60 * time.Second

// hookState is what the host remembers between hooks. It has its own lock
// and does no store I/O under it, so it never waits on h.mu or SQLite.
type hookState struct {
	mu     sync.Mutex
	typed  map[string][]typedText // agent id -> what the host typed into it lately
	tails  map[string]*agentTail  // agent id -> its transcript
	levels map[string]int64

	reopened map[string]bool
}

type typedText struct {
	sum string
	at  time.Time
}

// agentTail is one agent's transcript and how far its usage has been
// read. Its lock is held for a whole tail, so two tails of one transcript
// never count the same lines twice; hooks only touch want, so they never
// wait on a tail.
type agentTail struct {
	want atomic.Pointer[string] // the transcript the latest hook named

	mu      sync.Mutex
	path    string
	tailer  *usage.Tailer // nil until the first tail, which resumes it
	resumed bool
}

func promptSum(text string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	s := sha256.Sum256([]byte(text))
	return hex.EncodeToString(s[:])
}

// noteTyped remembers that the host is typing text into agentID, so the
// UserPromptSubmit it causes is not taken for a person's.
func (s *hookState) noteTyped(agentID, sum string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.typed == nil {
		s.typed = map[string][]typedText{}
	}
	s.typed[agentID] = append(fresh(s.typed[agentID], now), typedText{sum, now})
}

// takeTyped reports whether the host typed text with sum into agentID
// within typedWindow, and forgets that one if so.
func (s *hookState) takeTyped(agentID, sum string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := fresh(s.typed[agentID], now)
	found := false
	for i, t := range list {
		if t.sum == sum {
			list = append(list[:i:i], list[i+1:]...)
			found = true
			break
		}
	}
	if len(list) == 0 {
		delete(s.typed, agentID) // a no-op on a nil map
	} else {
		s.typed[agentID] = list
	}
	return found
}

func fresh(list []typedText, now time.Time) []typedText {
	out := list[:0]
	for _, t := range list {
		if now.Sub(t.at) <= typedWindow {
			out = append(out, t)
		}
	}
	return out
}

// tail returns agentID's transcript state, creating it; a non-empty path
// learned from a hook becomes the one the next tail reads.
func (s *hookState) tail(agentID, path string) *agentTail {
	s.mu.Lock()
	if s.tails == nil {
		s.tails = map[string]*agentTail{}
	}
	t := s.tails[agentID]
	if t == nil {
		t = &agentTail{}
		s.tails[agentID] = t
	}
	s.mu.Unlock()
	if path != "" {
		t.want.Store(&path)
	}
	return t
}

// hookInput is the part of a hook's input the host reads.
type hookInput struct {
	TranscriptPath string          `json:"transcript_path"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

func (h *Host) recordHook(agentID, event string, body []byte) error {
	now := time.Now()
	var in hookInput
	json.Unmarshal(body, &in) // input that is not JSON is stored as it came
	var evs []store.Event
	switch event {
	case "PostToolUse":
		evs = append(evs, dataEvent(store.KindToolUsed, store.ToolUsed{
			V: store.EventVersion, Tool: in.ToolName, Input: toolSummary(in.ToolInput), OK: toolOK(in.ToolResponse),
		}))
	case "UserPromptSubmit":
		by := h.promptBy(agentID, in.Prompt, now)
		evs = append(evs, dataEvent(store.KindHook, store.HookData{
			V: store.EventVersion, Event: event, By: by, Payload: store.Truncate(body, store.HookPayloadLimit),
		}))
		if by == store.ByHuman {
			evs = append(evs, dataEvent(store.KindHumanMessage, store.HumanMessage{
				V: store.EventVersion, Text: store.CutString(in.Prompt, store.HumanMessageLimit),
			}))
		}
	default:
		evs = append(evs, dataEvent(store.KindHook, store.HookData{
			V: store.EventVersion, Event: event, Payload: store.Truncate(body, store.HookPayloadLimit),
		}))
	}
	if err := h.Store.Hook(agentID, event, now, evs...); err != nil {
		return err
	}
	if in.TranscriptPath != "" {
		h.hooks.tail(agentID, in.TranscriptPath)
	}
	if event == "Stop" || event == "SessionEnd" || event == "PostToolUse" {
		go h.tailUsage(agentID)
	}
	return nil
}

// promptBy says who typed prompt into agentID: Claude Code itself, when
// it is a background task's notification; the host, when it is the
// agent's first prompt or a message the host typed into its terminal
// within typedWindow; and a person otherwise.
func (h *Host) promptBy(agentID, prompt string, now time.Time) string {
	if isTaskNotification(prompt) {
		return store.ByClaude
	}
	sum := promptSum(prompt)
	if h.hooks.takeTyped(agentID, sum, now) {
		return store.ByHost
	}
	if a, err := h.Store.Agent(agentID); err == nil && promptSum(a.Prompt) == sum {
		return store.ByHost
	}
	return store.ByHuman
}

// isTaskNotification reports whether prompt is the message Claude Code
// submits when a background task (a Bash run with run_in_background, or a
// background agent) finishes: a <task-notification> element.
func isTaskNotification(prompt string) bool {
	return strings.HasPrefix(strings.TrimSpace(prompt), "<task-notification>")
}

func dataEvent(kind string, data any) store.Event {
	b, _ := json.Marshal(data)
	return store.Event{Kind: kind, Data: b}
}

// toolSummary is a tool's input as compact JSON, cut to
// store.ToolInputLimit.
func toolSummary(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var b bytes.Buffer
	if json.Compact(&b, input) != nil {
		return store.CutString(string(input), store.ToolInputLimit)
	}
	return store.CutString(b.String(), store.ToolInputLimit)
}

// toolOK reads a tool's response as a success unless it says otherwise.
func toolOK(resp json.RawMessage) bool {
	var r struct {
		IsError *bool `json:"is_error"`
		Success *bool `json:"success"`
	}
	if json.Unmarshal(resp, &r) != nil {
		return true
	}
	if r.IsError != nil && *r.IsError {
		return false
	}
	return r.Success == nil || *r.Success
}

// tailUsage reads the token use agentID's transcript gained since the last
// read and appends a usage event per model. After a restart it resumes from
// the offset of the agent's last usage event. It fails soft: a transcript
// it cannot read is logged and skipped. It must not run under h.mu.
func (h *Host) tailUsage(agentID string) {
	t := h.hooks.tail(agentID, "")
	t.mu.Lock()
	defer t.mu.Unlock()
	if p := t.want.Load(); p != nil && *p != t.path {
		t.path, t.tailer, t.resumed = *p, nil, false
	}
	if t.path == "" {
		return
	}
	a, err := h.Store.Agent(agentID)
	if err != nil {
		log.Printf("usage %s: %v", agentID, err)
		return
	}
	if !t.resumed {
		t.tailer = &usage.Tailer{Path: t.path, Offset: h.lastUsageOffset(agentID, t.path), Level: h.contextLevels([]string{agentID})[agentID]}
		t.resumed = true
	}
	deltas, err := t.tailer.Tail()
	if err != nil {
		log.Printf("usage %s: %v", agentID, err)
		return
	}
	level := t.tailer.Level
	h.hooks.setLevel(agentID, level)
	for _, d := range deltas {
		h.emit(a.RunID, agentID, store.KindUsage, store.Usage{
			V: store.EventVersion, Model: d.Model, Input: d.Input, Output: d.Output, CacheRead: d.CacheRead,
			CacheWrite: d.CacheWrite, CacheWrite1h: d.CacheWrite1h, Turns: d.Turns, Path: t.path, Offset: t.tailer.Offset,
			Context: level,
		})
	}
	h.enforceCap(agentID, level)
	if len(deltas) > 0 {
		h.enforceTokenCap(a.RunID)
	}
}

func (s *hookState) setLevel(agentID string, level int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levels == nil {
		s.levels = map[string]int64{}
	}
	s.levels[agentID] = level
}

func (s *hookState) knownLevels(agentIDs []string) (map[string]int64, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	known := make(map[string]int64, len(agentIDs))
	var missing []string
	for _, id := range agentIDs {
		if lv, ok := s.levels[id]; ok {
			known[id] = lv
		} else {
			missing = append(missing, id)
		}
	}
	return known, missing
}

func (s *hookState) seedLevels(seed map[string]int64, agentIDs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.levels == nil {
		s.levels = map[string]int64{}
	}
	for _, id := range agentIDs {
		if _, ok := s.levels[id]; !ok {
			s.levels[id] = seed[id]
		}
	}
}

func (h *Host) contextLevels(agentIDs []string) map[string]int64 {
	known, missing := h.hooks.knownLevels(agentIDs)
	if len(missing) == 0 {
		return known
	}
	last, err := h.Store.LastEvents(store.KindUsage, missing)
	if err != nil {
		log.Printf("context levels: %v", err)
		return known
	}
	seed := map[string]int64{}
	for id, e := range last {
		var u store.Usage
		if json.Unmarshal(e.Data, &u) == nil {
			seed[id] = u.Context
		}
	}
	h.hooks.seedLevels(seed, missing)
	known, _ = h.hooks.knownLevels(agentIDs)
	return known
}

// lastUsageOffset is how far into path the agent's usage events have read.
func (h *Host) lastUsageOffset(agentID, path string) int64 {
	var offset, after int64
	for {
		evs, err := h.Store.Events(store.EventQuery{After: after, AgentID: agentID, Kinds: []string{store.KindUsage}})
		if err != nil {
			log.Printf("usage %s: %v", agentID, err)
			return offset
		}
		for _, e := range evs {
			var u store.Usage
			if json.Unmarshal(e.Data, &u) == nil && u.Path == path {
				offset = u.Offset
			}
			after = e.ID
		}
		if len(evs) < store.DefaultEventLimit {
			return offset
		}
	}
}
