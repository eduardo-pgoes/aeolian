package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/github"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

type RunRequest struct {
	// Workflow is the workflow file's contents.
	Workflow string `json:"workflow"`
	Goal     string `json:"goal"`
	// Source and Ref are optional; a ref needs a source, and no second run
	// starts on a source and ref while one is busy.
	Source     string            `json:"source,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Account    string            `json:"account,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	GitConfig  [][2]string       `json:"git_config,omitempty"`
	ContextCap int64             `json:"context_cap,omitempty"`
	CapKills   int               `json:"cap_kills,omitempty"`
	TokenCap   int64             `json:"token_cap,omitempty"`
	Dir        string            `json:"dir,omitempty"`
}

// StartRun stores a run of the workflow toward goal and starts its first
// step in the background, so the caller does not wait on the cap. A run
// with a source over max_runs is stored queued and starts later.
func (h *Host) StartRun(req RunRequest) (store.Run, error) {
	src, goal := req.Workflow, req.Goal
	if goal == "" {
		return store.Run{}, errors.New("empty goal")
	}
	if req.Ref != "" && req.Source == "" {
		return store.Run{}, errors.New("ref without a source")
	}
	if err := agent.CheckEnv(req.Env); err != nil {
		return store.Run{}, err
	}
	if err := agent.CheckGitConfig(req.GitConfig); err != nil {
		return store.Run{}, err
	}
	capKills, err := checkCap(req.ContextCap, req.CapKills)
	if err != nil {
		return store.Run{}, err
	}
	if req.TokenCap < 0 {
		return store.Run{}, errors.New("token_cap: must not be negative")
	}
	if err := checkRunDir(req.Dir); err != nil {
		return store.Run{}, err
	}
	account, err := h.pickAccount(req.Account)
	if err != nil {
		return store.Run{}, err
	}
	wf, err := workflow.Parse([]byte(src))
	if err != nil {
		return store.Run{}, err
	}
	run := store.Run{
		ID: agent.NewSessionID(), Goal: goal, Workflow: src, Source: req.Source, Ref: req.Ref, Account: account,
		Env: req.Env, GitConfig: req.GitConfig, ContextCap: req.ContextCap, CapKills: capKills, TokenCap: req.TokenCap, Dir: req.Dir, CreatedAt: time.Now(),
	}
	for _, st := range wf.Steps {
		run.Steps = append(run.Steps, store.Step{Name: st.Name})
	}
	if err := h.createRun(run, wf); err != nil {
		return store.Run{}, err
	}
	return h.Store.Run(run.ID)
}

// startStep launches step i's agent in its own scratch folder. A step that
// cannot start fails its run, unless the run was killed first: then the
// step stays pending.
func (h *Host) startStep(runID string, wf *workflow.Workflow, i int, d workflow.Data) {
	step := wf.Steps[i].Name
	fail := func(err error) {
		log.Printf("run %s step %s: %v", runID, step, err)
		end := store.End{State: store.StateFailed, Reason: store.ReasonStartFailed, At: time.Now()}
		if err := h.Store.FinishStep(runID, i, end); err != nil {
			log.Printf("run %s step %s: %v", runID, step, err)
		}
		if err := h.finishRun(runID, end); err != nil {
			log.Printf("run %s: %v", runID, err)
		}
	}
	prompt, err := wf.StepPrompt(i, d)
	if err != nil {
		fail(err)
		return
	}
	id := agent.NewSessionID()
	dir, err := wf.Dir(step, "")
	if err != nil {
		fail(err)
		return
	}
	if dir == "" {
		dir, err = h.runDir(runID)
		if err != nil {
			fail(err)
			return
		}
	} else if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		fail(fmt.Errorf("step dir %q is not a directory", dir))
		return
	}
	if dir == "" {
		dir = h.Layout.ScratchDir(id)
	}
	a, err := h.launch(h.runContext(runID), store.Agent{
		ID: id, Dir: dir, Prompt: prompt, RunID: runID, Step: step, Headless: wf.Steps[i].Headless,
	})
	if errors.Is(err, context.Canceled) {
		log.Printf("run %s step %s: run killed before the step started", runID, step)
		return
	}
	if err != nil {
		fail(err)
		return
	}
	if err := h.Store.StartStep(runID, i, a.ID, a.CreatedAt); err != nil {
		log.Printf("run %s step %s: %v", runID, step, err)
	}
	h.closeStepIfRunClosed(runID, i)
}

