// Package host serves agents their conductor tools and hook endpoint and
// starts them through the keeper. It never owns an agent's process: agents
// live in tmux and outlast the host.
package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/github"
	"github.com/eduardo-pgoes/aeolian/internal/keeper"
	"github.com/eduardo-pgoes/aeolian/internal/store"
)

// Keeper is the part of the terminal keeper the host drives.
type Keeper interface {
	Start(name, dir string, argv []string) error
	Type(name, text string) error
	Kill(name string) error
	List() ([]keeper.Session, error)
	Capture(name string) (string, error)
}

// Tmux is the real keeper.
type Tmux struct{}

func (Tmux) Start(name, dir string, argv []string) error { return keeper.Start(name, dir, argv) }
func (Tmux) Type(name, text string) error                { return keeper.Type(name, text) }
func (Tmux) Kill(name string) error                      { return keeper.Kill(name) }
func (Tmux) List() ([]keeper.Session, error)             { return keeper.List() }
func (Tmux) Capture(name string) (string, error)         { return keeper.Capture(name) }

type GitHubAuth interface {
	Token(ctx context.Context, permissions map[string]string) (string, error)
	Identity(ctx context.Context) (github.Identity, error)
}

// DefaultKillGrace is how long the host waits after answering done before
// killing the session, so Claude Code can take in the answer first.
const DefaultKillGrace = 2 * time.Second

// Repo is a repository entry of config.json.
type Repo struct {
	Path        string `json:"path"`
	GitHub      string `json:"github"`
	Base        string `json:"base"`
	Description string `json:"description"`
}

