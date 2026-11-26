package host

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// ReplayCase is one past human correction replayed as a claim, with the
// verdict the person would have given.
type ReplayCase struct {
	ID                 string           `json:"id"`
	ClaimText          string           `json:"claim_text"`
	Evidence           []ReplayEvidence `json:"evidence"`
	ExpectedVerdict    string           `json:"expected_verdict"`
	PrecedentIDs       []string         `json:"precedent_ids"`
	Why                string           `json:"why"`
	ExpectStaleFlag    []string         `json:"expect_stale_flag,omitempty"`
	ExpectNoteContains []string         `json:"expect_note_contains,omitempty"`
}

// ReplayEvidence is evidence whose content is given: the replay feeds
// Excerpt to the witness as what was read at Ref.
type ReplayEvidence struct {
	Kind    string `json:"kind"`
	Ref     string `json:"ref"`
	Excerpt string `json:"excerpt"`
}

// ReplayResult is how the witness did on one case.
type ReplayResult struct {
	Case     ReplayCase `json:"case"`
	Verdict  string     `json:"verdict"`
	Note     string     `json:"note"`
	Injected []string   `json:"injected"`
	Stale    []string   `json:"stale"`
	Pass     bool       `json:"pass"`
	Problems []string   `json:"problems,omitempty"`
	// Unmatched lists the case's precedent_ids the matcher did not hand
	// the witness; it is a hint about scopes, not a failure.
	Unmatched []string `json:"unmatched,omitempty"`
}

// ReplayReport scores a replay: OK when at least Bar cases pass and every
// case whose id starts with one of Required does.
type ReplayReport struct {
	Results  []ReplayResult `json:"results"`
	Passed   int            `json:"passed"`
	Bar      int            `json:"bar"`
	Required []string       `json:"required"`
	OK       bool           `json:"ok"`
}

// LoadReplayCases reads every *.json case in dir, sorted by name.
func LoadReplayCases(dir string) ([]ReplayCase, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	slices.Sort(paths)
	var cases []ReplayCase
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var c ReplayCase
		if err := json.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if c.ID == "" || c.ClaimText == "" || c.ExpectedVerdict == "" {
			return nil, fmt.Errorf("%s: a case needs id, claim_text and expected_verdict", p)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no *.json cases in %s", dir)
	}
	return cases, nil
}

// evidenceRef is how a case's evidence reads in a claim: a command is
// marked as one.
func (e ReplayEvidence) evidenceRef() string {
	if e.Kind == "cmd" && !strings.HasPrefix(strings.ToLower(e.Ref), "cmd:") {
		return "cmd: " + e.Ref
	}
	return e.Ref
}

// ReplayOptions tune a replay.
type ReplayOptions struct {
	// Parallel is how many cases run at once; under 1 means 1.
	Parallel int
	// Timeout is each case's; zero means the config's.
	Timeout time.Duration
	// Match has the host's matcher pick each claim's precedents from the
	// folder. Otherwise a case gets exactly its precedent_ids, as the
	// fixtures intend, still split fresh or stale by date.
	Match bool
}

// Replay runs the witness on each case exactly as on a step's claims: one
// witness per case, the case's excerpts fed as its evidence's content.
func (h *Host) Replay(ctx context.Context, cases []ReplayCase, c WitnessConfig, o ReplayOptions) []ReplayResult {
	parallel := o.Parallel
	if parallel < 1 {
		parallel = 1
	}
	results := make([]ReplayResult, len(cases))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, rc := range cases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = h.replayOne(ctx, rc, c, o)
		}()
	}
	wg.Wait()
	return results
}

