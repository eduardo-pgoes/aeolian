package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// PartInput is one structured result beside a handoff's text.
type PartInput struct {
	Kind string          `json:"kind" jsonschema:"what the body is: artifact (a file you wrote) or claims (statements with their evidence)"`
	Body json.RawMessage `json:"body" jsonschema:"the part itself, shaped by its kind. artifact: {path, title?, media_type?}. claims: {claims: [{id?, text, kind?: fact|inference|judgment, evidence: [path:line, url or 'cmd: ...'], confidence?: low|medium|high}]}"`
}

// MaxParts caps the parts on one handoff.
const MaxParts = 20

// PartKind checks and records one kind of part. Adding a kind is one file:
// implement it and call RegisterPartKind from an init.
type PartKind interface {
	Name() string
	// Validate checks body for agent a handing back h, and returns the body
	// to store, which may be normalized (a relative path made absolute,
	// missing ids filled in). Its error goes back to the agent as the tool
	// error, so it should say what to fix.
	Validate(a store.Agent, h store.Handoff, body json.RawMessage) (json.RawMessage, error)
	// Summary is a short line about a stored body, as a parent sees it
	// beside a helper's handoff.
	Summary(body json.RawMessage) string
}

// PartRecorder is a PartKind that keeps more than the part row once the
// handoff is stored, such as the claims ledger. bodies are every part of
// its kind on the handoff, in order. An error is logged; the done stands.
type PartRecorder interface {
	Record(h *Host, a store.Agent, bodies []json.RawMessage) error
}

var (
	kindsMu sync.RWMutex
	kinds   = map[string]PartKind{}
)

// RegisterPartKind adds k to the kinds every host accepts unless it sets
// PartKinds. It panics on a second kind of the same name.
func RegisterPartKind(k PartKind) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if _, dup := kinds[k.Name()]; dup {
		panic("host: part kind " + k.Name() + " registered twice")
	}
	kinds[k.Name()] = k
}

func (h *Host) partKinds() map[string]PartKind {
	if h.PartKinds != nil {
		return h.PartKinds
	}
	kindsMu.RLock()
	defer kindsMu.RUnlock()
	return maps.Clone(kinds)
}

// DoneCall is one call of done as gates see it: who called, what it hands
// back (parts already validated) and the outcome.
type DoneCall struct {
	Agent   store.Agent
	Handoff store.Handoff
	Outcome string
}

// DoneGate is a rule on done that needs no YAML: register one with
// RegisterDoneGate, or set Host.DoneGates.
//
// Check runs on every done, helpers' too, after the parts validate and
// before anything is stored. Its error goes back to the agent as the tool
// error; its session stays open, so it can fix the problem and call done
// again.
//
// Hold runs after a step agent's done is stored, before its run moves on:
// the next step does not start, and the run does not finish (so on_finish
// does not run), until every Hold returns. ctx ends after Timeout, or
// DefaultHoldTimeout, or when the run is killed; a Hold should return then,
// recording whatever "not in time" means to it. The run moves on whatever
// a Hold returns; its error is logged and emitted. Helpers' dones are
// never held, since their parents wait on them.
type DoneGate struct {
	Name    string
	Check   func(h *Host, c DoneCall) error
	Hold    func(ctx context.Context, h *Host, c DoneCall) error
	Timeout time.Duration
}

// DefaultHoldTimeout is how long a gate may hold a run when it sets no
// Timeout.
const DefaultHoldTimeout = 20 * time.Minute

var (
	gatesMu sync.RWMutex
	gates   []DoneGate
)

// RegisterDoneGate adds g to every host's gates, before Host.DoneGates.
func RegisterDoneGate(g DoneGate) {
	gatesMu.Lock()
	defer gatesMu.Unlock()
	gates = append(gates, g)
}

func (h *Host) doneGates() []DoneGate {
	gatesMu.RLock()
	defer gatesMu.RUnlock()
	return append(slices.Clone(gates), h.DoneGates...)
}