type Host struct {
	Store  store.Store
	Layout agent.Layout
	Keeper Keeper
	// URL is the base URL agents reach the host at; it is written into
	// their config at launch.
	URL       string
	KillGrace time.Duration
	// MaxAgents caps the working agents: busy ones not blocked waiting on
	// helpers. Starts over it wait in order. An idle agent holds no slot, so
	// one that wakes (a person types, a handoff is typed in) can take the
	// count over the cap, as Resume can; the cap throttles tokens and API
	// concurrency, not sessions. Zero means DefaultMaxAgents.
	MaxAgents int
	// MaxRuns caps the runs with a source that are busy at once; a sourced
	// run over it waits queued. Zero means DefaultMaxRuns.
	MaxRuns         int
	MaxRunsBySource map[string]int
	// Roles decides which helpers spawn may start. Nil means RunRoles.
	Roles RoleResolver
	// ProgressInterval is how often a blocked spawn reports progress. Zero
	// means DefaultProgressInterval.
	ProgressInterval time.Duration
	GitHub           GitHubAuth
	// OnFinish is the argv run with a run's inbox entry on stdin whenever
	// the run finishes. Empty means none.
	OnFinish []string
	// OnFinishTimeout is how long OnFinish may run before it is killed.
	// Zero means DefaultOnFinishTimeout.
	OnFinishTimeout time.Duration
	// EndGrace is how long EndRun gives an interactive lead to hand back.
	// Zero means DefaultEndGrace.
	EndGrace time.Duration
	// Transcripts is the folder Claude Code keeps its transcripts in, one
	// subfolder per project (~/.claude/projects). The host reads an
	// agent's last message from it when it writes a handoff for an agent
	// that ended without done. Empty means none is read.
	Transcripts string
	// StallAfter is how long a busy helper may go without a hook or a
	// transcript write before the watchdog ends it as lost and its parent
	// gets a handoff saying so. Zero means DefaultStallAfter; negative
	// turns stall detection off.
	StallAfter time.Duration
	// IdleCloseAfter is how long an idle agent that handed back before it
	// was resumed may sit at its prompt before the watchdog closes it; an
	// agent that never handed back is never closed. Zero means
	// DefaultIdleCloseAfter; negative turns it off.
	IdleCloseAfter   time.Duration
	LaunchStallAfter time.Duration
	// CPUUsage, when set, returns the CPU time used by the systemd scope of
	// the session whose pane process is pid; nil reads its cgroup. A quiet
	// helper whose scope keeps using CPU is working, not stalled.
	CPUUsage    func(pid int) (time.Duration, error)
	ScopeOf     func(pid int) (string, error)
	ScopeResult func(unit string) (string, error)
	// EventsRetention is how long a finished run keeps its hook input in
	// the event log; see PruneEvents. Zero keeps it forever.
	EventsRetention time.Duration
	// PartKinds are the kinds of part done accepts, by name. Nil means
	// every kind registered with RegisterPartKind.
	PartKinds map[string]PartKind
	// DoneGates run after the ones registered with RegisterDoneGate.
	DoneGates []DoneGate
	// Repos are config.json's repos, by key: the repos a worktree request
	// can name by key, with the base a new branch starts from.
	Repos map[string]Repo
	// WorktreeRoot is where the host makes worktrees. Empty means
	// $AEOLIAN_HOME/worktrees.
	WorktreeRoot string
	// GCAfter is how long after its ack a run's folders and worktrees are
	// kept. Zero means DefaultGCAfter.
	GCAfter time.Duration
	// AutoGC runs GC when Watch starts and then every GCInterval. Off,
	// GC runs only when asked through POST /api/gc.
	AutoGC bool
	// Precedents is the folder of precedent files. Empty means
	// $AEOLIAN_HOME/precedents.
	Precedents    string
	DefaultGrants []string
	// Boot decides each agent's boot profile and which agents have their
	// reads confined. The zero value boots helpers in DefaultBootRoles
	// lean, every other agent full, and confines DefaultConfined.
	Boot           BootConfig
	Accounts       map[string]string
	DefaultAccount string
	runAccounts    sync.Map
	launchBlocked  sync.Map

	// mu guards the fields below it up to starting. Never call a Store
	// write, or emit, while holding it: a write can wait seconds on SQLite's
	// lock, and every launch, admit and spawn wait queues behind mu. Only
	// liveLocked and runCtxLocked may read the store under it.
	mu       sync.Mutex
	queue    []chan struct{}
	reserved int
	spawns   map[string]*spawnWait // helper id -> its parent's wait
	calls    map[string]*call      // call token -> open tools/call
	missing  map[string]bool       // agent id -> session absent on the last sweep
	cpu      map[string]cpuSample  // agent id -> scope CPU time when it was last quiet
	scopes   map[string]scopeSeen
	runCtxs  map[string]runCtx   // run id -> the context its starts wait on
	mail     map[string]*mailbox // parent id -> helpers reporting later
	ending   map[string]*ending  // run id -> its EndRun in progress

	// starting is held for reading from a launch's last look at its context
	// until its session has started, and for writing by KillRun while it
	// cancels a run's context.
	starting sync.RWMutex

	typing sync.Mutex

	// admitting is held from counting the busy runs until a run is stored
	// or promoted, so two admissions never both take the last slot.
	admitting sync.Mutex

	hooks     hookState // what hooks and typed messages left behind; own lock
	repoLocks sync.Map  // repo path -> *sync.Mutex; see repoLock

	streamsOnce  sync.Once
	streamsClose sync.Once
	streamsStop  chan struct{} // closed by StopStreams

	wfs     sync.Map // run id -> *workflow.Workflow, parsed once; see wf
	servers sync.Map // agent id -> *mcp.Server
	closing sync.Map // agent id -> tmux session name, set by done
	mcp     http.Handler
	once    sync.Once
}

