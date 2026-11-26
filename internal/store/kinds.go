package store

import (
	"encoding/json"
	"time"
)

// Event kinds. This is the registry: every kind the host or the store
// appends is listed here, so a reader can switch on them and a new feature
// adds its kind to the list instead of inventing a string at the call site.
// Payloads are versioned with a "v" field and only ever gain fields.
//
// Projection kinds are appended by the store inside the same transaction as
// the row change they describe; Fold reads only these. Host kinds describe
// what happened around the rows and project into nothing.
const (
	// Projection kinds.
	KindRunCreated   = "run.created"   // RunCreated
	KindRunStarted   = "run.started"   // queued run made busy; Versioned
	KindRunFinished  = "run.finished"  // Ended
	KindRunAcked     = "run.acked"     // first ack only; Versioned
	KindStepStarted  = "step.started"  // StepStarted
	KindStepFinished = "step.finished" // Ended, with Index
	KindAgentCreated = "agent.created" // AgentCreated
	KindAgentState   = "agent.state"   // AgentState
	KindAgentDone    = "agent.done"    // AgentDone
	KindAgentEnded   = "agent.ended"   // Ended
	KindHook         = "hook"          // HookData, for every hook but PostToolUse
	KindToolUsed     = "tool.used"     // ToolUsed, the distilled PostToolUse hook
	KindAgentGC      = "agent.gc"      // garbage collection removed the agent's folders; Versioned

	// Host kinds.
	KindHumanMessage     = "human.message"       // HumanMessage
	KindUsage            = "usage"               // Usage
	KindSpawnRequested   = "spawn.requested"     // SpawnRequested
	KindSpawnRefused     = "spawn.refused"       // SpawnRefused
	KindCapWaiting       = "cap.waiting"         // CapData
	KindCapAdmitted      = "cap.admitted"        // CapData
	KindHandoffDelivered = "handoff.delivered"   // HandoffDelivered
	KindHandoffTyped     = "handoff.typed"       // HandoffTyped
	KindHandoffCollected = "handoff.collected"   // HandoffCollected, appended by the store
	KindHandoffDropped   = "handoff.dropped"     // HandoffDropped
	KindOnFinishResult   = "onfinish.result"     // OnFinishResult
	KindRunEnding        = "run.ending"          // RunEnding
	KindAgentStalled     = "agent.stalled"       // AgentStalled
	KindAgentIdleClosed  = "agent.idle_closed"   // AgentIdleClosed
	KindPartsRecorded    = "parts.recorded"      // PartsRecorded
	KindPartsFailed      = "parts.record_failed" // PartsFailed
	KindGateHeld         = "gate.held"           // GateData
	KindGatePassed       = "gate.passed"         // GateData
	KindGateTimeout      = "gate.timeout"        // GateData, with Error
	KindGateCancelled    = "gate.cancelled"      // GateData
	KindGateFailed       = "gate.failed"         // GateData, with Error
	KindClaimsRecorded   = "claims.recorded"     // ClaimsRecorded
	KindClaimVerdict     = "claim.verdict"       // ClaimVerdict

	KindAgentLaunchBlocked = "agent.launch_blocked"

	KindWorktreeCreated  = "worktree.created"  // WorktreeData
	KindWorktreeRemoved  = "worktree.removed"  // WorktreeData
	KindWorktreeKept     = "worktree.kept"     // WorktreeData, with Reason
	KindGCSwept          = "gc.swept"          // GCSwept
	KindArenaStarted     = "arena.started"     // ArenaStarted
	KindArenaPhase       = "arena.phase"       // ArenaPhase: proposing, judging, evolving
	KindArenaJudgment    = "arena.judgment"    // Versioned, by the judge's agent
	KindArenaFinished    = "arena.finished"    // ArenaFinished
	KindWitnessStarted   = "witness.started"   // WitnessStarted
	KindWitnessFinished  = "witness.finished"  // WitnessFinished
	KindPrecedentStale   = "precedent.stale"   // PrecedentIDs: precedents past review_by, left out
	KindPrecedentApplied = "precedent.applied" // PrecedentIDs: put into an agent's prompt at launch
	KindPrecedentAdded   = "precedent.added"   // PrecedentAdded
	KindDigestDismissed  = "digest.dismissed"  // DigestDismissed
)

// ProjectionKinds are the kinds Fold reads. CompactEvents never touches
// them.
var ProjectionKinds = []string{
	KindRunCreated, KindRunStarted, KindRunFinished, KindRunAcked,
	KindStepStarted, KindStepFinished,
	KindAgentCreated, KindAgentState, KindAgentDone, KindAgentEnded,
	KindHook, KindToolUsed, KindAgentGC,
}

// BulkyKinds are the kinds whose payloads CompactEvents prunes: they carry
// hook input, which is large and only matters while it is fresh.
var BulkyKinds = []string{KindHook, KindToolUsed, KindHumanMessage}

// EventVersion is the payload version every kind starts at.
const EventVersion = 1

// Versioned is the payload of a kind that carries nothing else.
type Versioned struct {
	V          int  `json:"v"`
	Backfilled bool `json:"backfilled,omitempty"`
}

