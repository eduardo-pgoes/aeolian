package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/gitwt"
	"github.com/eduardo-pgoes/aeolian/internal/precedent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// MaxPrecedents is how many precedents one agent's prompt gets at most.
const MaxPrecedents = 8

const GlobalPrecedentSlots = 3

// A precedents block bigger than inlinePrecedents, or one that would take
// the prompt past inlinePrompt, goes into a file in the agent's scratch
// folder instead: the prompt travels on tmux's command line, which refuses
// lines of about 16KB.
const (
	inlinePrecedents = 2000
	inlinePrompt     = 8000
)

func init() {
	extraRoutes = append(extraRoutes, func(h *Host, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/precedents", h.listPrecedents)
		mux.HandleFunc("POST /api/precedents", h.addPrecedent)
	})
}

// PrecedentsDir is where the host reads and writes precedents.
func (h *Host) PrecedentsDir() string {
	if h.Precedents != "" {
		return h.Precedents
	}
	return precedent.Dir(h.Layout.Home)
}

// takesPrecedents reports whether launch adds precedents to a's prompt.
// A witness does not: its input already lists the precedents in scope of
// each claim, fresh and stale, and its verdict tool accepts only those. An
// arena judge does not either: it compares anonymized drafts against the
// rubric alone, and precedents matched on each judge's own prompt could
// differ between judges and tilt them towards drafts that echo them.
// Proposers and the evolver do get them, like any helper.
func takesPrecedents(a store.Agent) bool {
	return !isWitness(a) && !(isArenaAgent(a) && a.Role == ArenaRoleJudge)
}

// withPrecedents matches the precedents against a's role, its run's goal
// and its prompt, which holds the handoff, and returns the prompt with the
// fresh ones added, the ids added and the stale ids left out, in the
// order precedent.Select gives. It must run after the agent's scratch
// folder exists.
func (h *Host) withPrecedents(a store.Agent) (prompt string, applied, stale []string) {
	if !takesPrecedents(a) {
		return a.Prompt, nil, nil
	}
	ps, errs := precedent.Load(h.PrecedentsDir())
	for _, err := range errs {
		log.Printf("agent %s: %v", a.ID, err)
	}
	if len(ps) == 0 {
		return a.Prompt, nil, nil
	}
	var goal string
	if a.RunID != "" {
		if run, err := h.Store.Run(a.RunID); err == nil {
			goal = run.Goal
		}
	}
	role := a.Role
	if role == "" {
		role = a.Step
	}
	m := precedent.MatchTarget(ps, time.Now(), precedent.Target{
		Role: role, Repos: h.reposOf(a.Dir, ps), Text: strings.Join([]string{a.Role, goal, a.Prompt}, "\n"),
	})
	for _, p := range m.Stale {
		stale = append(stale, p.ID)
	}
	if len(m.Fresh) == 0 {
		return a.Prompt, nil, stale
	}
	m.Fresh = precedent.Select(m.Fresh, MaxPrecedents, GlobalPrecedentSlots)
	for _, p := range m.Fresh {
		applied = append(applied, p.ID)
	}
	block := precedent.Format(m.Fresh, MaxPrecedents)
	if len(block) <= inlinePrecedents && len(a.Prompt)+len(block) <= inlinePrompt {
		return a.Prompt + "\n\n" + block, applied, stale
	}
	scratch := h.Layout.ScratchDir(a.ID)
	path := filepath.Join(scratch, "precedents.md")
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		log.Printf("agent %s: precedents file: %v", a.ID, err)
		return a.Prompt, nil, stale
	}
	if err := os.WriteFile(path, []byte(block), 0o644); err != nil {
		log.Printf("agent %s: precedents file: %v", a.ID, err)
		return a.Prompt, nil, stale
	}
	return a.Prompt + "\n\n" + precedent.Heading + "\n\n" + fmt.Sprintf(
		"%d corrections the person already made apply to this work (%s). Read %s before you start: each one overrides what you would otherwise assume.",
		len(applied), strings.Join(applied, ", "), path), applied, stale
}

func (h *Host) reposOf(dir string, ps []precedent.Precedent) []string {
	if dir == "" || !slices.ContainsFunc(ps, func(p precedent.Precedent) bool { return len(p.Repos) > 0 }) {
		return nil
	}
	repo, err := gitwt.MainCheckout(dir)
	if err != nil {
		return nil
	}
	names := []string{filepath.Base(repo)}
	real, _ := filepath.EvalSymlinks(repo)
	for k, r := range h.Repos {
		p, _ := filepath.EvalSymlinks(r.Path)
		if filepath.Clean(r.Path) == repo || (p != "" && p == real) {
			names = append(names, k)
		}
	}
	return names
}

// emitPrecedents records which precedents a's prompt got and which it did
// not because they are stale.
func (h *Host) emitPrecedents(a store.Agent, applied, stale []string) {
	if len(applied) > 0 {
		h.emit(a.RunID, a.ID, store.KindPrecedentApplied, store.PrecedentIDs{V: store.EventVersion, IDs: applied})
	}
	if len(stale) > 0 {
		h.emit(a.RunID, a.ID, store.KindPrecedentStale, store.PrecedentIDs{V: store.EventVersion, IDs: stale})
	}
}