func (h *Host) Handler() http.Handler {
	h.once.Do(func() {
		h.mcp = mcp.NewStreamableHTTPHandler(h.server, &mcp.StreamableHTTPOptions{Stateless: true})
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/{agent}", h.serveMCP)
	mux.HandleFunc("POST /hooks/{agent}/{event}", h.serveHook)
	mux.HandleFunc("POST /api/agents", h.startAgent)
	mux.HandleFunc("GET /api/agents", h.listAgents)
	mux.HandleFunc("GET /api/agents/{agent}", h.getAgent)
	mux.HandleFunc("POST /api/agents/{agent}/kill", h.killAgent)
	mux.HandleFunc("POST /api/agents/{agent}/resume", h.resumeAgent)
	mux.HandleFunc("POST /api/runs", h.startRun)
	mux.HandleFunc("GET /api/runs", h.listRuns)
	mux.HandleFunc("GET /api/runs/{run}", h.getRun)
	mux.HandleFunc("POST /api/runs/{run}/kill", h.killRun)
	mux.HandleFunc("POST /api/runs/{run}/end", h.endRun)
	mux.HandleFunc("POST /api/runs/{run}/ack", h.ackRun)
	mux.HandleFunc("GET /api/inbox", h.listInbox)
	mux.HandleFunc("GET /api/workflows", h.listWorkflows)
	mux.HandleFunc("GET /api/host", h.getHost)
	mux.HandleFunc("GET /api/events", h.listEvents)
	mux.HandleFunc("GET /api/events/stream", h.streamEvents)
	mux.HandleFunc("POST /api/gc", h.postGC)
	mux.HandleFunc("GET /api/accounts", h.listAccounts)
	for _, add := range extraRoutes {
		add(h, mux)
	}
	return mux
}

func (h *Host) serveMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("agent")
	if _, err := h.Store.Agent(id); err != nil {
		httpError(w, err)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.Header.Del(callHeader)
	var msg rpcMessage
	if json.Unmarshal(body, &msg) == nil {
		switch msg.Method {
		case "tools/call":
			token, closeCall := h.openCall(r.Context(), id, msg.ID)
			defer closeCall()
			r.Header.Set(callHeader, token)
		case "notifications/cancelled":
			h.cancelCall(id, msg.Params.RequestID)
		}
	}
	h.mcp.ServeHTTP(w, r)
	if name, ok := h.closing.LoadAndDelete(id); ok {
		go h.kill(name.(string))
	}
}

func (h *Host) kill(name string) {
	grace := h.KillGrace
	if grace == 0 {
		grace = DefaultKillGrace
	}
	time.Sleep(grace)
	if err := h.Keeper.Kill(name); err != nil && !errors.Is(err, keeper.ErrNoSession) {
		log.Printf("kill %s: %v", name, err)
	}
}

func (h *Host) server(r *http.Request) *mcp.Server {
	id := r.PathValue("agent")
	if s, ok := h.servers.Load(id); ok {
		return s.(*mcp.Server)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "aeolian", Version: "0.1.0"}, nil)
	doneDesc := "Hand back your result and end your session. Call it once, when your work is finished. " +
		"List every git worktree you created or changed; your session closes right after the call returns."
	a, aerr := h.Store.Agent(id)
	if aerr == nil && a.Headless && a.ParentID == "" && a.RunID != "" {
		doneDesc += " If a question only a person can answer stops the work, call it with outcome blocked " +
			"and the question in text: the run then ends blocked and no later step starts."
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "done",
		Description: doneDesc,
		InputSchema: inputSchema[DoneInput](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in DoneInput) (*mcp.CallToolResult, any, error) {
		return h.done(id, in)
	})
	// An arena is a bounded tournament the host drives: its agents get no
	// spawn, so a forfeit's kill always ends all of the agent's work.
	if aerr != nil || !isArenaAgent(a) {
		h.addSpawnTool(s, id)
	}
	if aerr == nil {
		for _, add := range extraTools {
			add(h, s, id, a)
		}
	}
	actual, _ := h.servers.LoadOrStore(id, s)
	return actual.(*mcp.Server)
}

func (h *Host) addSpawnTool(s *mcp.Server, id string) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "spawn",
		Description: "Start a helper in a role. By default the call waits for the helper to finish and its handoff is the result, which can take a long time. " +
			"With wait false the call returns at once with the helper's id, so you can start several; collect their handoffs with await. " +
			"In an interactive session a handoff nobody awaits arrives as a new message instead. " +
			"The helper starts in the first worktree your handoff lists, or in an empty scratch folder when it lists none. " +
			"For a helper that changes code, pass worktree {repo, branch} instead of running git worktree add yourself: the host makes a fresh worktree on a new branch, starts the helper there, and cleans it up later.",
		InputSchema: inputSchema[SpawnInput](),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in SpawnInput) (*mcp.CallToolResult, any, error) {
		return h.spawn(ctx, req, id, in)
	})
}

type DoneInput struct {
	Text      string          `json:"text" jsonschema:"your result, written for whoever picks up the work next"`
	Worktrees []WorktreeInput `json:"worktrees" jsonschema:"every git worktree you created or changed; empty when there are none"`
	Outcome   string          `json:"outcome,omitempty" jsonschema:"done, the default, when the work is finished; blocked when a question only a person can answer stops it, with the question in text"`
	Parts     []PartInput     `json:"parts,omitempty" jsonschema:"optional structured results beside text, each with a kind and a body"`
}

const (
	OutcomeDone    = "done"
	OutcomeBlocked = "blocked"
)