type RunCreated struct {
	V          int               `json:"v"`
	Goal       string            `json:"goal"`
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
	Steps      []string          `json:"steps"`
	Workflow   string            `json:"workflow"`
	Backfilled bool              `json:"backfilled,omitempty"`
}

// Ended is the payload of run.finished, step.finished and agent.ended.
type Ended struct {
	V          int    `json:"v"`
	Index      *int   `json:"index,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	Backfilled bool   `json:"backfilled,omitempty"`
}

type StepStarted struct {
	V          int    `json:"v"`
	Index      int    `json:"index"`
	AgentID    string `json:"agent_id"`
	Backfilled bool   `json:"backfilled,omitempty"`
}

type AgentCreated struct {
	V          int    `json:"v"`
	SessionID  string `json:"session_id"`
	Tmux       string `json:"tmux"`
	Dir        string `json:"dir"`
	ParentID   string `json:"parent_id,omitempty"`
	Role       string `json:"role,omitempty"`
	Step       string `json:"step,omitempty"`
	Headless   bool   `json:"headless,omitempty"`
	State      string `json:"state"`
	Prompt     string `json:"prompt"`
	Backfilled bool   `json:"backfilled,omitempty"`
}

// Causes of an agent.state change besides a hook's name.
const CauseReopen = "reopen"

type AgentState struct {
	V          int    `json:"v"`
	From       string `json:"from"`
	To         string `json:"to"`
	Cause      string `json:"cause"`
	Backfilled bool   `json:"backfilled,omitempty"`
}

// AgentDone is an agent finishing with a handoff. State is empty for done;
// an agent the host ended and wrote a handoff for carries its end state.
type AgentDone struct {
	V          int      `json:"v"`
	State      string   `json:"state,omitempty"`
	Reason     string   `json:"reason"`
	Handoff    *Handoff `json:"handoff"`
	Backfilled bool     `json:"backfilled,omitempty"`
}

// HookData is a hook's name and its input, cut down to fit; see Truncate.
type HookData struct {
	V          int             `json:"v"`
	Event      string          `json:"event"`
	By         string          `json:"by,omitempty"` // UserPromptSubmit: human, host or claude
	Payload    json.RawMessage `json:"payload,omitempty"`
	Backfilled bool            `json:"backfilled,omitempty"`
}

// Who typed a prompt, for HookData.By.
const (
	ByHuman = "human"
	ByHost  = "host"
	// ByClaude is a prompt Claude Code submits itself, such as the
	// notification that a background task finished.
	ByClaude = "claude"
)

type ToolUsed struct {
	V     int    `json:"v"`
	Tool  string `json:"tool_name"`
	Input string `json:"input,omitempty"` // at most ToolInputLimit bytes
	OK    bool   `json:"ok"`
}

type HumanMessage struct {
	V    int    `json:"v"`
	Text string `json:"text"` // at most HumanMessageLimit bytes
}

// Usage is the token use an agent's transcript gained since the last usage
// event, for one model. Offset is how far into Path the transcript has been
// read, so a restarted host resumes from it.
type Usage struct {
	V          int    `json:"v"`
	Model      string `json:"model"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
	// CacheWrite1h is the part of CacheWrite written to the one-hour cache.
	CacheWrite1h int64  `json:"cache_write_1h,omitempty"`
	Turns        int    `json:"turns"`
	Path         string `json:"path"`
	Offset       int64  `json:"offset"`
	Context      int64  `json:"context"`
}

type SpawnRequested struct {
	V         int        `json:"v"`
	ParentID  string     `json:"parent_id"`
	Role      string     `json:"role"`
	HelperID  string     `json:"helper_id"`
	Wait      bool       `json:"wait"`
	Dir       string     `json:"dir"`
	Worktrees []Worktree `json:"worktrees,omitempty"`
}

type SpawnRefused struct {
	V     int    `json:"v"`
	Role  string `json:"role"`
	Error string `json:"error"`
}

type CapData struct {
	V        int `json:"v"`
	QueueLen int `json:"queue_len"`
}

type HandoffDelivered struct {
	V        int    `json:"v"`
	HelperID string `json:"helper_id"`
	ParentID string `json:"parent_id"`
}

// HandoffCollected is a finished helper's handoff that reached its
// parent: By says how, one of the Collected values.
type HandoffCollected struct {
	V        int    `json:"v"`
	ParentID string `json:"parent_id"`
	By       string `json:"by"`
}

// How a helper's handoff reached its parent.
const (
	CollectedSpawn = "spawn" // its parent's blocked spawn call returned it
	CollectedAwait = "await" // an await call returned it
	CollectedTyped = "typed" // it was typed into its parent's terminal
	CollectedHold  = "hold"  // a witness's handoff reached, or was meant for, its producer's done gate
)

type HandoffTyped struct {
	V      int    `json:"v"`
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
	Len    int    `json:"len"`
}

// Why a handoff or message meant for an agent's terminal was not typed.
const (
	DroppedFinished = "finished"
	DroppedHeadless = "headless"
	DroppedError    = "error"
	// DroppedRunEnding is a helper's handoff that came in while its run
	// was ending: the lead is being asked to wrap up, so nothing is typed.
	DroppedRunEnding = "run_ending"
)

