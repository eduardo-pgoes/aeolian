package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/eduardo-pgoes/aeolian/internal/agent"
	"github.com/eduardo-pgoes/aeolian/internal/store"
	"github.com/eduardo-pgoes/aeolian/internal/workflow"
)

// An arena is a tournament the host runs by itself, with no lead agent:
// proposers with different priors answer one question, judges compare the
// anonymized answers pairwise, the host ranks them with Bradley-Terry, an
// evolver merges the top two, and arena.md tells the human where the judges
// agreed and where they split. The arena is a run with source arena, so it
// shows on the canvas and lands in the inbox like any other.

// SourceArena is the source of every arena's run.
const SourceArena = "arena"

const (
	DefaultArenaEntrants = 4
	DefaultArenaJudges   = 3
	// DefaultArenaTimeout is how long one arena agent may take from its
	// start to its done before it forfeits.
	DefaultArenaTimeout = 25 * time.Minute

	DefaultArenaModelPropose = "opus"
	DefaultArenaModelJudge   = "sonnet"
	DefaultArenaModelEvolve  = "opus"

	maxArenaJudges = 9
	// maxArenaEntrants keeps labels within A-H and a judge at 28 pairs.
	maxArenaEntrants = 8
	// arenaInlineCap bounds what an arena puts inline in one agent's prompt
	// from the request: question, context paths, rubric, the longest prior
	// and the longest persona. The prompt goes through tmux's command line,
	// which refuses about 16KB; the fixed text takes the rest.
	arenaInlineCap = 8 << 10
	// arenaWordCap is the proposal length proposers are told to keep under;
	// the host cuts a proposal at half again as much before judges see it.
	arenaWordCap = 900

	ArenaRoleProposer = "proposer"
	ArenaRoleJudge    = "judge"
	ArenaRoleEvolver  = "evolver"

	// FinalEvolver ends an arena with an evolver's synthesis of the top two;
	// FinalHuman stops after the ranking and leaves the pick to the user.
	FinalEvolver = "evolver"
	FinalHuman   = "human"
)

// Arena states. The run keeps the usual run states; these say which phase
// the tournament is in.
const (
	ArenaProposing = "proposing"
	ArenaJudging   = "judging"
	ArenaEvolving  = "evolving"
	ArenaDone      = "done"
	ArenaFailed    = "failed"
	ArenaKilled    = "killed"
)

// Entrant and judge states.
const (
	seatPending = "pending"
	seatBusy    = "busy"
	seatIn      = "in"
	seatForfeit = "forfeit"
	seatKilled  = "killed"
)

// Prior is the lens one proposer argues from. Judges never see it.
type Prior struct {
	Name string `json:"name" yaml:"name"`
	Text string `json:"text" yaml:"text"`
}

// DefaultPriors are the four lenses an arena uses unless told otherwise.
var DefaultPriors = []Prior{
	{Name: "cost-first", Text: "Optimize for the cheapest path to a real result: the fewest agent-hours, the least new code, the least to maintain afterwards. Prefer reusing what already exists, and say plainly what you would refuse to build."},
	{Name: "simplicity-first", Text: "Optimize for the smallest, most legible design one person can hold in their head and debug at 2am. Prefer deleting and narrowing over adding; every new concept has to pay rent."},
	{Name: "frontier-first", Text: "Optimize for capability: what becomes possible that was not before, borrowing from the current research and tooling frontier. Accept more risk and cost for a step change, and say which risks."},
	{Name: "adoption-first", Text: "Optimize for adoption and politics: what other people will actually use, fund or approve, who has to say yes and what they need to see, and how it lands in an organization that deprioritizes by default."},
}

// ArenaRequest starts an arena. Every field but Question is optional.
type ArenaRequest struct {
	Question string `json:"question"`
	// Context lists absolute paths, files or folders, every agent is told to
	// read.
	Context  []string `json:"context,omitempty"`
	Entrants int      `json:"entrants,omitempty"`
	Judges   int      `json:"judges,omitempty"`
	// Priors replace DefaultPriors; Entrants takes the first ones.
	Priors       []Prior `json:"priors,omitempty"`
	ModelPropose string  `json:"model_propose,omitempty"`
	ModelJudge   string  `json:"model_judge,omitempty"`
	ModelEvolve  string  `json:"model_evolve,omitempty"`
	// Timeout is a Go duration per agent, DefaultArenaTimeout when empty.
	Timeout  string `json:"timeout,omitempty"`
	NoEvolve bool   `json:"no_evolve,omitempty"`
	// Personas make the judges an audience: judge j reads as persona
	// j mod len(Personas). Judges defaults to len(Personas) when set.
	Personas []Persona `json:"personas,omitempty"`
	// Rubric lines are added to every judge's rubric.
	Rubric []string `json:"rubric,omitempty"`
	// Final is FinalEvolver, the default, or FinalHuman.
	Final   string `json:"final,omitempty"`
	Account string `json:"account,omitempty"`
}

// Arena is an arena as the API shows it; it is also arena.json. Its ID is
// its run's.
type Arena struct {
	ID        string          `json:"id"`
	Question  string          `json:"question"`
	Context   []string        `json:"context"`
	State     string          `json:"state"`
	Note      string          `json:"note,omitempty"`
	Final     string          `json:"final"`
	Dir       string          `json:"dir"`
	Report    string          `json:"report,omitempty"`
	Synthesis string          `json:"synthesis,omitempty"`
	Entrants  []Entrant       `json:"entrants"`
	Judges    []ArenaJudge    `json:"judges"`
	Judgments []Judgment      `json:"judgments"`
	Standings []ArenaStanding `json:"standings"`
	Splits    []ArenaSplit    `json:"splits"`
	CreatedAt time.Time       `json:"created_at"`
	DoneAt    *time.Time      `json:"done_at,omitempty"`
}

