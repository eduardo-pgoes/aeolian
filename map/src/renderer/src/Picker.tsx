import { useEffect, useMemo, useRef, useState } from 'react'
import type { AccountsView, WorkflowEntry, WorkflowRef } from '../../main/host'
import { closePicker } from './field'
import { followNewest } from './nav'
import { Icon } from './icons'

interface Choice {
  key: string
  name: string
  description: string
  steps: string[]
  error: string
  ref: WorkflowRef
}

const FREEFORM: Choice = {
  key: 'builtin:freeform',
  name: 'freeform',
  description: 'one lead agent that spawns whatever you ask for',
  steps: ['lead'],
  error: '',
  ref: { builtin: 'freeform' }
}

const two = (n: number): string => String(n).padStart(2, '0')

function freeformGoal(at: Date): string {
  return `freeform · ${two(at.getHours())}:${two(at.getMinutes())}`
}

function choiceOf(e: WorkflowEntry): Choice {
  return {
    key: `folder:${e.name}`,
    name: e.name,
    description: e.description,
    steps: e.steps,
    error: e.error,
    ref: { contents: e.contents }
  }
}

const keepKeys = (e: React.KeyboardEvent): void => e.stopPropagation()

const RECENT_KEY = 'aeolian.picker.recent'
const RECENT_MAX = 5

function readRecent(): string[] {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(RECENT_KEY) ?? '[]')
    return Array.isArray(v) ? v.filter((k): k is string => typeof k === 'string') : []
  } catch {
    return []
  }
}

function pushRecent(key: string): void {
  if (key === FREEFORM.key) return
  const next = [key, ...readRecent().filter((k) => k !== key)].slice(0, RECENT_MAX)
  try {
    localStorage.setItem(RECENT_KEY, JSON.stringify(next))
  } catch {
    return
  }
}

function score(c: Choice, terms: string[]): number {
  const name = c.name.toLowerCase()
  const desc = c.description.toLowerCase()
  const steps = c.steps.map((s) => s.toLowerCase())
  let total = 0
  for (const t of terms) {
    if (name === t) total += 8
    else if (name.startsWith(t) || name.includes(`-${t}`)) total += 6
    else if (name.includes(t)) total += 4
    else if (steps.some((s) => s.includes(t))) total += 2
    else if (desc.includes(t)) total += 1
    else return 0
  }
  return total
}

function prefixOf(name: string): string {
  const i = name.indexOf('-')
  return i > 0 ? name.slice(0, i) : ''
}

interface Section {
  id: string
  title: string
  rows: Choice[]
}

function grouped(choices: Choice[]): Section[] {
  const by = new Map<string, Choice[]>()
  for (const c of choices) {
    const p = prefixOf(c.name)
    by.set(p, [...(by.get(p) ?? []), c])
  }
  const groups: Section[] = []
  const other: Choice[] = []
  for (const [p, rows] of [...by].sort(([a], [b]) => a.localeCompare(b))) {
    if (p && rows.length > 1) groups.push({ id: `g:${p}`, title: p, rows })
    else other.push(...rows)
  }
  if (other.length)
    groups.push({
      id: 'g:',
      title: 'other',
      rows: other.sort((a, b) => a.name.localeCompare(b.name))
    })
  return groups
}

// Enter starts the run; Shift+Enter breaks the line.
function submitOnEnter(e: React.KeyboardEvent<HTMLTextAreaElement>): void {
  if (e.key !== 'Enter' || e.shiftKey || e.nativeEvent.isComposing) return
  e.preventDefault()
  e.currentTarget.form?.requestSubmit()
}

function Goal({ c, accounts, onCancel }: { c: Choice; accounts: AccountsView; onCancel: () => void }) {
  const builtin = 'builtin' in c.ref
  const [goal, setGoal] = useState('')
  const [account, setAccount] = useState(accounts.default ?? '')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const label = builtin ? freeformGoal(new Date()) : ''
  const ready = builtin || goal.trim() !== ''

  const start = (e: React.FormEvent): void => {
    e.preventDefault()
    if (!ready || busy) return
    setBusy(true)
    setError('')
    window.map.startRun(c.ref, goal.trim() || freeformGoal(new Date()), account).then(
      () => {
        pushRecent(c.key)
        followNewest()
      },
      (err) => {
        setError(String(err))
        setBusy(false)
      }
    )
  }

  return (
    <form
      className="e-body k-goal"
      onSubmit={start}
      onKeyDown={(e) => {
        e.stopPropagation()
        if (e.key === 'Escape') onCancel()
      }}
      onKeyUp={keepKeys}
    >
      <textarea
        autoFocus
        rows={3}
        value={goal}
        placeholder={builtin ? label : 'the goal for this run'}
        aria-label={`goal for ${c.name}`}
        spellCheck={false}
        onKeyDown={submitOnEnter}
        onChange={(e) => setGoal(e.target.value)}
      />
      <div className="e-acts">
        {accounts.accounts.length > 0 && (
          <div className="k-account" role="radiogroup" aria-label="Claude account for this run">
            {[...(accounts.default ? [] : ['']), ...accounts.accounts].map((a) => (
              <button
                key={a || 'host'}
                type="button"
                role="radio"
                aria-checked={account === a}
                title={a ? `run on the ${a} account` : "run on the host's own account"}
                onClick={() => setAccount(a)}
              >
                {a || 'host'}
              </button>
            ))}
          </div>
        )}
        <button className="btn pri sm" type="submit" disabled={!ready || busy}>
          start
        </button>
      </div>
      {error && <p className="error">{error}</p>}
    </form>
  )
}

