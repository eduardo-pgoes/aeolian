// Package store keeps aeolian's state in SQLite. It is the only package that
// knows about GORM; everything else goes through Store.
package store

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	// ErrFinished is returned when finishing a step or run that is already
	// done, blocked, failed or killed.
	ErrFinished = errors.New("already finished")
	// ErrNotFinished is returned when acking a run that is still queued or
	// busy.
	ErrNotFinished = errors.New("not finished")
)

const (
	StatePending = "pending"
	StateBusy    = "busy"
	// StateQueued is a run waiting for the host to start its first step.
	StateQueued  = "queued"
	StateIdle    = "idle"
	StateDone    = "done"
	StateFailed  = "failed"
	StateBlocked = "blocked"
	// StateKilled is an agent, run or step stopped from outside before it
	// finished.
	StateKilled = "killed"
)

// Finished reports whether an agent in state has ended its session: by
// done, by being killed, or, for a headless agent, by exiting without done.
func Finished(state string) bool { return slices.Contains(agentFinished, state) }

var agentFinished = []string{StateDone, StateKilled, StateFailed}

// Reasons say why an agent, step or run stopped. The state keeps its coarse
// meaning, which outside consumers key on; the reason carries the finer
// truth beside it. A live row has no reason.
const (
	// ReasonDelivered: the agent called done; for a run, its last step did.
	ReasonDelivered = "delivered"
	// ReasonBlocked: the agent called done with outcome blocked.
	ReasonBlocked = "blocked"
	// ReasonEnded: the run was ended on purpose, by a person or its lead.
	ReasonEnded = "ended"
	// ReasonClosed: an interactive session vanished while idle, so a person
	// closed its terminal.
	ReasonClosed = "closed"
	// ReasonLost: an interactive session vanished while busy (crash, OOM,
	// kill -9).
	ReasonLost = "lost"
	// ReasonExited: a headless process exited without calling done.
	ReasonExited = "exited"
	ReasonOOM    = "oom"
	// ReasonKilled: stopped from outside by Kill or KillRun.
	ReasonKilled = "killed"
	// ReasonStartFailed: the agent or its step could not start.
	ReasonStartFailed = "start_failed"
	// ReasonBadWorkflow: the run's stored workflow failed to parse when it
	// was admitted from the queue.
	ReasonBadWorkflow = "bad_workflow"
	ReasonBudget      = "budget"
)

// End is how an agent, step or run finishes: its final state, why, and
// when. A zero At means now.
type End struct {
	State, Reason string
	At            time.Time
}

func (e End) at() time.Time {
	if e.At.IsZero() {
		return time.Now()
	}
	return e.At
}

// Agent is one Claude Code session started by the host. ParentID and Role
// are set only on a helper started through spawn. RunID and Step are set on
// a step's agent and on the helpers it spawns. A headless agent runs
// claude -p and ends when its process exits.
type Agent struct {
	ID          string     `json:"id"`
	SessionID   string     `json:"session_id"`
	TmuxSession string     `json:"tmux_session"`
	Dir         string     `json:"dir"`
	Prompt      string     `json:"prompt"`
	ParentID    string     `json:"parent_id,omitempty"`
	Role        string     `json:"role,omitempty"`
	RunID       string     `json:"run_id,omitempty"`
	Step        string     `json:"step,omitempty"`
	Headless    bool       `json:"headless,omitempty"`
	State       string     `json:"state"`
	Reason      string     `json:"reason,omitempty"`
	LastHook    string     `json:"last_hook,omitempty"`
	LastHookAt  *time.Time `json:"last_hook_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	DoneAt      *time.Time `json:"done_at,omitempty"`
	// GCAt is set once garbage collection removed the agent's folders; it
	// cannot be resumed after that.
	GCAt    *time.Time `json:"gc_at,omitempty"`
	Handoff *Handoff   `json:"handoff,omitempty"`
}

// Handoff is what an agent hands back when it calls done. Synthesized is
// set when the host wrote it for an agent that ended without done. Parts
// are the structured results beside the text; only Agent loads them, so
// Agents stays small.
type Handoff struct {
	Text        string     `json:"text"`
	Worktrees   []Worktree `json:"worktrees"`
	Synthesized bool       `json:"synthesized,omitempty"`
	Parts       []Part     `json:"parts,omitempty"`
}

type Worktree struct {
	Repo   string `json:"repo"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
}

