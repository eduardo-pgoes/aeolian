import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { Agent, Run } from '../../main/host'
import { agentName, look, stateLabel } from './canvas/win'
import { busy, endRun, Keys, killRun, runNote } from './Booklet'
import { openPicker } from './field'
import { Icon, type IconName } from './icons'
import { ink } from './inks'
import { type Action, chordAction, chordOf, GESTURES, KEYMAP, trigger } from './keymap'
import { isLive } from './layout'
import { mark, markOf } from './marks'
import { closeBooklet, closePalette, flyTo, openBooklet, showOnCanvas } from './nav'
import { canvasRun, useWorld, world } from './store'
import { themeNote, useTheme } from './theme'

interface Cmd {
  id: string
  group: string
  label: string
  hint?: string
  chord?: string
  icon?: IconName
  agent?: Agent
  stay?: boolean
  keep?: boolean
  run: () => void
}

const ACTION_ICON: Record<Exclude<Action, 'palette'>, IconName> = {
  leave: 'fit',
  pinDrawer: 'pin',
  inbox: 'tray',
  digest: 'book',
  theme: 'theme'
}

function fuzzy(q: string, text: string): number {
  const s = text.toLowerCase()
  let at = 0
  let prev = -2
  let score = 0
  for (const ch of q) {
    if (ch === ' ') continue
    const i = s.indexOf(ch, at)
    if (i < 0) return -1
    if (i === prev + 1) score += 4
    else if (i === 0 || ' ·-_/.'.includes(s[i - 1])) score += 3
    else score += 1 - Math.min(1, (i - at) / 20)
    prev = i
    at = i + 1
  }
  return score
}

const order = (a: Agent): number => (isLive(a) ? (a.blocked ? 0 : 1) : a.state === 'pending' ? 3 : 2)

function agentCmd(a: Agent, group: string, run?: Run): Cmd {
  const focused = world.focusId === a.id ? ', focused' : ''
  return {
    id: `a:${a.id}`,
    group,
    label: agentName(a),
    hint: run ? `${stateLabel(a)} · ${run.goal}` : `${stateLabel(a)}${focused}`,
    agent: a,
    keep: true,
    run: () => flyTo(a)
  }
}

function commands(q: string): Cmd[] {
  const w = world
  const run = canvasRun()
  const waiting = w.inbox.length
  const cmds: Cmd[] = []

  const keyed = (action: Exclude<Action, 'palette'>, group: string, label: string, hint?: string, stay?: boolean): Cmd => ({
    id: `k:${action}`,
    group,
    label,
    hint,
    chord: chordOf(action),
    icon: ACTION_ICON[action],
    stay,
    run: () => trigger(action)
  })

  const inboxRow = keyed(
    'inbox',
    waiting ? 'waiting' : 'go',
    w.sheet === 'inbox' ? 'close the inbox' : 'open the inbox',
    waiting ? `${waiting} finished ${waiting === 1 ? 'run' : 'runs'} waiting` : 'nothing waiting'
  )
  if (waiting) cmds.push(inboxRow)

  if (run) {
    const g = `fly to · ${run.goal}`
    ;[...run.agents].sort((a, b) => order(a) - order(b)).forEach((a) => cmds.push(agentCmd(a, g)))
  }

  if (!waiting) cmds.push(inboxRow)
  cmds.push(keyed('digest', 'go', w.sheet === 'digest' ? 'close the digest' : 'open the digest'))
  cmds.push({ id: 'new', group: 'go', label: 'new run', hint: 'pick a workflow and a goal', icon: 'plus', run: openPicker })

  for (const r of w.runs) {
    if (r.id === run?.id) continue
    cmds.push({
      id: `r:${r.id}`,
      group: 'runs',
      label: `switch to ${r.goal}`,
      hint: runNote(r),
      icon: 'runs',
      keep: true,
      run: () => showOnCanvas(r.id)
    })
  }

  for (const r of w.runs) {
    if (!busy(r) || (!q && r.id !== run?.id)) continue
    const which = r.id === run?.id ? 'this run' : r.goal
    cmds.push({ id: `e:${r.id}`, group: 'actions', label: `end ${which}`, hint: 'its lead hands back first', icon: 'end', run: () => endRun(r.id) })
    cmds.push({ id: `x:${r.id}`, group: 'actions', label: `kill ${which}`, hint: 'stops every agent in it', icon: 'stop', run: () => killRun(r.id) })
  }

  cmds.push(
    w.bookletOpen
      ? { id: 'bk', group: 'view', label: 'close the booklet', icon: 'side', run: () => closeBooklet() }
      : { id: 'bk', group: 'view', label: 'open the booklet', icon: 'side', run: openBooklet }
  )
  for (const k of KEYMAP) {
    if (k.action === 'leave') cmds.push(keyed('leave', 'view', k.label))
    if (k.action === 'pinDrawer') cmds.push(keyed('pinDrawer', 'view', w.drawerPinned ? 'unpin the booklet' : 'pin the booklet open'))
  }
  const t = themeNote()
  cmds.push(keyed('theme', 'view', 'cycle the theme', `now ${t.now}${t.follows ? ', following the OS' : ', pinned'} · next: ${t.next}`, true))

  if (q) {
    for (const r of w.runs) {
      if (r.id === run?.id) continue
      r.agents.forEach((a) => cmds.push(agentCmd(a, 'fly to · other runs', r)))
    }
  }
  return cmds
}