// reasonFor is the reason an agent's done with outcome records.
func reasonFor(outcome string) string {
	if outcome == OutcomeBlocked {
		return store.ReasonBlocked
	}
	return store.ReasonDelivered
}

type WorktreeInput struct {
	Repo   string `json:"repo" jsonschema:"path of the main repository checkout"`
	Path   string `json:"path" jsonschema:"path of the worktree"`
	Branch string `json:"branch" jsonschema:"branch checked out in the worktree"`
}

func (h *Host) done(id string, in DoneInput) (*mcp.CallToolResult, any, error) {
	call, err := h.checkDoneInput(id, in)
	if err != nil {
		return nil, nil, err
	}
	return h.finishDone(call)
}

// checkDoneInput validates a done call by agent id and returns it, ready
// for finishDone.
func (h *Host) checkDoneInput(id string, in DoneInput) (DoneCall, error) {
	a, err := h.Store.Agent(id)
	if err != nil {
		return DoneCall{}, err
	}
	switch in.Outcome {
	case "", OutcomeDone:
	case OutcomeBlocked:
		if a.ParentID != "" {
			return DoneCall{}, errors.New("only a step's agent can end blocked; put the question in your text and call done without an outcome")
		}
	default:
		return DoneCall{}, fmt.Errorf("outcome %q is neither %s nor %s", in.Outcome, OutcomeDone, OutcomeBlocked)
	}
	handoff := toHandoff(in.Text, in.Worktrees)
	if handoff.Parts, err = h.toParts(a, handoff, in.Parts); err != nil {
		return DoneCall{}, err
	}
	call := DoneCall{Agent: a, Handoff: handoff, Outcome: in.Outcome}
	if err := h.checkDone(call); err != nil {
		return DoneCall{}, err
	}
	return call, nil
}

// finishDone records a checked done call and routes its handoff.
func (h *Host) finishDone(call DoneCall) (*mcp.CallToolResult, any, error) {
	a, handoff, id := call.Agent, call.Handoff, call.Agent.ID
	if err := h.Store.Done(id, handoff, reasonFor(call.Outcome), time.Now()); err != nil {
		return nil, nil, err
	}
	h.recordParts(a, handoff.Parts)
	h.agentEnded(a)
	h.forgetMailbox(id)
	h.closing.Store(id, a.TmuxSession)
	if a.ParentID == "" && a.RunID != "" {
		go func() {
			if h.holdDone(call) {
				h.advance(a, handoff, call.Outcome == OutcomeBlocked)
			}
		}()
	}
	if a.ParentID != "" {
		h.helperDone(a, handoff)
	}
	h.admit()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
		Text: "Handoff recorded. Your session closes now; do nothing further.",
	}}}, nil, nil
}

// serveHook records a hook with its input, read up to maxHookBody, and
// answers with an empty body: Claude Code adds a SessionStart or
// UserPromptSubmit hook's stdout to the agent's context.
func (h *Host) serveHook(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, maxHookBody))
	io.Copy(io.Discard, r.Body)
	event := r.PathValue("event")
	if err := h.recordHook(r.PathValue("agent"), event, body); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
	if idleHook(event) {
		h.admit()
	}
}

type StartRequest struct {
	Prompt string `json:"prompt"`
}

// Start launches a new agent on prompt in its own scratch folder, once there
// is a slot under the cap. It gives up waiting for the slot when ctx ends.
func (h *Host) Start(ctx context.Context, prompt string) (store.Agent, error) {
	if prompt == "" {
		return store.Agent{}, errors.New("empty prompt")
	}
	id := agent.NewSessionID()
	return h.launch(ctx, store.Agent{ID: id, Dir: h.Layout.ScratchDir(id), Prompt: prompt})
}