// toParts checks every part of in against the known kinds and returns them
// normalized, ready to store. A body sent as a JSON string holding JSON is
// unwrapped first: models sometimes quote an object.
func (h *Host) toParts(a store.Agent, hand store.Handoff, in []PartInput) ([]store.Part, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > MaxParts {
		return nil, fmt.Errorf("%d parts; at most %d fit on one handoff", len(in), MaxParts)
	}
	known := h.partKinds()
	out := make([]store.Part, len(in))
	for i, p := range in {
		k, ok := known[p.Kind]
		if !ok {
			return nil, fmt.Errorf("part %d has kind %q; known kinds: %s", i+1, p.Kind, strings.Join(slices.Sorted(maps.Keys(known)), ", "))
		}
		body, err := k.Validate(a, hand, unquote(p.Body))
		if err != nil {
			return nil, fmt.Errorf("part %d (%s): %w", i+1, p.Kind, err)
		}
		out[i] = store.Part{Kind: p.Kind, Body: body}
	}
	return out, nil
}

func unquote(body json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(body, &s) != nil {
		return body
	}
	t := strings.TrimSpace(s)
	if (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		return json.RawMessage(t)
	}
	return body
}

// checkDone runs every gate's Check on c.
func (h *Host) checkDone(c DoneCall) error {
	for _, g := range h.doneGates() {
		if g.Check == nil {
			continue
		}
		if err := g.Check(h, c); err != nil {
			return err
		}
	}
	return nil
}

// recordParts runs once a handoff with parts is stored: it emits
// parts.recorded and lets each recording kind keep what it keeps.
func (h *Host) recordParts(a store.Agent, parts []store.Part) {
	if len(parts) == 0 {
		return
	}
	byKind := map[string][]json.RawMessage{}
	var order []string
	for _, p := range parts {
		if _, seen := byKind[p.Kind]; !seen {
			order = append(order, p.Kind)
		}
		byKind[p.Kind] = append(byKind[p.Kind], p.Body)
	}
	counts := map[string]int{}
	for k, bs := range byKind {
		counts[k] = len(bs)
	}
	h.emit(a.RunID, a.ID, store.KindPartsRecorded, store.PartsRecorded{V: store.EventVersion, Kinds: order, Counts: counts})
	known := h.partKinds()
	for _, kind := range order {
		r, ok := known[kind].(PartRecorder)
		if !ok {
			continue
		}
		if err := r.Record(h, a, byKind[kind]); err != nil {
			log.Printf("record %s parts of %s: %v", kind, a.ID, err)
			h.emit(a.RunID, a.ID, store.KindPartsFailed, store.PartsFailed{V: store.EventVersion, Kind: kind, Error: err.Error()})
		}
	}
}

// holdDone runs every gate's Hold on a step agent's stored done, all at
// once, and returns when they have all returned. It reports false when the
// run was killed meanwhile. A run being ended has its context cancelled too,
// so its gates end at once, but it still reports true: the step has to close
// for the end to finish.
func (h *Host) holdDone(c DoneCall) bool {
	var held []DoneGate
	for _, g := range h.doneGates() {
		if g.Hold != nil {
			held = append(held, g)
		}
	}
	if len(held) == 0 {
		return true
	}
	runCtx := h.runContext(c.Agent.RunID)
	var wg sync.WaitGroup
	for _, g := range held {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := g.Timeout
			if timeout <= 0 {
				timeout = DefaultHoldTimeout
			}
			ctx, cancel := context.WithTimeout(runCtx, timeout)
			defer cancel()
			ev := store.GateData{V: store.EventVersion, Gate: g.Name}
			h.emit(c.Agent.RunID, c.Agent.ID, store.KindGateHeld, ev)
			err := g.Hold(ctx, h, c)
			kind := store.KindGatePassed
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				kind = store.KindGateTimeout
			case runCtx.Err() != nil:
				kind = store.KindGateCancelled
			case err != nil:
				kind = store.KindGateFailed
			}
			if err != nil {
				ev.Error = err.Error()
				log.Printf("gate %s on %s: %v", g.Name, c.Agent.ID, err)
			}
			h.emit(c.Agent.RunID, c.Agent.ID, kind, ev)
		}()
	}
	wg.Wait()
	return runCtx.Err() == nil || h.endingOf(c.Agent.RunID) != nil
}