// Run is one walk through a workflow toward a goal. Workflow is the YAML it
// was started with. Source names what asked for the run and Ref is that
// asker's key for it; a ticket run has source linear and the issue id.
type Run struct {
	ID         string            `json:"id"`
	Goal       string            `json:"goal"`
	Workflow   string            `json:"workflow"`
	Source     string            `json:"source,omitempty"`
	Ref        string            `json:"ref,omitempty"`
	Account    string            `json:"account,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	GitConfig  [][2]string       `json:"git_config,omitempty"`
	ContextCap int64             `json:"context_cap,omitempty"`
	CapKills   int               `json:"cap_kills,omitempty"`
	TokenCap   int64             `json:"token_cap,omitempty"`
	Dir        string            `json:"dir,omitempty"`
	State      string            `json:"state"`
	Reason     string            `json:"reason,omitempty"`
	CreatedAt  time.Time         `json:"created_at"`
	DoneAt     *time.Time        `json:"done_at,omitempty"`
	AckedAt    *time.Time        `json:"acked_at,omitempty"`
	Steps      []Step            `json:"steps"`
}

// ManagedWorktree is a git worktree the host made and owns: it was asked
// for through spawn or the worktree tool, and garbage collection removes it
// once its run is acked and old enough, unless it has changes in it. It is
// a different fact from a Worktree in a handoff, which is what an agent
// reported.
type ManagedWorktree struct {
	ID int64 `json:"id"`
	// RunID and AgentID are the run and the agent that asked for it.
	RunID   string `json:"run_id,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	// Repo is the main checkout's path; RepoKey its config key, if any.
	Repo    string `json:"repo"`
	RepoKey string `json:"repo_key,omitempty"`
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	// State is active, removed or kept; Reason says why a worktree was
	// kept.
	State     string     `json:"state"`
	Reason    string     `json:"reason,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	RemovedAt *time.Time `json:"removed_at,omitempty"`
}

const (
	WorktreeActive  = "active"
	WorktreeRemoved = "removed"
	WorktreeKept    = "kept"
	// WorktreeDirty: kept because it had uncommitted or untracked files.
	WorktreeDirty = "dirty"
	// WorktreeRemovalFailed: kept because git would not remove it.
	WorktreeRemovalFailed = "removal_failed"
)

// SourceLinear is the source of every ticket run.
const SourceLinear = "linear"

// RunFilter selects runs by exact source and ref; an empty field matches
// every run. A positive Limit keeps only the Limit newest of those; with
// Active, the runs still queued or busy and the finished runs not yet
// acked come back too, however old, and so do the runs named in Include.
type RunFilter struct {
	Source  string
	Ref     string
	Limit   int
	Active  bool
	Include []string
}

// LiveRunError is returned by CreateRun when a run with the same source and
// ref has not finished yet.
type LiveRunError struct {
	RunID, Source, Ref string
}

func (e *LiveRunError) Error() string {
	return fmt.Sprintf("run %s for %s %s is still %s", e.RunID, e.Source, e.Ref, StateBusy)
}

// Step is one step of a run; AgentID is set once its agent has started.
type Step struct {
	Index     int        `json:"index"`
	Name      string     `json:"name"`
	AgentID   string     `json:"agent_id,omitempty"`
	State     string     `json:"state"`
	Reason    string     `json:"reason,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	DoneAt    *time.Time `json:"done_at,omitempty"`
}