// advance closes the step whose agent a just handed back, then starts the
// next step on that handoff, or closes the run after the last step. A
// blocked handoff closes the step and the run blocked. A run being ended
// closes with the step, done for reason ended, and starts nothing more.
func (h *Host) advance(a store.Agent, handoff store.Handoff, blocked bool) {
	// Looked at before the step finishes: EndRun forgets the ending only
	// after it has tried to finish the step itself.
	e := h.endingOf(a.RunID)
	run, err := h.Store.Run(a.RunID)
	if err != nil {
		log.Printf("advance run %s: %v", a.RunID, err)
		return
	}
	if run.State != store.StateBusy {
		return
	}
	wf, err := h.wfOf(run)
	if err != nil {
		log.Printf("advance run %s: %v", run.ID, err)
		return
	}
	i := wf.StepIndex(a.Step)
	if i < 0 {
		log.Printf("advance run %s: no step %q", run.ID, a.Step)
		return
	}
	end := store.End{State: store.StateDone, Reason: store.ReasonDelivered, At: time.Now()}
	switch {
	case blocked:
		end.State, end.Reason = store.StateBlocked, store.ReasonBlocked
	case e != nil:
		end.Reason = store.ReasonEnded
	}
	if err := h.Store.FinishStep(run.ID, i, end); err != nil {
		if !errors.Is(err, store.ErrFinished) {
			log.Printf("advance run %s: %v", run.ID, err)
		}
		return
	}
	if e != nil {
		if err := h.finishRun(run.ID, end); err != nil && !errors.Is(err, store.ErrFinished) {
			log.Printf("finish run %s: %v", run.ID, err)
		}
		h.dropEnding(run.ID, e)
		return
	}
	if blocked || i == len(wf.Steps)-1 {
		if err := h.finishRun(run.ID, end); err != nil {
			log.Printf("finish run %s: %v", run.ID, err)
		}
		return
	}
	h.startStep(run.ID, wf, i+1, workflow.Data{Goal: run.Goal, Handoff: handoff.Text, Worktrees: handoff.Worktrees, Parts: h.withVerdicts(a.ID, handoff.Parts)})
}

// closeStepOf closes the step a ran, and its run, as done for reason
// closed: a's person closed its session while it was idle. The run does not
// go on to later steps: closing the terminal means the work is over.
func (h *Host) closeStepOf(a store.Agent, at time.Time) {
	h.failStepOf(a, store.End{State: store.StateDone, Reason: store.ReasonClosed, At: at})
}

// failStepOf ends the step a runs and its run as end says, unless the step
// has finished: an agent resumed after its step handed back may end while a
// later step is running, and that does not end the run.
func (h *Host) failStepOf(a store.Agent, end store.End) {
	run, err := h.Store.Run(a.RunID)
	if err != nil {
		log.Printf("fail step of %s: %v", a.ID, err)
		return
	}
	for _, st := range run.Steps {
		if st.AgentID != a.ID {
			continue
		}
		if err := h.Store.FinishStep(run.ID, st.Index, end); err != nil {
			if !errors.Is(err, store.ErrFinished) {
				log.Printf("run %s step %s: %v", run.ID, st.Name, err)
			}
			continue
		}
		if err := h.finishRun(run.ID, end); err != nil && !errors.Is(err, store.ErrFinished) {
			log.Printf("run %s: %v", run.ID, err)
		}
	}
}

// RunRoles allows a run's agents the helpers their step lists and renders
// the role's template. Agents outside a run may spawn any role. Workflow,
// when set, parses a run's workflow; nil parses it afresh each time.
type RunRoles struct {
	Store    store.Store
	Workflow func(store.Run) (*workflow.Workflow, error)
}

