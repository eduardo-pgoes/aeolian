import FREEFORM from '../../../examples/freeform.yaml?raw'

// Must stay in sync with DefaultAddr in cmd/aeolian.
const DEFAULT_ADDR = '127.0.0.1:7431'
export const BASE = `http://${process.env.AEOLIAN_ADDR || DEFAULT_ADDR}`

export interface Worktree {
  repo: string
  path: string
  branch: string
}

export interface Handoff {
  text: string
  worktrees: Worktree[]
  // set when the host wrote it for an agent that ended without done
  synthesized?: boolean
}

export interface Agent {
  id: string
  session_id: string
  tmux_session: string
  dir: string
  parent_id?: string
  role?: string
  run_id?: string
  step?: string
  state: string
  // why a finished agent, step or run stopped: delivered, ended, closed, lost...
  reason?: string
  blocked: boolean
  created_at: string
  done_at?: string
  handoff?: Handoff
  ink: string
  mark: string
  // the waveform the host picked for this agent's step or role in its run
  tuning?: string
  cols?: number
  rows?: number
}

export interface Step {
  index: number
  name: string
  agent_id?: string
  state: string
  reason?: string
  ink: string
  mark: string
  tuning?: string
}

export interface Run {
  id: string
  goal: string
  state: string
  reason?: string
  account?: string
  created_at: string
  done_at?: string
  steps: Step[]
  agents: Agent[]
}

export interface AccountsView {
  accounts: string[]
  default?: string
}

export interface InboxEntry {
  run_id: string
  source: string
  ref: string
  state: string
  reason?: string
  goal: string
  finished_at: string | null
  step: string
  handoff: string
  worktrees: Worktree[] | null
  log_tail: string[] | null
  parts?: Part[]
}

// Part is a structured result beside a handoff; body is shaped by kind.
export interface Part {
  kind: string
  body: unknown
}

// DigestView is GET /api/digest: what needs the person, ranked, capped and
// grouped by run (run is absent on the precedents group).
export interface DigestItem {
  kind: string
  run_id?: string
  text: string
  note?: string
  evidence?: string[]
  ref?: string
  action?: string
  count?: number
  key: string
  at: string
  dismissed?: boolean
}

export interface DigestRun {
  id: string
  goal: string
  state: string
  reason?: string
  source?: string
  started_at: string
  finished_at: string
  precedents?: string[]
}

export interface DigestView {
  since: string
  now: string
  run_id?: string
  groups: { run?: DigestRun; items: DigestItem[] }[]
  shown: number
  overflow: number
  clean: DigestRun[]
  running: number
  dismissed: number
}

export interface WorkflowEntry {
  name: string
  description: string
  steps: string[]
  contents: string
  error: string
}

export type WorkflowRef = { contents: string } | { builtin: 'freeform' }

export interface HostInfo {
  max_agents: number
  busy?: number
  idle?: number
  last_event_id?: number
}

// HostEvent is one entry of the host's event log, as /api/events/stream
// sends it. A 'reset' event is made up by the map when the stream skipped
// ahead.
export interface HostEvent {
  id: number
  at: string
  run_id?: string
  agent_id?: string
  kind: string
  data: unknown
}

// RUNS_NEWEST is how many finished, acked runs the map lists besides the
// queued, busy and unacked ones.
export const RUNS_NEWEST = 20

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  let resp: Response
  try {
    resp = await fetch(BASE + path, {
      method,
      ...(body === undefined ? {} : { body: JSON.stringify(body), headers: { 'content-type': 'application/json' } })
    })
  } catch (err) {
    throw new Error(`is the host running at ${BASE}? ${(err as Error).message}`)
  }
  if (!resp.ok) throw new Error(`${method} ${path}: ${resp.status} ${(await resp.text()).trim()}`)
  return (await resp.json()) as T
}

// listRuns returns the runs the map draws: every queued, busy or unacked
// run, the RUNS_NEWEST newest, and include, such as the pinned run.
export function listRuns(include?: string | null): Promise<Run[]> {
  const q = new URLSearchParams({ limit: String(RUNS_NEWEST), active: '1' })
  if (include) q.set('include', include)
  return call<Run[]>('GET', `/api/runs?${q}`)
}

export function info(): Promise<HostInfo> {
  return call<HostInfo>('GET', '/api/host')
}

export function inbox(): Promise<InboxEntry[]> {
  return call<InboxEntry[]>('GET', '/api/inbox')
}

export function digest(since: string): Promise<DigestView> {
  return call<DigestView>('GET', `/api/digest?since=${encodeURIComponent(since)}`)
}

export function dismissDigest(keys: string[]): Promise<{ keys: string[] }> {
  return call<{ keys: string[] }>('POST', '/api/digest/dismiss', { keys })
}

export function ack(runId: string): Promise<Run> {
  return call<Run>('POST', `/api/runs/${encodeURIComponent(runId)}/ack`)
}

export function kill(agentId: string): Promise<Agent> {
  return call<Agent>('POST', `/api/agents/${encodeURIComponent(agentId)}/kill`)
}

export function listWorkflows(): Promise<WorkflowEntry[]> {
  return call<WorkflowEntry[]>('GET', '/api/workflows')
}

export function startRun(workflow: WorkflowRef, goal: string, account = ''): Promise<Run> {
  const contents = 'builtin' in workflow ? FREEFORM : workflow.contents
  return call<Run>('POST', '/api/runs', account ? { workflow: contents, goal, account } : { workflow: contents, goal })
}

export function listAccounts(): Promise<AccountsView> {
  return call<AccountsView>('GET', '/api/accounts')
}

export function killRun(runId: string): Promise<Run> {
  return call<Run>('POST', `/api/runs/${encodeURIComponent(runId)}/kill`)
}

// endRun closes a run as done: its lead is asked to hand back first unless
// force. The host answers at once with the run as it is; it may still be busy.
export function endRun(runId: string, force = false): Promise<Run> {
  return call<Run>('POST', `/api/runs/${encodeURIComponent(runId)}/end${force ? '?force=1' : ''}`)
}

// resume restarts a finished agent's session and returns the tmux session
// to attach to.
export async function resume(agentId: string): Promise<string> {
  const res = await call<{ tmux_session: string }>('POST', `/api/agents/${encodeURIComponent(agentId)}/resume`)
  return res.tmux_session
}