// PrecedentRequest asks for a new precedent. With ClaimID it is drawn from
// that claim: Source and, when Scope is empty, a narrow Scope come from the
// claim, and the claim is quoted under Text.
type PrecedentRequest struct {
	Kind       string   `json:"kind,omitempty"`
	Scope      []string `json:"scope,omitempty"`
	Global     bool     `json:"global,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Repos      []string `json:"repos,omitempty"`
	ReviewBy   string   `json:"review_by,omitempty"`
	Source     string   `json:"source,omitempty"`
	Confidence string   `json:"confidence,omitempty"`
	Text       string   `json:"text"`
	ClaimID    string   `json:"claim_id,omitempty"`
}

// AddPrecedent writes the precedent req asks for. Only a person adds
// precedents: no agent tool calls this.
func (h *Host) AddPrecedent(req PrecedentRequest) (precedent.Precedent, error) {
	var claim *store.Claim
	if req.ClaimID != "" {
		c, err := h.Store.Claim(req.ClaimID)
		if err != nil {
			return precedent.Precedent{}, fmt.Errorf("claim %s: %w", req.ClaimID, err)
		}
		claim = &c
	}
	now := time.Now()
	d, err := DraftPrecedent(req, claim, now)
	if err != nil {
		return precedent.Precedent{}, err
	}
	p, err := precedent.Write(h.PrecedentsDir(), d, now)
	if err != nil {
		return precedent.Precedent{}, err
	}
	var runID string
	if claim != nil {
		runID = claim.RunID
	}
	h.emit(runID, "", store.KindPrecedentAdded, store.PrecedentAdded{V: store.EventVersion, ID: p.ID, ClaimID: req.ClaimID})
	return p, nil
}

// DraftPrecedent turns a request into a draft. With a claim, the source
// names it, a narrow scope is drawn from it when the request gives none,
// and the body quotes it under the correction.
func DraftPrecedent(req PrecedentRequest, claim *store.Claim, now time.Time) (precedent.Draft, error) {
	d := precedent.Draft{Kind: req.Kind, Scope: req.Scope, Roles: req.Roles, Repos: req.Repos, Source: req.Source, Confidence: req.Confidence, Body: req.Text}
	if strings.TrimSpace(req.Text) == "" {
		return d, errors.New("say what is true: the text is empty")
	}
	if req.Global {
		if len(req.Scope) > 0 || len(req.Roles) > 0 || len(req.Repos) > 0 {
			return d, errors.New("global reaches every agent: drop scope, roles and repos, or drop global")
		}
		d.Scope = []string{precedent.Any}
	} else if len(d.Scope) == 0 && (len(d.Roles) > 0 || len(d.Repos) > 0) && claim == nil {
		d.Scope = []string{precedent.Any}
	}
	if req.ReviewBy != "" {
		t, err := time.Parse(precedent.DateLayout, req.ReviewBy)
		if err != nil {
			return d, fmt.Errorf("review_by %q is not a %s date", req.ReviewBy, precedent.DateLayout)
		}
		d.ReviewBy = t
	}
	if c := claim; c != nil {
		if d.Source == "" {
			verdict := c.Verdict
			if verdict == "" {
				verdict = "no verdict"
			}
			d.Source = fmt.Sprintf("run %s, claim %s (%s)", c.RunID, c.ID, verdict)
		}
		if len(d.Scope) == 0 {
			d.Scope = precedent.SuggestScope(c.Text, 3)
			if len(d.Scope) == 0 {
				return d, fmt.Errorf("claim %s names nothing to scope on; give a scope", c.ID)
			}
		}
		d.Body = strings.TrimSpace(req.Text) + "\n\nThis corrects an agent's claim: " + quote(c.Text)
	}
	if d.Source == "" {
		d.Source = "added by hand on " + now.Format(precedent.DateLayout)
	}
	return d, nil
}

func quote(s string) string {
	return `"` + strings.Join(strings.Fields(s), " ") + `"`
}

// PrecedentView is a precedent as the API lists it.
type PrecedentView struct {
	precedent.Precedent
	Stale bool `json:"stale"`
}

// PrecedentList is every precedent and every file that does not parse.
type PrecedentList struct {
	Precedents []PrecedentView `json:"precedents"`
	Errors     []string        `json:"errors"`
}

// ListPrecedents lists the precedents, only the stale ones when stale is
// set.
func (h *Host) ListPrecedents(stale bool) PrecedentList {
	return ListPrecedents(h.PrecedentsDir(), stale, time.Now())
}

// ListPrecedents lists the precedents in dir as of now.
func ListPrecedents(dir string, stale bool, now time.Time) PrecedentList {
	ps, errs := precedent.Load(dir)
	l := PrecedentList{Precedents: []PrecedentView{}, Errors: []string{}}
	for _, p := range ps {
		if s := p.Stale(now); s || !stale {
			l.Precedents = append(l.Precedents, PrecedentView{p, s})
		}
	}
	for _, err := range errs {
		l.Errors = append(l.Errors, err.Error())
	}
	return l
}

func (h *Host) listPrecedents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.ListPrecedents(r.URL.Query().Get("stale") != ""))
}

func (h *Host) addPrecedent(w http.ResponseWriter, r *http.Request) {
	var req PrecedentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := h.AddPrecedent(req)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, err)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, p)
}