// Entrant is one proposer's seat. Label is its anonymous name in judging,
// set once the proposals are in.
type Entrant struct {
	Index    int    `json:"index"`
	Prior    string `json:"prior"`
	Label    string `json:"label,omitempty"`
	AgentID  string `json:"agent_id,omitempty"`
	State    string `json:"state"`
	Note     string `json:"note,omitempty"`
	Proposal string `json:"proposal,omitempty"`
	Words    int    `json:"words,omitempty"`
}

type ArenaJudge struct {
	Index   int    `json:"index"`
	Persona string `json:"persona,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	State   string `json:"state"`
	Note    string `json:"note,omitempty"`
	Pairs   int    `json:"pairs"`
	Judged  int    `json:"judged"`
}

// Judgment is one judge's verdict on one pair, by label.
type Judgment struct {
	Judge  int       `json:"judge"`
	A      string    `json:"a"`
	B      string    `json:"b"`
	Winner string    `json:"winner"`
	Margin int       `json:"margin"`
	Why    string    `json:"why"`
	Steal  string    `json:"steal,omitempty"`
	At     time.Time `json:"at"`
}

type ArenaStanding struct {
	Rank   int     `json:"rank"`
	Label  string  `json:"label"`
	Prior  string  `json:"prior"`
	Score  float64 `json:"score"`
	Wins   int     `json:"wins"`
	Losses int     `json:"losses"`
	Margin int     `json:"margin"`
	Words  int     `json:"words"`
	// Tie is set when this entry equals the one ranked above it on
	// strength, wins and margin: their order is by label, which the arena
	// drew at random.
	Tie bool `json:"tie,omitempty"`
}

type ArenaSplit struct {
	A    string   `json:"a"`
	B    string   `json:"b"`
	ForA int      `json:"for_a"`
	ForB int      `json:"for_b"`
	Why  []string `json:"why"`
}

// arenaRun is a live arena. mu guards everything below it; it is never held
// while calling into the host's launch, kill or store-writing paths that
// take h.mu, apart from the file writes of save.
type arenaRun struct {
	h        *Host
	req      ArenaRequest
	priors   []Prior
	timeout  time.Duration
	stepBase struct{ judge, evolve int }

	mu        sync.Mutex
	a         Arena
	proposals map[int]string // entrant index -> text, as judges see it
	raw       map[int]string // entrant index -> text as handed back, not yet on disk
	// revealed is set when judging ends. Until then arena.json and the API
	// hide each entrant's label and word count, and the proposals stay off
	// disk, so nothing a judge could stumble on ties a label to a lens.
	revealed bool
	byLabel  map[string]int // label -> entrant index
	pairs    map[[2]string]bool
	judged   map[int]map[[2]string]bool // judge index -> pairs judged
}

// arenaBook is every arena a host knows of, and which agent judges which.
type arenaBook struct {
	mu     sync.Mutex
	arenas map[string]*arenaRun
	judges map[string]*arenaRun // judge agent id -> its arena
}

// arenaBooks holds one book per host. It lives outside Host so the arena
// stays in its own files.
var arenaBooks sync.Map // *Host -> *arenaBook

func (h *Host) arenas() *arenaBook {
	if b, ok := arenaBooks.Load(h); ok {
		return b.(*arenaBook)
	}
	b, _ := arenaBooks.LoadOrStore(h, &arenaBook{arenas: map[string]*arenaRun{}, judges: map[string]*arenaRun{}})
	return b.(*arenaBook)
}

func (b *arenaBook) arena(id string) *arenaRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.arenas[id]
}

func (b *arenaBook) judgeOf(agentID string) *arenaRun {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.judges[agentID]
}

func (h *Host) arenasDir() string { return filepath.Join(h.Layout.Home, "arenas") }

// StartArena stores the arena's run and starts the tournament in the
// background. It does not wait for slots: the proposers queue under
// max_agents like any other start.
func (h *Host) StartArena(req ArenaRequest) (Arena, error) {
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		return Arena{}, errors.New("empty question")
	}
	priors := req.Priors
	if len(priors) == 0 {
		priors = DefaultPriors
	}
	if req.Entrants == 0 {
		req.Entrants = min(DefaultArenaEntrants, len(priors))
	}
	if req.Entrants > maxArenaEntrants {
		return Arena{}, fmt.Errorf("entrants is %d; an arena takes at most %d (each judge already compares %d pairs)", req.Entrants, maxArenaEntrants, maxArenaEntrants*(maxArenaEntrants-1)/2)
	}
	if req.Entrants < 2 || req.Entrants > len(priors) {
		return Arena{}, fmt.Errorf("entrants must be between 2 and the %d priors", len(priors))
	}
	priors = priors[:req.Entrants]
	seen := map[string]bool{}
	for i, p := range priors {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Text) == "" {
			return Arena{}, fmt.Errorf("prior %d needs a name and a text", i+1)
		}
		if seen[p.Name] {
			return Arena{}, fmt.Errorf("two priors named %q", p.Name)
		}
		seen[p.Name] = true
	}
	for i, p := range req.Personas {
		if err := p.check(); err != nil {
			return Arena{}, fmt.Errorf("persona %d: %w", i+1, err)
		}
	}
	if req.Judges == 0 {
		req.Judges = DefaultArenaJudges
		if len(req.Personas) > 0 {
			req.Judges = min(len(req.Personas), maxArenaJudges)
		}
	}
	switch req.Final {
	case "":
		req.Final = FinalEvolver
	case FinalEvolver, FinalHuman:
	default:
		return Arena{}, fmt.Errorf("final %q is neither %s nor %s", req.Final, FinalEvolver, FinalHuman)
	}
	if req.Final == FinalHuman {
		req.NoEvolve = true
	}
	if req.Judges < 1 || req.Judges > maxArenaJudges {
		return Arena{}, fmt.Errorf("judges must be between 1 and %d", maxArenaJudges)
	}
	for _, p := range req.Context {
		if !filepath.IsAbs(p) {
			return Arena{}, fmt.Errorf("context %q is not an absolute path", p)
		}
		if _, err := os.Stat(p); err != nil {
			return Arena{}, fmt.Errorf("context: %w", err)
		}
	}
	if n := inlineSize(req, priors); n > arenaInlineCap {
		return Arena{}, fmt.Errorf("the question, context, rubric, longest prior and longest persona come to %d bytes, past the %d an agent's prompt can carry; put the long part in a file and pass it with context", n, arenaInlineCap)
	}
	account, err := h.pickAccount(req.Account)
	if err != nil {
		return Arena{}, err
	}
	timeout := DefaultArenaTimeout
	if req.Timeout != "" {
		d, err := time.ParseDuration(req.Timeout)
		if err != nil || d <= 0 {
			return Arena{}, fmt.Errorf("timeout %q is not a positive duration", req.Timeout)
		}
		timeout = d
	}
	req.ModelPropose = or(req.ModelPropose, DefaultArenaModelPropose)
	req.ModelJudge = or(req.ModelJudge, DefaultArenaModelJudge)
	req.ModelEvolve = or(req.ModelEvolve, DefaultArenaModelEvolve)

	id := agent.NewSessionID()
	ar := &arenaRun{h: h, req: req, priors: priors, timeout: timeout,
		proposals: map[int]string{}, raw: map[int]string{}, byLabel: map[string]int{}, pairs: map[[2]string]bool{}, judged: map[int]map[[2]string]bool{}}
	ar.stepBase.judge = len(priors)
	ar.stepBase.evolve = len(priors) + req.Judges
	src, steps, err := arenaWorkflow(req, priors)
	if err != nil {
		return Arena{}, err
	}
	now := time.Now()
	ar.a = Arena{
		ID: id, Question: req.Question, Context: append([]string{}, req.Context...), State: ArenaProposing, Final: req.Final,
		Dir: filepath.Join(h.arenasDir(), id), CreatedAt: now,
		Judgments: []Judgment{}, Standings: []ArenaStanding{}, Splits: []ArenaSplit{},
	}
	for i, p := range priors {
		ar.a.Entrants = append(ar.a.Entrants, Entrant{Index: i, Prior: p.Name, State: seatPending})
	}
	for j := range req.Judges {
		jd := ArenaJudge{Index: j, State: seatPending}
		if p, ok := ar.persona(j); ok {
			jd.Persona = p.Name()
		}
		ar.a.Judges = append(ar.a.Judges, jd)
	}
	run := store.Run{ID: id, Goal: req.Question, Workflow: src, Source: SourceArena, Account: account, State: store.StateBusy, CreatedAt: now}
	for _, name := range steps {
		run.Steps = append(run.Steps, store.Step{Name: name})
	}
	if err := h.createArenaRun(run, ar.a.Dir); err != nil {
		return Arena{}, err
	}
	h.emit(id, "", store.KindArenaStarted, store.ArenaStarted{V: store.EventVersion, Entrants: len(priors), Judges: req.Judges, Final: req.Final})
	book := h.arenas()
	book.mu.Lock()
	book.arenas[id] = ar
	book.mu.Unlock()
	ar.mu.Lock()
	ar.saveLocked()
	snap := ar.snapshotLocked()
	ar.mu.Unlock()
	go ar.run()
	return snap, nil
}

// inlineSize is how much of req and priors goes inline in the largest
// arena prompt.
func inlineSize(req ArenaRequest, priors []Prior) int {
	n := len(req.Question)
	for _, c := range req.Context {
		n += len(c)
	}
	for _, r := range req.Rubric {
		n += len(r)
	}
	longest := 0
	for _, p := range priors {
		longest = max(longest, len(p.Name)+len(p.Text))
	}
	for _, p := range req.Personas {
		m := len(p.ID) + len(p.Role) + len(p.Briefing) + len(p.TieBreak)
		for _, x := range append(slices.Clone(p.OptimizesFor), p.SaysNoWhen...) {
			m += len(x)
		}
		longest = max(longest, m)
	}
	return n + longest
}

// ErrNoRunSlot is StartArena's refusal when the sourced runs busy already
// reach max_runs.
var ErrNoRunSlot = errors.New("no run slot free")

// createArenaRun stores an arena's run busy if a run slot is free under
// max_runs, the same cap createRun keeps, and refuses it otherwise: an arena
// has no workflow step AdmitRuns could start later, so it never queues.
func (h *Host) createArenaRun(run store.Run, dir string) error {
	h.admitting.Lock()
	defer h.admitting.Unlock()
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		return err
	}
	busy := busyByLane(runs, h.MaxRunsBySource)
	if lane := h.lane(run.Source); !h.laneFree(busy, lane) {
		return fmt.Errorf("an arena needs a run slot and %d sourced run(s) are busy in its lane; try again when one ends: %w", busy[lane], ErrNoRunSlot)
	}
	if err := os.MkdirAll(filepath.Join(dir, "proposals"), 0o755); err != nil {
		return err
	}
	return h.Store.CreateRun(run)
}

// CloseOrphanArenas fails every busy arena run with no live arena behind it:
// the host restarted while it ran, and nothing will finish it. Left busy, it
// would hold a run slot forever. Its agents' sessions outlive the host, so
// they are killed too. Call it once when the host starts, before AdmitRuns.
func (h *Host) CloseOrphanArenas() {
	runs, err := h.Store.Runs(store.RunFilter{})
	if err != nil {
		log.Printf("close orphan arenas: %v", err)
		return
	}
	agents, err := h.Store.Agents()
	if err != nil {
		log.Printf("close orphan arenas: %v", err)
		return
	}
	for _, r := range runs {
		if r.Source != SourceArena || r.State != store.StateBusy || h.arenas().arena(r.ID) != nil {
			continue
		}
		for _, ag := range agents {
			if ag.RunID == r.ID && !store.Finished(ag.State) {
				if _, err := h.Kill(ag.ID); err != nil && !errors.Is(err, store.ErrFinished) {
					log.Printf("arena %s: kill %s: %v", r.ID, ag.ID, err)
				}
			}
		}
		ar := &arenaRun{h: h, byLabel: map[string]int{}, raw: map[int]string{}, revealed: true}
		ar.a = Arena{ID: r.ID, Question: r.Goal, Dir: filepath.Join(h.arenasDir(), r.ID), CreatedAt: r.CreatedAt}
		if b, err := os.ReadFile(filepath.Join(ar.a.Dir, "arena.json")); err == nil {
			json.Unmarshal(b, &ar.a)
		}
		for _, e := range ar.a.Entrants {
			if e.Label != "" {
				ar.byLabel[e.Label] = e.Index
			}
			if e.State != seatIn || e.Proposal != "" || e.AgentID == "" {
				continue
			}
			if ag, err := h.Store.Agent(e.AgentID); err == nil && ag.Handoff != nil {
				ar.raw[e.Index] = strings.TrimSpace(ag.Handoff.Text)
			}
		}
		if err := os.MkdirAll(filepath.Join(ar.a.Dir, "proposals"), 0o755); err != nil {
			log.Printf("arena %s: %v", r.ID, err)
		}
		ar.stop(ArenaFailed, arenaReasonOrphaned, "the host restarted while this arena ran; it was closed when the host came back")
	}
}

// persona is the audience judge j reads as, if the arena has personas.
func (ar *arenaRun) persona(j int) (Persona, bool) {
	if len(ar.req.Personas) == 0 {
		return Persona{}, false
	}
	return ar.req.Personas[j%len(ar.req.Personas)], true
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// arenaWorkflow is the workflow stored on the arena's run. Nothing starts
// from its prompts; it is there so the canvas can name and colour the
// arena's agents and so launch finds each role's model.
func arenaWorkflow(req ArenaRequest, priors []Prior) (string, []string, error) {
	const placeholder = "Started by the arena host; the agent's own prompt says what to do."
	wf := workflow.Workflow{
		Description: "arena: " + req.Question,
		Roles: map[string]workflow.Role{
			ArenaRoleProposer: {Prompt: placeholder, Ink: "sakura", Mark: "propose", Model: req.ModelPropose, Headless: true},
			ArenaRoleJudge:    {Prompt: placeholder, Ink: "sky", Mark: "judge", Model: req.ModelJudge, Headless: true},
			ArenaRoleEvolver:  {Prompt: placeholder, Ink: "matcha", Mark: "evolve", Model: req.ModelEvolve, Headless: true},
		},
	}
	for i, p := range priors {
		wf.Steps = append(wf.Steps, workflow.Step{Name: fmt.Sprintf("propose-%d", i+1), Prompt: placeholder, Ink: "sakura", Mark: p.Name, Headless: true})
	}
	for j := range req.Judges {
		wf.Steps = append(wf.Steps, workflow.Step{Name: fmt.Sprintf("judge-%d", j+1), Prompt: placeholder, Ink: "sky", Mark: "judge", Headless: true})
	}
	if !req.NoEvolve {
		wf.Steps = append(wf.Steps, workflow.Step{Name: "evolve", Prompt: placeholder, Ink: "matcha", Mark: "evolve", Headless: true})
	}
	b, err := yaml.Marshal(wf)
	if err != nil {
		return "", nil, err
	}
	if _, err := workflow.Parse(b); err != nil {
		return "", nil, fmt.Errorf("arena workflow: %w", err)
	}
	var steps []string
	for _, st := range wf.Steps {
		steps = append(steps, st.Name)
	}
	return string(b), steps, nil
}

// parent is the ParentID of every arena agent. No agent has it, so a done
// is delivered to the arena's own wait and never typed anywhere, and an
// arena agent never advances the run as a step's lead would.
func (ar *arenaRun) parent() string { return arenaParentPrefix + ar.a.ID }

const arenaParentPrefix = "arena:"

// Reasons an arena's run or step fails for, beside the store's own.
const (
	// arenaReasonTooFew: fewer than two proposals came in.
	arenaReasonTooFew = "too_few_entries"
	// arenaReasonNoJudgments: judging ended with no judgment recorded.
	arenaReasonNoJudgments = "no_judgments"
	// arenaReasonTimedOut: a seat's agent ran past the arena's timeout.
	arenaReasonTimedOut = "timed_out"
	// arenaReasonOrphaned: the host restarted while the arena ran.
	arenaReasonOrphaned = "orphaned"
)

// isArenaAgent says whether a is a proposer, judge or evolver of an arena.
// Its done goes to the arena's wait; there is no parent agent to tell.
func isArenaAgent(a store.Agent) bool { return strings.HasPrefix(a.ParentID, arenaParentPrefix) }

// run walks the phases. Any phase that sees the run's context end stops the
// arena as killed: KillRun has closed the run already.
func (ar *arenaRun) run() {
	h := ar.h
	ctx := h.runContext(ar.a.ID)

	var wg sync.WaitGroup
	for i := range ar.priors {
		wg.Go(func() { ar.propose(ctx, i) })
	}
	wg.Wait()
	if ctx.Err() != nil {
		ar.stop(ArenaKilled, store.ReasonKilled, "killed while proposing")
		return
	}
	survivors := ar.survivors()
	if len(survivors) < 2 {
		ar.stop(ArenaFailed, arenaReasonTooFew, fmt.Sprintf("only %d proposal(s) came in; an arena needs two", len(survivors)))
		return
	}
	ar.assignLabels(survivors)

	ar.setState(ArenaJudging)
	for j := range ar.req.Judges {
		wg.Go(func() { ar.judge(ctx, j) })
	}
	wg.Wait()
	if ctx.Err() != nil {
		ar.stop(ArenaKilled, store.ReasonKilled, "killed while judging")
		return
	}
	ar.mu.Lock()
	ar.revealed = true
	ar.writeProposalsLocked()
	ar.rankLocked()
	none := len(ar.a.Judgments) == 0
	ar.mu.Unlock()
	if none {
		ar.stop(ArenaFailed, arenaReasonNoJudgments, "no judgment came in")
		return
	}

	if !ar.req.NoEvolve {
		ar.setState(ArenaEvolving)
		ar.evolve(ctx)
		if ctx.Err() != nil {
			ar.stop(ArenaKilled, store.ReasonKilled, "killed while evolving")
			return
		}
	}
	ar.stop(ArenaDone, store.ReasonDelivered, "")
}

// stop writes the report and ends the arena and, unless it was killed, its
// run, for reason.
func (ar *arenaRun) stop(state, reason, note string) {
	h := ar.h
	now := time.Now()
	ar.mu.Lock()
	ar.a.State, ar.a.DoneAt = state, &now
	if note != "" {
		ar.a.Note = note
	}
	for i := range ar.a.Entrants {
		if e := &ar.a.Entrants[i]; e.State == seatPending || e.State == seatBusy {
			e.State = seatKilled
		}
	}
	for i := range ar.a.Judges {
		if j := &ar.a.Judges[i]; j.State == seatPending || j.State == seatBusy {
			j.State = seatKilled
		}
	}
	ar.revealed = true
	ar.writeProposalsLocked()
	ar.a.Report = filepath.Join(ar.a.Dir, "arena.md")
	if err := os.WriteFile(ar.a.Report, []byte(ar.reportLocked()), 0o644); err != nil {
		log.Printf("arena %s: %v", ar.a.ID, err)
		ar.a.Report = ""
	}
	ar.saveLocked()
	ar.mu.Unlock()
	if note != "" {
		log.Printf("arena %s: %s: %s", ar.a.ID, state, note)
	}
	h.emit(ar.a.ID, "", store.KindArenaFinished, store.ArenaFinished{V: store.EventVersion, State: state, Reason: reason, Note: note})
	defer ar.forget()
	if state == ArenaKilled {
		return
	}
	runState := store.StateDone
	if state == ArenaFailed {
		runState = store.StateFailed
	}
	err := h.finishRun(ar.a.ID, store.End{State: runState, Reason: reason, At: now})
	switch {
	case errors.Is(err, store.ErrFinished):
		// KillRun got there first, after the last phase checked for it:
		// the arena ends as its run did.
		if run, rerr := h.Store.Run(ar.a.ID); rerr == nil && run.State == store.StateKilled {
			ar.mu.Lock()
			ar.a.State, ar.a.Note = ArenaKilled, "killed as it finished"
			if ar.a.Report != "" {
				os.WriteFile(ar.a.Report, []byte(ar.reportLocked()), 0o644)
			}
			ar.saveLocked()
			ar.mu.Unlock()
		}
	case err != nil:
		log.Printf("arena %s: %v", ar.a.ID, err)
	}
}

// forget drops a finished arena from the host's book; Arena reads it from
// arena.json from then on, and its proposals no longer sit in memory.
func (ar *arenaRun) forget() {
	book := ar.h.arenas()
	book.mu.Lock()
	defer book.mu.Unlock()
	if book.arenas[ar.a.ID] == ar {
		delete(book.arenas, ar.a.ID)
	}
	for id, x := range book.judges {
		if x == ar {
			delete(book.judges, id)
		}
	}
}

// writeProposalsLocked puts every proposal not yet on disk into the
// arena's proposals folder.
func (ar *arenaRun) writeProposalsLocked() {
	for i, text := range ar.raw {
		e := &ar.a.Entrants[i]
		path := filepath.Join(ar.a.Dir, "proposals", fmt.Sprintf("%d-%s.md", i+1, slug(e.Prior)))
		if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
			log.Printf("arena %s: %v", ar.a.ID, err)
			continue
		}
		e.Proposal = path
		delete(ar.raw, i)
	}
}

func (ar *arenaRun) setState(state string) {
	ar.mu.Lock()
	ar.a.State = state
	ar.saveLocked()
	ar.mu.Unlock()
	ar.h.emit(ar.a.ID, "", store.KindArenaPhase, store.ArenaPhase{V: store.EventVersion, Phase: state})
}

// outcome is how one arena agent ended.
type outcome struct {
	text   string
	ok     bool
	killed bool
	note   string
}

// runAgent launches one arena agent as step and waits for its done, its
// session's end, the timeout or the run's end, whichever is first. The
// timeout counts from the start, not from the queue: a seat waiting on
// max_agents does not forfeit. registered runs once the agent's id is known
// and before it can call any tool; expired, if set, runs when the timeout
// fires, before the agent is killed.
//
// build makes the prompt and the files to put in the agent's folder first.
// What the agent reads goes in files, not in the prompt: the prompt travels
// through tmux's command line, which refuses anything past about 16KB.
func (ar *arenaRun) runAgent(ctx context.Context, step int, role string, build func(dir string) (string, map[string]string), registered func(id string), expired func()) outcome {
	h := ar.h
	runID := ar.a.ID
	stepName := ar.stepName(step)
	id := agent.NewSessionID()
	dir := h.Layout.ScratchDir(id)
	prompt, files := build(dir)
	for name, text := range files {
		path := filepath.Join(dir, name)
		err := os.MkdirAll(filepath.Dir(path), 0o755)
		if err == nil {
			err = os.WriteFile(path, []byte(text), 0o644)
		}
		if err != nil {
			ar.finishStep(step, store.End{State: store.StateFailed, Reason: store.ReasonStartFailed})
			return outcome{note: "could not write its files: " + err.Error()}
		}
	}
	registered(id)
	w := h.wait(id, ar.parent(), role)
	a, err := h.launch(ctx, store.Agent{
		ID: id, Dir: dir, Prompt: prompt, ParentID: ar.parent(), Role: role,
		RunID: runID, Step: stepName, Headless: true,
	})
	if err != nil {
		h.stopWaiting(id)
		if ctx.Err() != nil {
			return outcome{killed: true, note: "killed before it started"}
		}
		ar.finishStep(step, store.End{State: store.StateFailed, Reason: store.ReasonStartFailed})
		return outcome{note: "could not start: " + err.Error()}
	}
	if err := h.Store.StartStep(runID, step, id, a.CreatedAt); err != nil {
		log.Printf("arena %s step %s: %v", runID, stepName, err)
	}
	h.closeStepIfRunClosed(runID, step)

	timer := time.NewTimer(ar.timeout)
	defer timer.Stop()
	var got store.Handoff
	select {
	case got = <-w.handoff:
	case <-timer.C:
		if h.stopWaiting(id) {
			if expired != nil {
				expired()
			}
			if _, err := h.Kill(id); err != nil && !errors.Is(err, store.ErrFinished) {
				log.Printf("arena %s: kill %s after its timeout: %v", runID, id, err)
			}
			ar.finishStep(step, store.End{State: store.StateFailed, Reason: arenaReasonTimedOut})
			return outcome{note: fmt.Sprintf("timed out after %s", ar.timeout)}
		}
		got = <-w.handoff
	case <-ctx.Done():
		h.stopWaiting(id)
		return outcome{killed: true, note: "killed"}
	}
	if ctx.Err() != nil {
		return outcome{killed: true, note: "killed"}
	}
	cur, err := h.Store.Agent(id)
	if err != nil || cur.State != store.StateDone {
		// The agent's own reason says how it ended: exited, lost, killed.
		ar.finishStep(step, store.End{State: store.StateFailed, Reason: or(cur.Reason, store.ReasonExited)})
		return outcome{note: "ended without calling done: " + got.Text}
	}
	ar.finishStep(step, store.End{State: store.StateDone, Reason: store.ReasonDelivered})
	return outcome{text: got.Text, ok: true}
}

func (ar *arenaRun) stepName(step int) string {
	switch {
	case step < ar.stepBase.judge:
		return fmt.Sprintf("propose-%d", step+1)
	case step < ar.stepBase.evolve:
		return fmt.Sprintf("judge-%d", step-ar.stepBase.judge+1)
	}
	return "evolve"
}

func (ar *arenaRun) finishStep(step int, end store.End) {
	if end.At.IsZero() {
		end.At = time.Now()
	}
	err := ar.h.Store.FinishStep(ar.a.ID, step, end)
	if err != nil && !errors.Is(err, store.ErrFinished) {
		log.Printf("arena %s step %s: %v", ar.a.ID, ar.stepName(step), err)
	}
}

func (ar *arenaRun) propose(ctx context.Context, i int) {
	build := func(string) (string, map[string]string) { return ar.proposerPrompt(i), nil }
	out := ar.runAgent(ctx, i, ArenaRoleProposer, build, func(id string) {
		ar.mu.Lock()
		ar.a.Entrants[i].AgentID, ar.a.Entrants[i].State = id, seatBusy
		ar.saveLocked()
		ar.mu.Unlock()
	}, nil)
	ar.mu.Lock()
	defer ar.mu.Unlock()
	e := &ar.a.Entrants[i]
	switch {
	case out.killed:
		e.State, e.Note = seatKilled, out.note
	case !out.ok:
		e.State, e.Note = seatForfeit, out.note
	default:
		raw := strings.TrimSpace(out.text)
		e.State, e.Words = seatIn, len(strings.Fields(raw))
		if e.Words == 0 {
			e.State, e.Note = seatForfeit, "handed back an empty proposal"
			break
		}
		ar.raw[i] = raw
		ar.proposals[i] = ar.anonymize(raw)
	}
	ar.saveLocked()
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// anonymize strips every prior's name and text from a proposal and cuts it
// at half again the word cap, so judges cannot tell lenses apart by label
// or reward length past a point.
func (ar *arenaRun) anonymize(text string) string {
	for _, p := range ar.priors {
		text = strings.ReplaceAll(text, strings.TrimSpace(p.Text), "[lens]")
		words := strings.FieldsFunc(p.Name, func(r rune) bool { return r == '-' || r == '_' || r == ' ' || r == '/' })
		for k, w := range words {
			words[k] = regexp.QuoteMeta(w)
		}
		if len(words) == 0 {
			continue
		}
		re := regexp.MustCompile(`(?i)` + strings.Join(words, `[\s\-_/]*`))
		text = re.ReplaceAllString(text, "[lens]")
	}
	limit := arenaWordCap * 3 / 2
	return cutWords(text, limit, fmt.Sprintf("cut by the arena host at %d words", limit))
}

func (ar *arenaRun) survivors() []int {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var out []int
	for _, e := range ar.a.Entrants {
		if e.State == seatIn {
			out = append(out, e.Index)
		}
	}
	return out
}

// arenaRand is the arena's seeded randomness: the same arena id and salt
// always shuffle the same way, so a run can be explained after the fact.
func arenaRand(id string, salt int) *rand.Rand {
	f := fnv.New64a()
	f.Write([]byte(id + ":" + strconv.Itoa(salt)))
	s := f.Sum64()
	return rand.New(rand.NewPCG(s, s^0x9e3779b97f4a7c15))
}

func labelOf(k int) string { return string(rune('A' + k)) }

// assignLabels gives the surviving entrants labels A.. in a shuffled order
// that never matches entrant order.
func (ar *arenaRun) assignLabels(survivors []int) {
	r := arenaRand(ar.a.ID, -1)
	perm := r.Perm(len(survivors))
	identity := true
	for i, p := range perm {
		identity = identity && i == p
	}
	if identity && len(perm) > 1 {
		perm = append(perm[1:], perm[0])
	}
	ar.mu.Lock()
	defer ar.mu.Unlock()
	var labels []string
	for i, e := range survivors {
		l := labelOf(perm[i])
		ar.a.Entrants[e].Label = l
		ar.byLabel[l] = e
		labels = append(labels, l)
	}
	slices.Sort(labels)
	for x := range labels {
		for y := x + 1; y < len(labels); y++ {
			ar.pairs[[2]string{labels[x], labels[y]}] = true
		}
	}
	ar.saveLocked()
}

func pairKey(a, b string) [2]string {
	if b < a {
		a, b = b, a
	}
	return [2]string{a, b}
}

func (ar *arenaRun) judge(ctx context.Context, j int) {
	build := func(dir string) (string, map[string]string) { return ar.judgePrompt(j, dir) }
	out := ar.runAgent(ctx, ar.stepBase.judge+j, ArenaRoleJudge, build, func(id string) {
		ar.mu.Lock()
		ar.a.Judges[j].AgentID, ar.a.Judges[j].State = id, seatBusy
		ar.a.Judges[j].Pairs = len(ar.pairs)
		ar.judged[j] = map[[2]string]bool{}
		ar.saveLocked()
		ar.mu.Unlock()
		book := ar.h.arenas()
		book.mu.Lock()
		book.judges[id] = ar
		book.mu.Unlock()
	}, func() {
		// Forfeit before the kill, so a judgment already on its way in
		// is refused.
		ar.mu.Lock()
		ar.a.Judges[j].State = seatForfeit
		ar.mu.Unlock()
	})
	ar.mu.Lock()
	defer ar.mu.Unlock()
	jd := &ar.a.Judges[j]
	switch {
	case out.killed:
		jd.State, jd.Note = seatKilled, out.note
	case !out.ok:
		jd.State, jd.Note = seatForfeit, out.note
	default:
		jd.State = seatIn
		if jd.Judged < jd.Pairs {
			jd.Note = fmt.Sprintf("judged %d of %d pairs", jd.Judged, jd.Pairs)
		}
	}
	ar.saveLocked()
}

// JudgmentInput is what a judge hands the judgment tool for one pair.
type JudgmentInput struct {
	A      string `json:"a" jsonschema:"the first label of the pair, as listed"`
	B      string `json:"b" jsonschema:"the second label of the pair, as listed"`
	Winner string `json:"winner" jsonschema:"the label of the better entry, a or b"`
	Margin int    `json:"margin" jsonschema:"1 slight, 2 clear, 3 decisive"`
	Why    string `json:"why" jsonschema:"one or two sentences naming the difference that decided it"`
	Steal  string `json:"steal,omitempty" jsonschema:"the one idea from either entry most worth keeping in a synthesis; empty when there is none"`
}

// recordJudgment checks and stores one judge's verdict on one pair.
func (ar *arenaRun) recordJudgment(agentID string, in JudgmentInput) (string, error) {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	if ar.a.State != ArenaJudging || ar.revealed {
		return "", errors.New("judging has ended; your judgment was not recorded")
	}
	j := -1
	for _, jd := range ar.a.Judges {
		if jd.AgentID == agentID {
			j = jd.Index
		}
	}
	if j < 0 {
		return "", errors.New("you are not a judge of this arena")
	}
	if st := ar.a.Judges[j].State; st != seatBusy {
		return "", fmt.Errorf("your seat is %s; your judgment was not recorded", st)
	}
	a, b, winner := norm(in.A), norm(in.B), norm(in.Winner)
	if a == b {
		return "", fmt.Errorf("a and b are both %q; judge two different entries", a)
	}
	key := pairKey(a, b)
	if !ar.pairs[key] {
		return "", fmt.Errorf("%s vs %s is not a pair of this arena; judge only the pairs listed", a, b)
	}
	if ar.judged[j][key] {
		return "", fmt.Errorf("you already judged %s vs %s; each pair is judged once", a, b)
	}
	if winner != a && winner != b {
		return "", fmt.Errorf("winner %q is neither %s nor %s", in.Winner, a, b)
	}
	if in.Margin < 1 || in.Margin > 3 {
		return "", fmt.Errorf("margin %d is not 1, 2 or 3", in.Margin)
	}
	if strings.TrimSpace(in.Why) == "" {
		return "", errors.New("why is empty; name the difference that decided it")
	}
	ar.judged[j][key] = true
	ar.a.Judgments = append(ar.a.Judgments, Judgment{
		Judge: j, A: a, B: b, Winner: winner, Margin: in.Margin,
		Why: strings.TrimSpace(in.Why), Steal: strings.TrimSpace(in.Steal), At: time.Now(),
	})
	ar.a.Judges[j].Judged++
	ar.saveLocked()
	left := len(ar.pairs) - ar.a.Judges[j].Judged
	if left == 0 {
		return fmt.Sprintf("Recorded %s over %s. Every pair is judged; call done now.", winner, other(winner, a, b)), nil
	}
	return fmt.Sprintf("Recorded %s over %s. %d pair(s) left.", winner, other(winner, a, b), left), nil
}

func norm(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

func other(x, a, b string) string {
	if x == a {
		return b
	}
	return a
}

// rankLocked turns the judgments into standings and splits.
func (ar *arenaRun) rankLocked() {
	// Entrants are numbered in label order, not entrant order: labels are
	// a seeded shuffle, so a full tie falls to a draw fixed by the arena's
	// id instead of always to the first prior.
	var order []int // compact index -> entrant index
	compact := map[int]int{}
	labels := slices.Sorted(maps.Keys(ar.byLabel))
	for _, l := range labels {
		e := ar.byLabel[l]
		compact[e] = len(order)
		order = append(order, e)
	}
	var games []Game
	for _, jm := range ar.a.Judgments {
		w, l := ar.byLabel[jm.Winner], ar.byLabel[other(jm.Winner, jm.A, jm.B)]
		games = append(games, Game{Winner: compact[w], Loser: compact[l], Margin: jm.Margin})
	}
	ar.a.Standings = ar.a.Standings[:0]
	ranked := Rank(len(order), games)
	for k, s := range ranked {
		e := ar.a.Entrants[order[s.Entrant]]
		ar.a.Standings = append(ar.a.Standings, ArenaStanding{
			Rank: k + 1, Label: e.Label, Prior: e.Prior, Score: s.Score,
			Wins: s.Wins, Losses: s.Losses, Margin: s.Margin, Words: e.Words,
			Tie: k > 0 && Tied(ranked[k-1], s),
		})
	}
	ar.a.Splits = ar.a.Splits[:0]
	for _, s := range Splits(games) {
		la, lb := ar.a.Entrants[order[s.A]].Label, ar.a.Entrants[order[s.B]].Label
		sp := ArenaSplit{A: la, B: lb, ForA: s.ForA, ForB: s.ForB}
		if lb < la {
			sp = ArenaSplit{A: lb, B: la, ForA: s.ForB, ForB: s.ForA}
		}
		for _, jm := range ar.a.Judgments {
			if pairKey(jm.A, jm.B) == pairKey(la, lb) {
				sp.Why = append(sp.Why, fmt.Sprintf("judge %d picked %s (margin %d): %s", jm.Judge+1, jm.Winner, jm.Margin, jm.Why))
			}
		}
		ar.a.Splits = append(ar.a.Splits, sp)
	}
	slices.SortFunc(ar.a.Splits, func(x, y ArenaSplit) int { return strings.Compare(x.A+x.B, y.A+y.B) })
	ar.saveLocked()
}

func (ar *arenaRun) evolve(ctx context.Context) {
	out := ar.runAgent(ctx, ar.stepBase.evolve, ArenaRoleEvolver, ar.evolverPrompt, func(string) {}, nil)
	if !out.ok {
		ar.mu.Lock()
		if !out.killed {
			ar.a.Note = "evolver: " + out.note
		}
		ar.saveLocked()
		ar.mu.Unlock()
		return
	}
	path := filepath.Join(ar.a.Dir, "synthesis.md")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(out.text)+"\n"), 0o644); err != nil {
		log.Printf("arena %s: %v", ar.a.ID, err)
		return
	}
	ar.mu.Lock()
	ar.a.Synthesis = path
	ar.saveLocked()
	ar.mu.Unlock()
}

// saveLocked writes arena.json; a failed write is logged, the arena goes on.
func (ar *arenaRun) saveLocked() {
	b, err := json.MarshalIndent(ar.publicLocked(), "", "  ")
	if err == nil {
		tmp := filepath.Join(ar.a.Dir, "arena.json.tmp")
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, filepath.Join(ar.a.Dir, "arena.json"))
		}
	}
	if err != nil {
		log.Printf("arena %s: save: %v", ar.a.ID, err)
	}
}

// publicLocked is the arena as arena.json and the API show it: before
// judging ends, without the labels and word counts that would tie an
// anonymous entry to its lens.
func (ar *arenaRun) publicLocked() Arena {
	if ar.revealed {
		return ar.a
	}
	a := ar.a
	a.Entrants = slices.Clone(a.Entrants)
	for i := range a.Entrants {
		a.Entrants[i].Label, a.Entrants[i].Words, a.Entrants[i].Proposal = "", 0, ""
	}
	return a
}

func (ar *arenaRun) snapshot() Arena {
	ar.mu.Lock()
	defer ar.mu.Unlock()
	return ar.snapshotLocked()
}

func (ar *arenaRun) snapshotLocked() Arena {
	b, _ := json.Marshal(ar.publicLocked())
	var out Arena
	json.Unmarshal(b, &out)
	return out
}

// Arena returns the arena id, live or from its arena.json.
func (h *Host) Arena(id string) (Arena, error) {
	if ar := h.arenas().arena(id); ar != nil {
		return ar.snapshot(), nil
	}
	if strings.ContainsAny(id, `/\`) || id == "" || id == "." || id == ".." {
		return Arena{}, store.ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(h.arenasDir(), id, "arena.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Arena{}, store.ErrNotFound
	}
	if err != nil {
		return Arena{}, err
	}
	var a Arena
	if err := json.Unmarshal(b, &a); err != nil {
		return Arena{}, err
	}
	if a.DoneAt == nil {
		a.Note = "the host restarted while this arena ran; it will not go on, and the host closes it when it starts again"
	}
	return a, nil
}

// Arenas returns every arena on disk, newest first.
func (h *Host) Arenas() ([]Arena, error) {
	dirs, err := os.ReadDir(h.arenasDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Arena{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Arena{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		a, err := h.Arena(d.Name())
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	slices.SortFunc(out, func(x, y Arena) int { return y.CreatedAt.Compare(x.CreatedAt) })
	return out, nil
}