function Entry({
  c,
  row,
  open,
  hot,
  onOpen,
  onHover,
  onCancel,
  accounts
}: {
  c: Choice
  row: string
  open: boolean
  hot: boolean
  onOpen: () => void
  onHover: () => void
  onCancel: () => void
  accounts: AccountsView
}) {
  const broken = c.error !== ''
  return (
    <li
      className={`entry${broken ? ' broken' : ''}${open ? ' open' : ''}${hot ? ' hot' : ''}`}
      data-row={row}
      onMouseMove={broken || hot ? undefined : onHover}
    >
      <button className="e-head" aria-expanded={open} disabled={broken} onClick={onOpen}>
        <span className="e-goal">{c.name}</span>
        <span className="e-meta">
          <span className="e-step">{c.steps.join(' · ')}</span>
        </span>
      </button>
      {broken ? (
        <p className="error k-note">{c.error}</p>
      ) : (
        c.description && <p className="k-note">{c.description}</p>
      )}
      {open && <Goal c={c} accounts={accounts} onCancel={onCancel} />}
    </li>
  )
}

export function Picker() {
  const [listed, setListed] = useState<WorkflowEntry[]>([])
  const [listError, setListError] = useState('')
  const [open, setOpen] = useState<string | null>(null)
  const [accounts, setAccounts] = useState<AccountsView>({ accounts: [] })
  const [query, setQuery] = useState('')
  const [hot, setHot] = useState<string | null>(null)
  const [recent] = useState(readRecent)
  const search = useRef<HTMLInputElement>(null)
  const list = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let live = true
    window.map.workflows().then(
      (entries) => live && setListed(entries),
      (err) => live && setListError(String(err))
    )
    window.map.accounts().then(
      (v) => live && setAccounts(v),
      () => undefined
    )
    return () => void (live = false)
  }, [])

  const q = query.trim().toLowerCase().split(/\s+/).filter(Boolean).join(' ')

  const sections = useMemo<Section[]>(() => {
    const terms = q ? q.split(' ') : []
    const workflows = listed.map(choiceOf)
    if (terms.length) {
      const ranked = [FREEFORM, ...workflows]
        .map((c) => ({ c, n: score(c, terms) }))
        .filter((r) => r.n > 0)
        .sort((a, b) => b.n - a.n || a.c.name.localeCompare(b.c.name))
        .map((r) => r.c)
      return [{ id: 'q', title: '', rows: ranked }]
    }
    const byKey = new Map(workflows.map((c) => [c.key, c]))
    const recents = recent.flatMap((k) => byKey.get(k) ?? [])
    return [
      { id: 'pin', title: '', rows: [FREEFORM] },
      ...(recents.length ? [{ id: 'recent', title: 'recent', rows: recents }] : []),
      ...grouped(workflows)
    ]
  }, [listed, recent, q])

  const rows = sections.flatMap((s) => s.rows.filter((c) => !c.error).map((c) => `${s.id}|${c.key}`))
  const current = hot && rows.includes(hot) ? hot : (rows[0] ?? null)

  useEffect(() => {
    if (!current) return
    list.current?.querySelector(`[data-row="${CSS.escape(current)}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [current])

  const refocus = (): void => {
    setOpen(null)
    search.current?.focus()
  }

  const clear = (): void => {
    setQuery('')
    setHot(null)
    search.current?.focus()
  }

  const onSearchKey = (e: React.KeyboardEvent<HTMLInputElement>): void => {
    e.stopPropagation()
    if (e.nativeEvent.isComposing) return
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault()
      if (!rows.length) return
      const at = current ? rows.indexOf(current) : -1
      const step = e.key === 'ArrowDown' ? 1 : -1
      setHot(rows[(at + step + rows.length) % rows.length])
    } else if (e.key === 'Enter') {
      e.preventDefault()
      if (current) setOpen(current)
    } else if (e.key === 'Escape') {
      e.preventDefault()
      if (query) {
        setQuery('')
        setHot(null)
      } else closePicker()
    }
  }

  const empty = sections.every((s) => s.rows.length === 0)

  return (
    <section className="inbox picker">
      <header className="f-head">
        <h2>start a run</h2>
        <button className="btn quiet sm" onClick={closePicker}>
          cancel
        </button>
      </header>
      <label className="k-search">
        <Icon name="search" />
        <input
          ref={search}
          autoFocus
          type="text"
          value={query}
          placeholder="search workflows by name, step or description"
          aria-label="search workflows"
          spellCheck={false}
          autoComplete="off"
          onKeyDown={onSearchKey}
          onKeyUp={keepKeys}
          onChange={(e) => {
            setQuery(e.target.value)
            setHot(null)
          }}
        />
        {query && (
          <button className="btn quiet sm" type="button" tabIndex={-1} onClick={clear}>
            clear
          </button>
        )}
      </label>
      <div ref={list}>
        {sections.map(
          (s) =>
            s.rows.length > 0 && (
              <div key={s.id} className="k-sec">
                {s.title && <h3 className="k-grp">{s.title}</h3>}
                <ol className="entries">
                  {s.rows.map((c) => {
                    const row = `${s.id}|${c.key}`
                    return (
                      <Entry
                        key={row}
                        c={c}
                        row={row}
                        accounts={accounts}
                        open={open === row}
                        hot={current === row}
                        onHover={() => setHot(row)}
                        onCancel={refocus}
                        onOpen={() => {
                          setHot(row)
                          setOpen(open === row ? null : row)
                        }}
                      />
                    )
                  })}
                </ol>
              </div>
            )
        )}
        {empty && <p className="e-none k-none">nothing matches “{query.trim()}”</p>}
      </div>
      {listError && <p className="error">{listError}</p>}
    </section>
  )
}