// launch waits for a slot, then stores a and starts its terminal. a needs
// ID, Dir and Prompt. The agent is stored before tmux starts so its first
// hooks find it. A ctx that has ended by the time the slot comes refuses
// the start.
//
// A confined agent may also read the folders in readRoots.
func (h *Host) launch(ctx context.Context, a store.Agent, readRoots ...string) (store.Agent, error) {
	if err := h.acquire(ctx, a.RunID, a.ID); err != nil {
		return store.Agent{}, err
	}
	defer h.release()
	setup, err := h.setup(ctx, a, a.Headless)
	if err != nil {
		return store.Agent{}, err
	}
	if setup.ReadRoots != nil {
		setup.ReadRoots = append(setup.ReadRoots, readRoots...)
	}
	h.starting.RLock()
	defer h.starting.RUnlock()
	if err := ctx.Err(); err != nil {
		return store.Agent{}, err
	}
	lay, err := h.agentLayout(a)
	if err != nil {
		return store.Agent{}, err
	}
	if err := lay.Prepare(a.ID, a.Dir, setup); err != nil {
		return store.Agent{}, err
	}
	access, err := lay.ConnectorDeny(ctx, a.Dir, setup)
	if err != nil {
		return store.Agent{}, fmt.Errorf("agent %s: %w", a.ID, err)
	}
	logSkipped(a.ID, access)
	var applied, stale []string
	a.Prompt, applied, stale = h.withPrecedents(a)
	a.SessionID, a.TmuxSession = a.ID, agent.SessionName(a.ID)
	a.State, a.CreatedAt = store.StateBusy, time.Now()
	if err := h.Store.CreateAgent(a); err != nil {
		return store.Agent{}, err
	}
	argv := lay.Argv(agent.Launch{AgentID: a.ID, SessionID: a.ID, Prompt: a.Prompt, Model: h.model(a), Headless: a.Headless, Memory: h.memory(a), Boot: setup.Boot, Connectors: access.On, Deny: access.Deny})
	err = lay.WritePrompt(a.ID, a.Prompt)
	if err == nil {
		err = h.Keeper.Start(a.TmuxSession, a.Dir, argv)
	}
	if err != nil {
		err = fmt.Errorf("start agent %s: %w", a.ID, err)
		if derr := h.Store.Done(a.ID, store.Handoff{Text: err.Error()}, store.ReasonStartFailed, time.Now()); derr != nil {
			log.Printf("mark failed agent %s done: %v", a.ID, derr)
		}
		return store.Agent{}, err
	}
	h.emitPrecedents(a, applied, stale)
	return a, nil
}

func (h *Host) startAgent(w http.ResponseWriter, r *http.Request) {
	var req StartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "empty prompt", http.StatusBadRequest)
		return
	}
	a, err := h.Start(r.Context(), req.Prompt)
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// listAgents returns the agents of ?run=, every agent ever with ?all=1,
// and by default the live ones, without handoffs.
func (h *Host) listAgents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var agents []store.Agent
	var err error
	switch {
	case q.Get("run") != "":
		agents, err = h.Store.AgentsOfRuns([]string{q.Get("run")})
	case q.Get("all") == "1" || q.Get("all") == "true":
		agents, err = h.Store.Agents()
	default:
		agents, err = h.Store.LiveAgents()
	}
	if err != nil {
		httpError(w, err)
		return
	}
	if agents == nil {
		agents = []store.Agent{}
	}
	writeJSON(w, http.StatusOK, agents)
}

func (h *Host) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := h.Store.Agent(r.PathValue("agent"))
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// HostView is the host's settings, its load and the newest event id. Busy
// counts the agents working, Idle the live ones waiting at a prompt. A
// client reads it before its snapshot and streams events after
// LastEventID; the events that land between replay harmlessly.
type HostView struct {
	MaxAgents   int   `json:"max_agents"`
	Busy        int   `json:"busy"`
	Idle        int   `json:"idle"`
	LastEventID int64 `json:"last_event_id"`
}

func (h *Host) getHost(w http.ResponseWriter, r *http.Request) {
	last, err := h.Store.LastEventID()
	if err != nil {
		httpError(w, err)
		return
	}
	v := HostView{MaxAgents: h.maxAgents(), LastEventID: last}
	live, err := h.Store.LiveAgents()
	if err != nil {
		httpError(w, err)
		return
	}
	for _, a := range live {
		switch a.State {
		case store.StateBusy:
			v.Busy++
		case store.StateIdle:
			v.Idle++
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func httpError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, store.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, store.ErrFinished), errors.Is(err, store.ErrNotFinished), errors.Is(err, ErrLive), errors.Is(err, ErrEnding):
		code = http.StatusConflict
	case errors.Is(err, ErrGone), errors.Is(err, ErrDirGone):
		code = http.StatusGone
	case errors.Is(err, ErrUnknownAccount):
		code = http.StatusBadRequest
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