func (r RunRoles) Resolve(parent store.Agent, role string, handoff store.Handoff, extra string) (string, error) {
	if parent.RunID == "" {
		return AnyRole{}.Resolve(parent, role, handoff, extra)
	}
	run, err := r.Store.Run(parent.RunID)
	if err != nil {
		return "", err
	}
	parse := r.Workflow
	if parse == nil {
		parse = func(run store.Run) (*workflow.Workflow, error) { return workflow.Parse([]byte(run.Workflow)) }
	}
	wf, err := parse(run)
	if err != nil {
		return "", err
	}
	if wf.FreeRole(parent.Step, role) {
		prompt, err := AnyRole{}.Resolve(parent, role, handoff, extra)
		return "Goal of the run: " + run.Goal + "\n\n" + prompt, err
	}
	return wf.RolePrompt(parent.Step, role, workflow.Data{
		Goal: run.Goal, Handoff: handoff.Text, Worktrees: handoff.Worktrees, ExtraInstructions: extra, Parts: handoff.Parts,
	})
}

func (h *Host) model(a store.Agent) string {
	if isWitness(a) {
		return witnessModel(a)
	}
	if a.RunID == "" {
		return ""
	}
	wf, err := h.wf(a.RunID)
	if err != nil {
		log.Printf("model for agent %s: %v", a.ID, err)
		return ""
	}
	role := ""
	if a.ParentID != "" {
		role = a.Role
	}
	return wf.Model(a.Step, role)
}

var DefaultGrants = []string{workflow.GrantClaudeAIPrefix + "claude_docs"}

func logSkipped(agentID string, access agent.ConnectorAccess) {
	for _, err := range access.Skipped {
		log.Printf("agent %s: %v", agentID, err)
	}
}

func (h *Host) grants(a store.Agent) ([]string, error) {
	if a.RunID == "" || isWitness(a) {
		return nil, nil
	}
	wf, err := h.wf(a.RunID)
	if err != nil {
		return nil, err
	}
	if a.ParentID == "" {
		return wf.Grants(a.Step, ""), nil
	}
	if wf.FreeRole(a.Step, a.Role) {
		parent, err := h.Store.Agent(a.ParentID)
		if err != nil {
			return nil, fmt.Errorf("parent %s: %w", a.ParentID, err)
		}
		return h.grants(parent)
	}
	return wf.Grants(a.Step, a.Role), nil
}

func (h *Host) memory(a store.Agent) string {
	if a.RunID == "" {
		return ""
	}
	wf, err := h.wf(a.RunID)
	if err != nil {
		log.Printf("memory for agent %s: %v", a.ID, err)
		return ""
	}
	role := ""
	if a.ParentID != "" {
		role = a.Role
	}
	return wf.Memory(a.Step, role)
}