type Store interface {
	CreateAgent(a Agent) error
	Agent(id string) (Agent, error)
	Agents() ([]Agent, error)
	// LiveAgents returns the agents that have not finished, in start order,
	// without their handoffs. It is the cheap read for counting slots and
	// sweeping.
	LiveAgents() ([]Agent, error)
	// Hook records hook on the agent at at. UserPromptSubmit makes the
	// agent busy and Stop makes it idle; a finished agent keeps its state.
	// evs are the hook's own log entries, appended in the same transaction
	// with the agent's id, run and at filled in; when none is a hook or
	// tool.used event, a bare hook event is appended first.
	Hook(agentID, hook string, at time.Time, evs ...Event) error
	// Done stores the handoff and marks the agent done with reason. It
	// returns ErrFinished, and stores nothing, if the agent is done, killed
	// or failed already: a late done never overwrites a kill. A reopened
	// agent is idle, so it can call done again.
	Done(agentID string, h Handoff, reason string, at time.Time) error
	// EndAgent marks a live agent e.State, killed or failed, for e.Reason,
	// and returns ErrFinished if it had already finished.
	EndAgent(agentID string, e End) error
	// EndAgentHandoff is EndAgent that also stores h as the agent's
	// handoff, replacing any it had, in the same transaction: the host
	// writes a synthesized handoff for an agent that ended without done.
	EndAgentHandoff(agentID string, e End, h Handoff) error
	// BusyAgentIDs returns the ids of the agents in state busy: the ones
	// working, as against idle at a prompt or finished.
	BusyAgentIDs() ([]string, error)
	// CollectHandoffs marks the handoffs of helperIDs as having reached
	// their parent, by one of the Collected values, and appends
	// handoff.collected for each one not marked already. A new handoff,
	// from a resumed helper calling done again, starts uncollected.
	CollectHandoffs(helperIDs []string, by string, at time.Time) error
	// Uncollected returns the finished helpers of parentID whose handoff
	// has not reached it, oldest first, each with its handoff: what await
	// still owes the parent after a host restart.
	Uncollected(parentID string) ([]Agent, error)
	// Reopen makes a finished agent idle again and clears its reason; its
	// handoff stays.
	Reopen(agentID string) error
	// CreateRun stores a run and its steps, pending, in the order given. The
	// run is queued when r.State is queued and busy otherwise. It returns a
	// *LiveRunError, and stores nothing, when r has a ref and a run with the
	// same source and ref has not finished.
	CreateRun(r Run) error
	// StartQueuedRun makes a queued run busy, and returns ErrFinished if it
	// was not queued.
	StartQueuedRun(runID string) error
	Run(id string) (Run, error)
	// Runs returns every run f selects with its steps, newest first.
	Runs(f RunFilter) ([]Run, error)
	// StartStep marks a step busy on agentID.
	StartStep(runID string, index int, agentID string, at time.Time) error
	// FinishStep and FinishRun set e.State (done, blocked, failed or
	// killed) and e.Reason, and return ErrFinished if the step or run had
	// already finished.
	FinishStep(runID string, index int, e End) error
	FinishRun(runID string, e End) error
	// Inbox returns the finished runs not yet acked, oldest finished
	// first; a non-empty source keeps only that source's.
	Inbox(source string) ([]Run, error)
	// Ack marks a finished run acked at at. It returns ErrNotFinished for a
	// queued or busy run, and keeps the first ack of a run acked already.
	Ack(runID string, at time.Time) error

	// AgentsOfRuns returns the agents of the given runs, in start order,
	// with their handoffs.
	AgentsOfRuns(runIDs []string) ([]Agent, error)
	// MarkGC records that the agent's folders were removed at at, and
	// appends agent.gc.
	MarkGC(agentID string, at time.Time) error

	// CreateManagedWorktree stores w as active, appends worktree.created,
	// and returns it with its id.
	CreateManagedWorktree(w ManagedWorktree) (ManagedWorktree, error)
	// ManagedWorktrees returns the host's worktrees of runID, oldest
	// first; an empty runID returns every one.
	ManagedWorktrees(runID string) ([]ManagedWorktree, error)
	// SetManagedWorktree moves worktree id to state for reason; removed
	// sets its removed time to at. It appends worktree.removed, or
	// worktree.kept for any other state but active.
	SetManagedWorktree(id int64, state, reason string, at time.Time) error

	// Append adds events to the log, in order, and wakes subscribers once
	// they are committed.
	Append(evs ...Event) error
	// LastEventID is the id of the newest event, or zero when there is
	// none.
	LastEventID() (int64, error)
	// Events returns the events q selects, oldest first.
	Events(q EventQuery) ([]Event, error)
	LastEvents(kind string, agentIDs []string) (map[string]Event, error)
	RunTokens(runIDs []string) (map[string]int64, error)
	ReopenedTokens(runID string) (int64, error)
	// Subscribe returns a channel that gets a signal after each committed
	// write that appended events. Signals coalesce, so a reader re-reads
	// with Events from its cursor and never loses one. cancel stops them.
	Subscribe() (signal <-chan struct{}, cancel func())
	// CompactEvents prunes the payloads of the events of the given runs
	// whose kind is among kinds, which must all be in BulkyKinds. Rows and
	// ids stay, so cursors and counts hold; it returns how many it pruned.
	CompactEvents(runIDs, kinds []string) (int64, error)

	// AddClaims replaces agentID's claims with cs, in order: claim i gets
	// id ClaimID(agentID, i). Each claim keeps the RunID it carries.
	AddClaims(agentID string, cs []Claim) error
	// Claims returns run runID's claims, oldest first, each agent's in
	// order.
	Claims(runID string) ([]Claim, error)
	// AgentClaims returns agentID's claims in order.
	AgentClaims(agentID string) ([]Claim, error)
	// Claim returns one claim by id.
	Claim(id string) (Claim, error)
	// SetVerdict records a witness's verdict on a claim, replacing any
	// earlier one. verdict must be one of Verdicts.
	SetVerdict(claimID, verdict, note, witnessID string, at time.Time) error

	Close() error
}