function filter(q: string): Cmd[] {
  const all = commands(q)
  if (!q) return all
  const need = q.replace(/ /g, '').length
  const scored = all
    .map((c, i) => ({ c, i, s: Math.max(fuzzy(q, c.label) * 2, fuzzy(q, `${c.label} ${c.hint ?? ''} ${c.group}`)) }))
    .filter((x) => x.s >= need)
  const best = new Map<string, number>()
  const first = new Map<string, number>()
  for (const x of scored) {
    best.set(x.c.group, Math.max(best.get(x.c.group) ?? -1, x.s))
    if (!first.has(x.c.group)) first.set(x.c.group, x.i)
  }
  const rank = (x: { c: Cmd }): [number, number] => [best.get(x.c.group)!, first.get(x.c.group)!]
  return scored
    .sort((a, b) => {
      const [ba, fa] = rank(a)
      const [bb, fb] = rank(b)
      return bb - ba || fa - fb || b.s - a.s || a.i - b.i
    })
    .map((x) => x.c)
}

function Lead({ c }: { c: Cmd }) {
  if (c.agent)
    return (
      <span
        className={`pal-ic pal-mark ${look(c.agent)}`}
        style={{ '--c': ink(c.agent.ink) } as React.CSSProperties}
        dangerouslySetInnerHTML={{ __html: mark(markOf(c.agent)) }}
      />
    )
  return <span className="pal-ic">{c.icon && <Icon name={c.icon} />}</span>
}

function Sheet() {
  useWorld()
  useTheme()
  const [q, setQ] = useState('')
  const [selId, setSelId] = useState<string | null>(null)
  const [, bump] = useState(0)
  const input = useRef<HTMLInputElement>(null)
  const list = useRef<HTMLDivElement>(null)
  const query = q.trim().toLowerCase()
  const rows = filter(query)
  const found = rows.findIndex((c) => c.id === selId)
  const sel = found < 0 ? 0 : found
  const cur = rows[sel]

  useEffect(() => input.current!.focus({ preventScroll: true }), [])

  useLayoutEffect(() => {
    list.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({ block: 'nearest' })
  }, [sel, query])

  const run = (c: Cmd | undefined): void => {
    if (!c) return
    if (c.stay) {
      c.run()
      bump((n) => n + 1)
      return
    }
    closePalette(!c.keep)
    c.run()
  }

  const move = (d: number): void => {
    if (!rows.length) return
    setSelId((prev) => {
      const i = Math.max(0, rows.findIndex((c) => c.id === prev))
      return rows[(i + d + rows.length) % rows.length].id
    })
  }

  const keys = (e: React.KeyboardEvent<HTMLInputElement>): void => {
    if (e.nativeEvent.isComposing) return
    const k = e.key
    if (k === 'ArrowDown' || (e.ctrlKey && !e.shiftKey && k === 'n')) move(1)
    else if (k === 'ArrowUp' || (e.ctrlKey && !e.shiftKey && k === 'p')) move(-1)
    else if (k === 'PageDown') setSelId(rows[Math.min(rows.length - 1, sel + 8)]?.id ?? null)
    else if (k === 'PageUp') setSelId(rows[Math.max(0, sel - 8)]?.id ?? null)
    else if (k === 'Enter') run(cur)
    else if (k === 'Escape') closePalette()
    else if (k === 'Tab') {
      e.preventDefault()
      return
    } else {
      const a = chordAction(e.nativeEvent)
      if (a && a !== 'palette') closePalette()
      return
    }
    e.preventDefault()
    e.stopPropagation()
  }

  let group = ''
  return (
    <div className="pal-wrap">
      <div className="pal-scrim" onPointerDown={() => closePalette()} />
      <div className="pal" role="dialog" aria-modal="true" aria-label="commands">
        <label className="pal-in">
          <Icon name="search" />
          <input
            ref={input}
            role="combobox"
            aria-expanded="true"
            aria-controls="pal-list"
            aria-activedescendant={cur ? `pal-${cur.id}` : undefined}
            aria-autocomplete="list"
            spellCheck={false}
            autoComplete="off"
            placeholder="fly to an agent, or type a command…"
            value={q}
            onChange={(e) => {
              setQ(e.target.value)
              setSelId(null)
            }}
            onKeyDown={keys}
          />
          <kbd>esc</kbd>
        </label>
        <div ref={list} id="pal-list" className="pal-list" role="listbox" aria-label="commands">
          {!rows.length && <p className="pal-none">nothing matches “{q.trim()}”</p>}
          {rows.map((c, i) => {
            const head = c.group !== group
            group = c.group
            return (
              <div key={c.id} role="presentation">
                {head && (
                  <div className="pal-grp" role="presentation">
                    <span className="lab">{c.group}</span>
                  </div>
                )}
                <div
                  id={`pal-${c.id}`}
                  role="option"
                  aria-selected={i === sel}
                  className={`pal-row${i === sel ? ' sel' : ''}`}
                  onPointerDown={(e) => e.preventDefault()}
                  onClick={() => run(c)}
                >
                  <Lead c={c} />
                  <span className="t">
                    {c.label}
                    {c.hint && <i> · {c.hint}</i>}
                  </span>
                  <Keys chord={c.chord} />
                </div>
              </div>
            )
          })}
        </div>
        <footer className="pal-foot">
          <span className="lab">
            <kbd>↑</kbd>
            <kbd>↓</kbd> move
          </span>
          <span className="lab">
            <kbd>⏎</kbd> run
          </span>
          <span className="lab">
            <kbd>esc</kbd> close
          </span>
          {GESTURES.map((g) => (
            <span key={g} className="lab">
              {g}
            </span>
          ))}
        </footer>
      </div>
    </div>
  )
}

export function Palette() {
  const w = useWorld()
  return w.paletteOpen ? <Sheet /> : null
}