func (h *Host) setup(ctx context.Context, a store.Agent, headless bool) (agent.Setup, error) {
	s := agent.Setup{HostURL: h.URL, Headless: headless, Boot: h.boot(a)}
	if h.confined(a) {
		s.ReadRoots = h.readRoots(a)
	}
	if a.RunID != "" {
		run, err := h.Store.Run(a.RunID)
		if err != nil {
			return s, fmt.Errorf("run of agent %s: %w", a.ID, err)
		}
		s.Env, s.GitConfig = run.Env, run.GitConfig
	}
	grants, err := h.grants(a)
	if err != nil {
		return s, fmt.Errorf("grants for agent %s: %w", a.ID, err)
	}
	s.Linear = slices.Contains(grants, workflow.GrantLinear)
	for _, g := range grants {
		if name, ok := workflow.MCPServer(g); ok {
			s.MCP = append(s.MCP, name)
		}
		if name, ok := workflow.ClaudeAIConnector(g); ok {
			s.Connectors = append(s.Connectors, name)
		}
	}
	if !isWitness(a) {
		for _, g := range h.DefaultGrants {
			if name, ok := workflow.ClaudeAIConnector(g); ok && !slices.Contains(s.Connectors, name) && !slices.Contains(s.DefaultConnectors, name) {
				s.DefaultConnectors = append(s.DefaultConnectors, name)
			}
		}
	}
	if headless && h.GitHub != nil {
		bot, err := h.GitHub.Identity(ctx)
		if err != nil {
			return s, fmt.Errorf("agent %s commits as its configured GitHub identity, but that identity is unknown: %w", a.ID, err)
		}
		if bot.Name == "" || bot.Email == "" {
			return s, fmt.Errorf("agent %s commits as its configured GitHub identity, but that identity came back empty", a.ID)
		}
		s.Bot = agent.Committer{Name: bot.Name, Email: bot.Email}
	}
	grant, permissions := "", map[string]string(nil)
	switch {
	case slices.Contains(grants, workflow.GrantGitHub):
		grant, permissions = workflow.GrantGitHub, github.WritePermissions
	case slices.Contains(grants, workflow.GrantGitHubRead):
		grant, permissions = workflow.GrantGitHubRead, github.ReadPermissions
	}
	if grant == "" {
		return s, nil
	}
	if h.GitHub == nil {
		return s, fmt.Errorf("agent %s is granted %s, but config.json has no github section", a.ID, grant)
	}
	token, err := h.GitHub.Token(ctx, permissions)
	if err != nil {
		return s, fmt.Errorf("agent %s is granted %s: %w", a.ID, grant, err)
	}
	s.GHToken = token
	log.Printf("agent %s: minted a %s token", a.ID, grant)
	return s, nil
}

func (h *Host) helperHeadless(parent store.Agent, role string) bool {
	if parent.Headless || parent.RunID == "" {
		return parent.Headless
	}
	wf, err := h.wf(parent.RunID)
	if err != nil {
		log.Printf("headless for helper of %s: %v", parent.ID, err)
		return false
	}
	return wf.Headless(parent.Step, role)
}

func (h *Host) roleDir(parent store.Agent, role string) (string, error) {
	if parent.RunID == "" {
		return "", nil
	}
	wf, err := h.wf(parent.RunID)
	if err != nil {
		return "", err
	}
	d, err := wf.Dir(parent.Step, role)
	if err != nil || d != "" {
		return d, err
	}
	return h.runDir(parent.RunID)
}

func (h *Host) runDir(runID string) (string, error) {
	run, err := h.Store.Run(runID)
	if err != nil {
		return "", err
	}
	if run.Dir == "" {
		return "", nil
	}
	if fi, err := os.Stat(run.Dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("run dir %q is not a directory", run.Dir)
	}
	return run.Dir, nil
}