func (h *Host) replayOne(ctx context.Context, rc ReplayCase, c WitnessConfig, o ReplayOptions) ReplayResult {
	res := ReplayResult{Case: rc}
	fail := func(err error) ReplayResult {
		res.Problems = append(res.Problems, err.Error())
		return res
	}
	id := agent.NewSessionID()
	now := time.Now()
	producer := store.Agent{
		ID: id, SessionID: id, TmuxSession: agent.SessionName(id), Dir: h.Layout.ScratchDir(id),
		Prompt: "replay " + rc.ID, State: store.StateBusy, CreatedAt: now,
	}
	if err := os.MkdirAll(producer.Dir, 0o755); err != nil {
		return fail(err)
	}
	if err := h.Store.CreateAgent(producer); err != nil {
		return fail(err)
	}
	if err := h.Store.Done(id, store.Handoff{Text: "replay " + rc.ID}, store.ReasonDelivered, now); err != nil {
		return fail(err)
	}
	refs, excerpts := []string{}, []string{}
	for _, e := range rc.Evidence {
		refs = append(refs, e.evidenceRef())
		excerpts = append(excerpts, e.Excerpt)
	}
	key := rc.ID
	if k, _, ok := strings.Cut(rc.ID, "-"); ok {
		key = k
	}
	if err := h.Store.AddClaims(id, []store.Claim{{Key: key, Text: rc.ClaimText, Evidence: refs, CreatedAt: now}}); err != nil {
		return fail(err)
	}
	claimID := store.ClaimID(id, 0)
	claims, _ := h.Store.AgentClaims(id)
	given := &WitnessGiven{Excerpts: map[string][]string{claimID: excerpts}}
	if !o.Match {
		given.Precedents = map[string][]string{claimID: append([]string{}, rc.PrecedentIDs...)}
	}
	in := h.witnessInput(c, producer, claims, given, now)
	for _, p := range in.Precedents {
		res.Injected = append(res.Injected, p.ID)
	}
	for _, p := range in.StalePrecedents {
		res.Stale = append(res.Stale, p.ID)
	}
	for _, p := range rc.PrecedentIDs {
		if !slices.Contains(res.Injected, p) && !slices.Contains(res.Stale, p) {
			res.Unmatched = append(res.Unmatched, p)
		}
	}

	timeout := o.Timeout
	if timeout <= 0 {
		timeout = c.timeout()
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := h.RunWitness(wctx, c, producer, given); err != nil {
		res.Problems = append(res.Problems, "witness: "+err.Error())
	}
	cl, err := h.Store.Claim(claimID)
	if err != nil {
		return fail(err)
	}
	res.Verdict, res.Note = cl.Verdict, cl.VerdictNote
	if res.Verdict != rc.ExpectedVerdict {
		res.Problems = append(res.Problems, fmt.Sprintf("verdict %s, want %s", orNone(res.Verdict), rc.ExpectedVerdict))
	}
	for _, p := range rc.ExpectStaleFlag {
		if !slices.Contains(res.Stale, p) {
			res.Problems = append(res.Problems, "stale precedent "+p+" not flagged")
		}
		if slices.Contains(res.Injected, p) {
			res.Problems = append(res.Problems, "stale precedent "+p+" injected")
		}
	}
	for _, s := range rc.ExpectNoteContains {
		if !strings.Contains(strings.ToLower(res.Note), strings.ToLower(s)) {
			res.Problems = append(res.Problems, fmt.Sprintf("note lacks %q", s))
		}
	}
	res.Pass = res.Verdict == rc.ExpectedVerdict && len(res.Problems) == 0
	return res
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// Score turns results into a report against bar and the required case
// prefixes.
func Score(results []ReplayResult, bar int, required []string) ReplayReport {
	r := ReplayReport{Results: results, Bar: bar, Required: required, OK: true}
	for _, res := range results {
		if res.Pass {
			r.Passed++
		}
	}
	if r.Passed < bar {
		r.OK = false
	}
	for _, req := range required {
		found := false
		for _, res := range results {
			if strings.HasPrefix(res.Case.ID, req) {
				found = true
				r.OK = r.OK && res.Pass
			}
		}
		r.OK = r.OK && found
	}
	return r
}
