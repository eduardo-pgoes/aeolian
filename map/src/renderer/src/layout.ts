import type { Agent, Run, Step } from '../../main/host'
import { DIALS } from './dials'
import type { Cell } from './term'

export interface Rect {
  x: number
  y: number
  w: number
  h: number
}

export interface Size {
  cols: number
  rows: number
}

export type Slot = ({ kind: 'agent'; agent: Agent } | { kind: 'pending'; step: Step }) & {
  key: string
  rect: Rect
}

export const FINISHED = ['done', 'killed', 'failed']

export const isLive = (a: Agent): boolean => !FINISHED.includes(a.state)

export const isWorking = (a: Agent): boolean => a.state === 'busy' && !a.blocked

// clampSize keeps a size the map asks for between the dials; cols only get
// the floor, rows also get maxRows.
export function clampSize(size: Size): Size {
  return {
    cols: Math.max(DIALS.minCols, size.cols),
    rows: Math.min(DIALS.maxRows, Math.max(DIALS.minRows, size.rows))
  }
}

// The size the map gives a session's terminal: the host's, clamped.
export function hostSize(a: Agent): Size {
  return clampSize({ cols: a.cols ?? DIALS.startCols, rows: a.rows ?? DIALS.startRows })
}

// The window's chrome around the terminal grid; terminal.css pads .w-body
// with the same numbers.
const chromeW = (): number => 2 * DIALS.padPx
const chromeH = (): number => DIALS.headPx + DIALS.bandPx + DIALS.gutPx + DIALS.padPx

export function windowSize(size: Size, cell: Cell): { w: number; h: number } {
  return {
    w: size.cols * cell.w + chromeW(),
    h: size.rows * cell.h + chromeH()
  }
}

export function sizeFor(w: number, h: number, cell: Cell): Size {
  return clampSize({
    cols: Math.round((w - chromeW()) / cell.w),
    rows: Math.round((h - chromeH()) / cell.h)
  })
}

export const CARD = { w: DIALS.cardW, h: DIALS.cardH }

export function stepAgent(run: Run, step: Step): Agent | undefined {
  const byId = step.agent_id && run.agents.find((a) => a.id === step.agent_id)
  if (byId) return byId
  return run.agents.filter((a) => a.step === step.name && !a.parent_id).at(-1)
}

export function layout(run: Run, cell: Cell, sizeOf: (a: Agent) => Size): Slot[] {
  const slots: Slot[] = []
  const placed = new Set<string>()

  const place = (agent: Agent, x: number, y: number): number => {
    placed.add(agent.id)
    const { w, h } = isLive(agent) ? windowSize(sizeOf(agent), cell) : CARD
    slots.push({ kind: 'agent', agent, key: agent.id, rect: { x, y, w, h } })
    let bottom = y + h
    let hy = y + DIALS.dockY
    const helpers = run.agents
      .filter((c) => c.parent_id === agent.id && !placed.has(c.id))
      .sort((a, b) => a.created_at.localeCompare(b.created_at))
    for (const c of helpers) {
      const b = place(c, x + w + DIALS.gapX, hy)
      bottom = Math.max(bottom, b)
      hy = b + DIALS.gapY / 2
    }
    return bottom
  }

  let y = 0
  for (const step of [...run.steps].sort((a, b) => a.index - b.index)) {
    const agent = stepAgent(run, step)
    if (agent && !placed.has(agent.id)) {
      y = place(agent, 0, y) + DIALS.gapY
      continue
    }
    slots.push({ kind: 'pending', step, key: `step:${step.name}`, rect: { x: 0, y, ...CARD } })
    y += CARD.h + DIALS.gapY
  }
  return slots
}