// partsSummary is the line a parent reads about a helper's parts, such as
// "Parts: claims (12), artifact /x/report.md".
func (h *Host) partsSummary(parts []store.Part) string {
	if len(parts) == 0 {
		return ""
	}
	known := h.partKinds()
	items := make([]string, len(parts))
	for i, p := range parts {
		items[i] = p.Kind
		if k, ok := known[p.Kind]; ok {
			if s := k.Summary(p.Body); s != "" {
				items[i] += " " + s
			}
		}
	}
	return "Parts: " + strings.Join(items, ", ")
}

// formatParts writes parts in full, for a helper handed them by spawn.
func formatParts(parts []store.Part) string {
	if len(parts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nParts:")
	for _, p := range parts {
		fmt.Fprintf(&b, "\n- %s: %s", p.Kind, p.Body)
	}
	return b.String()
}

// rawSchema lets a json.RawMessage take any JSON value. The SDK's own
// inference reads it as []byte, a byte array, and refuses an object.
var rawSchema = map[reflect.Type]*jsonschema.Schema{reflect.TypeFor[json.RawMessage](): {}}

// inputSchema is T's tool input schema with parts' bodies left open.
func inputSchema[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](&jsonschema.ForOptions{TypeSchemas: rawSchema})
	if err != nil {
		panic(err)
	}
	return s
}

// artifactKind is a file or folder the agent wrote, inside a folder it
// owns: its own, or a worktree its handoff lists.
type artifactKind struct{}

type artifactBody struct {
	Path      string `json:"path"`
	Title     string `json:"title,omitempty"`
	MediaType string `json:"media_type,omitempty"`
}

func (artifactKind) Name() string { return "artifact" }

func (artifactKind) Validate(a store.Agent, hand store.Handoff, body json.RawMessage) (json.RawMessage, error) {
	var b artifactBody
	var path string
	if json.Unmarshal(body, &path) == nil {
		b.Path = path
	} else if err := strictDecode(body, &b); err != nil {
		return nil, fmt.Errorf("body must be {path, title?, media_type?}: %v", err)
	}
	b.Path = strings.TrimSpace(b.Path)
	if b.Path == "" {
		return nil, errors.New("no path")
	}
	if !filepath.IsAbs(b.Path) {
		b.Path = filepath.Join(a.Dir, b.Path)
	}
	b.Path = filepath.Clean(b.Path)
	real, err := filepath.EvalSymlinks(b.Path)
	if err != nil {
		return nil, fmt.Errorf("%s does not exist: write the file first", b.Path)
	}
	roots := []string{a.Dir}
	for _, w := range hand.Worktrees {
		roots = append(roots, w.Path)
	}
	if !under(real, roots) {
		return nil, fmt.Errorf("%s is outside your folder and the worktrees your handoff lists (%s)", b.Path, strings.Join(roots, ", "))
	}
	return json.Marshal(b)
}

func (artifactKind) Summary(body json.RawMessage) string {
	var b artifactBody
	json.Unmarshal(body, &b)
	return b.Path
}