// RunEnding is a run being ended on purpose; By is who asked.
type RunEnding struct {
	V  int    `json:"v"`
	By string `json:"by"`
}

// AgentStalled is a busy helper the watchdog stopped after QuietS seconds
// without a sign of life.
type AgentStalled struct {
	V      int    `json:"v"`
	QuietS int    `json:"quiet_s"`
	Role   string `json:"role"`
}

// AgentIdleClosed is an idle helper the watchdog closed after IdleS seconds
// at its prompt.
type AgentIdleClosed struct {
	V     int `json:"v"`
	IdleS int `json:"idle_s"`
}

type AgentLaunchBlocked struct {
	V       int    `json:"v"`
	WaitedS int    `json:"waited_s"`
	Role    string `json:"role"`
	Match   string `json:"match"`
}

type HandoffDropped struct {
	V      int    `json:"v"`
	Target string `json:"target"`
	Why    string `json:"why"`
	Error  string `json:"error,omitempty"`
	Len    int    `json:"len"`
}

type OnFinishResult struct {
	V          int      `json:"v"`
	Exit       int      `json:"exit"`
	DurationMS int64    `json:"duration_ms"`
	StderrTail []string `json:"stderr_tail,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// event builds an event with data marshalled; data is one of the payloads
// above, which always marshal.
func event(at time.Time, runID, agentID, kind string, data any) Event {
	b, err := json.Marshal(data)
	if err != nil {
		panic(err)
	}
	return Event{At: at, RunID: runID, AgentID: agentID, Kind: kind, Data: b}
}

// PartsRecorded is a handoff's parts once stored: their kinds in first-seen
// order and how many of each.
type PartsRecorded struct {
	V      int            `json:"v"`
	Kinds  []string       `json:"kinds"`
	Counts map[string]int `json:"counts"`
}

// PartsFailed is a part kind whose Record failed.
type PartsFailed struct {
	V     int    `json:"v"`
	Kind  string `json:"kind"`
	Error string `json:"error"`
}

// GateData is a done gate's Hold starting or ending.
type GateData struct {
	V     int    `json:"v"`
	Gate  string `json:"gate"`
	Error string `json:"error,omitempty"`
}

type ClaimsRecorded struct {
	V     int `json:"v"`
	Count int `json:"count"`
}

type ClaimVerdict struct {
	V         int    `json:"v"`
	ClaimID   string `json:"claim_id"`
	Verdict   string `json:"verdict"`
	Note      string `json:"note"`
	WitnessID string `json:"witness_id"`
}

// WorktreeData is a managed worktree made, removed, or kept by garbage
// collection with why.
type WorktreeData struct {
	V      int    `json:"v"`
	ID     int64  `json:"id"`
	Repo   string `json:"repo,omitempty"`
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Base   string `json:"base,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// GCSwept is one run's agents garbage collection removed the folders of.
type GCSwept struct {
	V      int   `json:"v"`
	Agents int   `json:"agents"`
	Bytes  int64 `json:"bytes"`
}

// ArenaStarted is an arena's run starting: how many seats it has and who
// makes the final call.
type ArenaStarted struct {
	V        int    `json:"v"`
	Entrants int    `json:"entrants"`
	Judges   int    `json:"judges"`
	Final    string `json:"final"`
}

type ArenaPhase struct {
	V     int    `json:"v"`
	Phase string `json:"phase"`
}

// ArenaFinished is an arena ending: its own state (done, failed, killed),
// the reason its run got, and the arena's note.
type ArenaFinished struct {
	V      int    `json:"v"`
	State  string `json:"state"`
	Reason string `json:"reason"`
	Note   string `json:"note,omitempty"`
}

// WitnessStarted is a witness started on a producer's claims; the event's
// agent is the producer.
type WitnessStarted struct {
	V               int      `json:"v"`
	WitnessID       string   `json:"witness_id"`
	Claims          int      `json:"claims"`
	Precedents      []string `json:"precedents"`
	StalePrecedents []string `json:"stale_precedents"`
}

// WitnessFinished is a witness that called done; Unwitnessed counts the
// claims it left without a verdict.
type WitnessFinished struct {
	V           int    `json:"v"`
	WitnessID   string `json:"witness_id"`
	Unwitnessed int    `json:"unwitnessed"`
}

// PrecedentIDs names precedents. For precedent.stale they were past their
// review_by and left out: of an agent's prompt at launch, or of a witness's
// input, when WitnessID is set.
type PrecedentIDs struct {
	V         int      `json:"v"`
	IDs       []string `json:"ids"`
	WitnessID string   `json:"witness_id,omitempty"`
}

// PrecedentAdded is a precedent a person wrote, drawn from a claim when
// ClaimID is set.
type PrecedentAdded struct {
	V       int    `json:"v"`
	ID      string `json:"id"`
	ClaimID string `json:"claim_id,omitempty"`
}

type DigestDismissed struct {
	V   int    `json:"v"`
	Key string `json:"key"`
}