func checkRunDir(dir string) error {
	if dir == "" {
		return nil
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("dir %q: must be an absolute path", dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("dir %q is not a directory", dir)
	}
	return nil
}

func (h *Host) startRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	run, err := h.StartRun(req)
	var live *store.LiveRunError
	switch {
	case errors.As(err, &live):
		http.Error(w, err.Error(), http.StatusConflict)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (h *Host) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := h.Store.Run(r.PathValue("run"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// RunView is a run as the map draws it: its steps plus every agent it
// started, helpers included. Ink and mark come from the run's workflow: a
// step's agent takes its step's, a helper its role's. Ink and tuning are
// the pair workflow.Looks picked for the step or role in this run, so no
// two of them look alike; tuning is empty for a helper with no role. Blocked is set on an
// agent waiting in spawn; Cols and Rows only on an agent whose session is live.
type RunView struct {
	store.Run
	Steps  []StepView  `json:"steps"`
	Agents []AgentView `json:"agents"`
	Tokens int64       `json:"tokens"`
}

type StepView struct {
	store.Step
	Ink    string `json:"ink"`
	Mark   string `json:"mark"`
	Tuning string `json:"tuning,omitempty"`
}

type AgentView struct {
	store.Agent
	Blocked bool   `json:"blocked"`
	Ink     string `json:"ink"`
	Mark    string `json:"mark"`
	Tuning  string `json:"tuning,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Context int64  `json:"context"`
}

// Runs returns every run f selects, newest first, with its agents in start
// order.
func (h *Host) Runs(f store.RunFilter) ([]RunView, error) {
	runs, err := h.Store.Runs(f)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(runs))
	for i, r := range runs {
		ids[i] = r.ID
	}
	agents, err := h.Store.AgentsOfRuns(ids)
	if err != nil {
		return nil, err
	}
	sessions, err := h.Keeper.List()
	if err != nil {
		log.Printf("list sessions: %v", err)
	}
	live := map[string]keeper.Session{}
	for _, s := range sessions {
		live[s.Name] = s
	}
	h.mu.Lock()
	blocked := h.blockedLocked()
	h.mu.Unlock()
	agentIDs := make([]string, 0, len(agents))
	for _, a := range agents {
		if a.RunID != "" {
			agentIDs = append(agentIDs, a.ID)
		}
	}
	levels := h.contextLevels(agentIDs)
	tokens, err := h.Store.RunTokens(ids)
	if err != nil {
		log.Printf("run tokens: %v", err)
	}
	byRun := map[string][]AgentView{}
	for _, a := range agents {
		if a.RunID != "" {
			s := live[a.TmuxSession]
			byRun[a.RunID] = append(byRun[a.RunID], AgentView{Agent: a, Blocked: blocked[a.ID], Cols: s.Cols, Rows: s.Rows, Context: levels[a.ID]})
		}
	}
	views := make([]RunView, len(runs))
	for i, r := range runs {
		wf, err := h.wfOf(r)
		if err != nil {
			log.Printf("run %s: %v", r.ID, err)
			wf = &workflow.Workflow{}
		}
		views[i] = RunView{Run: r, Steps: make([]StepView, len(r.Steps)), Agents: byRun[r.ID], Tokens: tokens[r.ID]}
		var free []string
		for _, a := range views[i].Agents {
			if a.ParentID != "" {
				free = append(free, a.Role)
			}
		}
		stepLooks, roleLooks := wf.Looks(free)
		for j, st := range r.Steps {
			views[i].Steps[j] = StepView{Step: st}
			if k := wf.StepIndex(st.Name); k >= 0 {
				l := stepLooks[st.Name]
				views[i].Steps[j].Ink, views[i].Steps[j].Mark, views[i].Steps[j].Tuning = l.Ink, wf.Steps[k].Mark, l.Tuning
			}
		}
		for j := range views[i].Agents {
			a := &views[i].Agents[j]
			if a.ParentID != "" {
				role, ok := wf.Roles[a.Role]
				a.Ink, a.Mark = role.Ink, role.Mark
				if !ok {
					a.Ink, a.Mark = workflow.FreeInk(a.Role), a.Role
				}
				if l, ok := roleLooks[a.Role]; ok {
					a.Ink, a.Tuning = l.Ink, l.Tuning
				}
			} else if k := wf.StepIndex(a.Step); k >= 0 {
				l := stepLooks[a.Step]
				a.Ink, a.Mark, a.Tuning = l.Ink, wf.Steps[k].Mark, l.Tuning
			}
		}
		if views[i].Agents == nil {
			views[i].Agents = []AgentView{}
		}
	}
	return views, nil
}

// listRuns serves GET /api/runs. source and ref filter; limit=N keeps the
// N newest; with active=1 the queued, busy and unacked runs come too, and
// so do the runs in include (comma separated), such as a pinned one.
func (h *Host) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{Source: q.Get("source"), Ref: q.Get("ref"), Active: q.Get("active") == "1" || q.Get("active") == "true"}
	if l := q.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			http.Error(w, "limit must be a non-negative number", http.StatusBadRequest)
			return
		}
		f.Limit = n
	}
	if inc := q.Get("include"); inc != "" {
		f.Include = strings.Split(inc, ",")
	}
	runs, err := h.Runs(f)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}