func under(path string, roots []string) bool {
	for _, r := range roots {
		if r == "" {
			continue
		}
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			r = rr
		}
		rel, err := filepath.Rel(r, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func strictDecode(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// Claim kinds and confidences a claims part may give.
var (
	ClaimKinds       = []string{"fact", "inference", "judgment"}
	ClaimConfidences = []string{"low", "medium", "high"}
)

// MaxClaims caps the claims on one part.
const MaxClaims = 500

// ClaimInput is one claim in a claims part.
type ClaimInput struct {
	ID         string   `json:"id"`
	Text       string   `json:"text"`
	Kind       string   `json:"kind,omitempty"`
	Evidence   []string `json:"evidence"`
	Confidence string   `json:"confidence,omitempty"`
}

// ClaimsBody is the stored shape of a claims part.
type ClaimsBody struct {
	Claims []ClaimInput `json:"claims"`
}

// claimsKind is a list of statements, each with its evidence, that the
// host keeps in the claims ledger for witnesses and the digest.
type claimsKind struct{}

func (claimsKind) Name() string { return "claims" }

func (claimsKind) Validate(_ store.Agent, _ store.Handoff, body json.RawMessage) (json.RawMessage, error) {
	var b ClaimsBody
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '[' {
		if err := json.Unmarshal(t, &b.Claims); err != nil {
			return nil, fmt.Errorf("body must be {claims: [...]}: %v", err)
		}
	} else if err := json.Unmarshal(body, &b); err != nil {
		return nil, fmt.Errorf("body must be {claims: [...]}: %v", err)
	}
	if len(b.Claims) == 0 {
		return nil, errors.New("no claims: give at least one, or leave the part out")
	}
	if len(b.Claims) > MaxClaims {
		return nil, fmt.Errorf("%d claims; at most %d fit on one part", len(b.Claims), MaxClaims)
	}
	ids := map[string]int{}
	for i := range b.Claims {
		c := &b.Claims[i]
		n := i + 1
		c.ID, c.Text = strings.TrimSpace(c.ID), strings.TrimSpace(c.Text)
		c.Kind, c.Confidence = strings.ToLower(strings.TrimSpace(c.Kind)), strings.ToLower(strings.TrimSpace(c.Confidence))
		if c.ID == "" {
			c.ID = fmt.Sprintf("c%d", n)
		}
		if prev, dup := ids[c.ID]; dup {
			return nil, fmt.Errorf("claims %d and %d share id %q", prev, n, c.ID)
		}
		ids[c.ID] = n
		if c.Text == "" {
			return nil, fmt.Errorf("claim %d (%s) has no text", n, c.ID)
		}
		if c.Kind != "" && !slices.Contains(ClaimKinds, c.Kind) {
			return nil, fmt.Errorf("claim %d (%s) has kind %q; use %s", n, c.ID, c.Kind, strings.Join(ClaimKinds, ", "))
		}
		if c.Confidence != "" && !slices.Contains(ClaimConfidences, c.Confidence) {
			return nil, fmt.Errorf("claim %d (%s) has confidence %q; use %s", n, c.ID, c.Confidence, strings.Join(ClaimConfidences, ", "))
		}
		ev := []string{}
		for _, e := range c.Evidence {
			if e = strings.TrimSpace(e); e != "" {
				ev = append(ev, e)
			}
		}
		c.Evidence = ev
	}
	return json.Marshal(b)
}

func (claimsKind) Summary(body json.RawMessage) string {
	var b ClaimsBody
	json.Unmarshal(body, &b)
	return fmt.Sprintf("(%d)", len(b.Claims))
}

// Record keeps every claim of the handoff in the ledger, replacing the
// agent's earlier ones, and emits claims.recorded.
func (claimsKind) Record(h *Host, a store.Agent, bodies []json.RawMessage) error {
	var cs []store.Claim
	now := time.Now()
	for _, body := range bodies {
		var b ClaimsBody
		if err := json.Unmarshal(body, &b); err != nil {
			return err
		}
		for _, c := range b.Claims {
			cs = append(cs, store.Claim{
				RunID: a.RunID, AgentID: a.ID, Key: c.ID, Text: c.Text, Kind: c.Kind,
				Confidence: c.Confidence, Evidence: c.Evidence, CreatedAt: now,
			})
		}
	}
	if err := h.Store.AddClaims(a.ID, cs); err != nil {
		return err
	}
	h.emit(a.RunID, a.ID, store.KindClaimsRecorded, store.ClaimsRecorded{V: store.EventVersion, Count: len(cs)})
	return nil
}

// SetVerdict records a witness's verdict on a claim and emits
// claim.verdict.
func (h *Host) SetVerdict(claimID, verdict, note, witnessID string) error {
	c, err := h.Store.Claim(claimID)
	if err != nil {
		return err
	}
	if err := h.Store.SetVerdict(claimID, verdict, note, witnessID, time.Now()); err != nil {
		return err
	}
	h.emit(c.RunID, c.AgentID, store.KindClaimVerdict, store.ClaimVerdict{
		V: store.EventVersion, ClaimID: claimID, Verdict: verdict, Note: note, WitnessID: witnessID,
	})
	return nil
}

// MarkUnwitnessed gives every claim of agentID that has no verdict yet the
// verdict unwitnessed, for a witness that did not answer in time. It
// returns how many it marked.
func (h *Host) MarkUnwitnessed(agentID, note string) (int, error) {
	cs, err := h.Store.AgentClaims(agentID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if c.Verdict != "" {
			continue
		}
		if err := h.SetVerdict(c.ID, store.VerdictUnwitnessed, note, ""); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func init() {
	RegisterPartKind(artifactKind{})
	RegisterPartKind(claimsKind{})
}
