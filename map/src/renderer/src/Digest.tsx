import { useCallback, useEffect, useRef, useState } from 'react'
import type { DigestItem, DigestRun, DigestView } from '../../main/host'
import { Icon } from './icons'
import { showOnCanvas } from './nav'

// The return page: what needs the person from the runs of the last hours,
// ranked and capped by the host. It polls its own endpoint, since it is
// only mounted while open.
const SINCE = '12h'
const POLL_MS = 5000

const LABELS: Record<string, string> = {
  blocked: 'question',
  pick: 'your pick',
  dead: 'ended without delivering',
  contradicted: 'contradicted',
  helper_died: 'helpers died',
  stale_precedent: 'stale precedent',
  bad_precedent: 'precedent does not parse',
  unverifiable: 'unverifiable',
  unwitnessed: 'unwitnessed'
}

// Three kinds of item: a question holds a glowing edge, a failure gets a
// snapped one and the only red, everything else is a quiet note.
type Tone = 'q' | 'f' | 'n'
const tone = (kind: string): Tone =>
  kind === 'blocked' || kind === 'pick' ? 'q' : kind === 'dead' || kind === 'helper_died' ? 'f' : 'n'
const CHIP: Record<Tone, string> = { q: 'st ask', f: 'st fail out', n: 'st out' }

type Group = DigestView['groups'][number]

const occurrence = (it: DigestItem): string => `${it.key}@${it.at}`

const ackable = (run: DigestRun): boolean => !run.source && run.state !== 'busy' && run.state !== 'queued'

function token(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim()
}

function collapse(el: HTMLElement | null): Promise<void> {
  if (!el || matchMedia('(prefers-reduced-motion: reduce)').matches) return Promise.resolve()
  const cs = getComputedStyle(el)
  const ms = parseFloat(token('--d2')) || 240
  const a = el.animate(
    [
      {
        height: `${el.offsetHeight}px`,
        opacity: 1,
        marginBottom: cs.marginBottom,
        paddingTop: cs.paddingTop,
        paddingBottom: cs.paddingBottom,
        borderTopWidth: cs.borderTopWidth,
        borderBottomWidth: cs.borderBottomWidth
      },
      {
        height: '0px',
        opacity: 0,
        marginBottom: '0px',
        paddingTop: '0px',
        paddingBottom: '0px',
        borderTopWidth: '0px',
        borderBottomWidth: '0px'
      }
    ],
    { duration: ms, easing: token('--e-damp') || 'ease-out', fill: 'forwards' }
  )
  el.style.overflow = 'hidden'
  return a.finished.then(
    () => undefined,
    () => undefined
  )
}

function Item({ it, onDismiss }: { it: DigestItem; onDismiss: (el: HTMLElement | null) => void }) {
  const ref = useRef<HTMLLIElement>(null)
  const t = tone(it.kind)
  const paths = [it.action, it.kind === 'contradicted' || it.kind === 'unverifiable' ? '' : it.ref].filter(
    (s): s is string => !!s
  )
  return (
    <li ref={ref} className={`dg ${t}`}>
      <div className="dg-edge" />
      <div className="dg-in">
        <div className="dg-meta">
          <span className={CHIP[t]}>{LABELS[it.kind] ?? it.kind}</span>
          {(it.count ?? 0) > 1 && <span className="t">×{it.count}</span>}
          <button
            className="ib dg-x"
            aria-label="dismiss"
            title="Dismiss until it happens again"
            onClick={() => onDismiss(ref.current)}
          >
            <Icon name="close" />
          </button>
        </div>
        <p className={t === 'q' ? 'dg-q' : 'dg-t'}>{it.text}</p>
        {it.note && <p className="dg-note">{it.note}</p>}
        {(it.evidence?.length ?? 0) > 0 && <pre className="dg-tail">{it.evidence!.slice(0, 3).join('\n')}</pre>}
        {(it.run_id || paths.length > 0) && (
          <div className="dg-acts">
            {it.run_id && (
              <button className="btn quiet sm" onClick={() => showOnCanvas(it.run_id!)}>
                put on canvas
              </button>
            )}
            {paths.map((s) => (
              <button key={s} className="pathb" title="Copy" onClick={() => window.map.copy(s)}>
                {s}
              </button>
            ))}
          </div>
        )}
      </div>
    </li>
  )
}

function RunHead({ run, onClear }: { run: DigestRun; onClear: () => void }) {
  const failed = run.state === 'failed' || run.state === 'killed'
  const ack = ackable(run)
  return (
    <h3 className="grp-h">
      <button className="gn" title={`${run.goal}\nput on canvas`} onClick={() => showOnCanvas(run.id)}>
        {run.goal}
      </button>
      <span className={`st${failed ? ' fail' : ''}`}>{run.reason || run.state}</span>
      {run.precedents?.length ? <span className="pre">precedents: {run.precedents.join(', ')}</span> : null}
      <span className="lab">{run.id.slice(0, 8)}</span>
      <button
        className="btn quiet sm"
        title={ack ? 'Ack the run: out of the inbox and the digest' : 'Clear these items until they happen again'}
        onClick={onClear}
      >
        {ack ? 'ack' : 'clear'}
      </button>
    </h3>
  )
}

function counts(groups: DigestView['groups']): string {
  const n: Record<Tone, number> = { q: 0, f: 0, n: 0 }
  for (const g of groups) for (const it of g.items) n[tone(it.kind)]++
  const parts = [
    n.q && `${n.q} ${n.q === 1 ? 'question' : 'questions'}`,
    n.f && `${n.f} ${n.f === 1 ? 'failure' : 'failures'}`,
    n.n && `${n.n} ${n.n === 1 ? 'note' : 'notes'}`
  ]
  return parts.filter(Boolean).join(' · ')
}

export function Digest() {
  const [raw, setRaw] = useState<DigestView | null>(null)
  const [error, setError] = useState('')
  const [hidden, setHidden] = useState<ReadonlySet<string>>(new Set())
  const groupEls = useRef(new Map<string, HTMLElement>())

  const load = useCallback((): void => {
    window.map.digest(SINCE).then(
      (v) => {
        setRaw(v)
        setError('')
      },
      (err) => setError(String(err))
    )
  }, [])

  useEffect(() => {
    load()
    const t = setInterval(load, POLL_MS)
    return () => clearInterval(t)
  }, [load])

  const hide = (ids: string[], show: boolean): void =>
    setHidden((prev) => {
      const next = new Set(prev)
      for (const id of ids) {
        if (show) next.delete(id)
        else next.add(id)
      }
      return next
    })

  const clear = (items: DigestItem[], el: HTMLElement | null, send: () => Promise<unknown>): void => {
    const ids = items.map(occurrence)
    setError('')
    const sent = send().then(
      () => true,
      (err) => {
        setError(String(err))
        return false
      }
    )
    void collapse(el)
      .then(() => {
        hide(ids, false)
        return sent
      })
      .then((ok) => {
        if (!ok) hide(ids, true)
        load()
      })
  }

  const dismissItem = (g: Group, it: DigestItem, el: HTMLElement | null): void => {
    const last = g.items.length === 1 ? groupEls.current.get(g.run?.id ?? '') : undefined
    clear([it], last ?? el, () => window.map.dismissDigest([it.key]))
  }

  const clearGroup = (g: Group): void => {
    const el = groupEls.current.get(g.run?.id ?? '')
    if (g.run && ackable(g.run)) {
      const id = g.run.id
      clear(g.items, el ?? null, () => window.map.ack(id))
    } else {
      clear(g.items, el ?? null, () => window.map.dismissDigest(g.items.map((it) => it.key)))
    }
  }

  if (error && !raw) return <p className="error">{error}</p>
  if (!raw) return <div className="sh-empty" />
  let gone = 0
  const groups = raw.groups
    .map((g) => {
      const items = g.items.filter((it) => !hidden.has(occurrence(it)))
      gone += g.items.length - items.length
      return { ...g, items }
    })
    .filter((g) => g.items.length > 0)
  const view: DigestView = { ...raw, groups, shown: raw.shown - gone, dismissed: raw.dismissed + gone }
  const total = view.shown + view.overflow
  if (total === 0) {
    return (
      <div className="sh-empty">
        <h2>nothing needs you</h2>
        <p>
          {view.clean.length} {view.clean.length === 1 ? 'run' : 'runs'} in the last {SINCE} with nothing for you
          {view.dismissed > 0 && ` · ${view.dismissed} cleared`} ·{' '}
          <kbd>esc</kbd> closes
        </p>
      </div>
    )
  }

  return (
    <>
      <header className="f-head">
        <h2>
          {total} {total === 1 ? 'thing needs' : 'things need'} you
        </h2>
        <span className="lab">
          {counts(view.groups)}
          {view.running > 0 && ` · ${view.running} still going`}
          {view.dismissed > 0 && ` · ${view.dismissed} cleared`} · last {SINCE}
        </span>
      </header>
      {error && <p className="error">{error}</p>}
      {view.groups.map((g) => (
        <section
          key={g.run?.id ?? 'precedents'}
          className="grp"
          ref={(el) => {
            if (el) groupEls.current.set(g.run?.id ?? '', el)
            else groupEls.current.delete(g.run?.id ?? '')
          }}
        >
          {g.run ? (
            <RunHead run={g.run} onClear={() => clearGroup(g)} />
          ) : (
            <h3 className="grp-h">
              <span className="gn">precedents</span>
              <button
                className="btn quiet sm"
                title="Clear these items until they happen again"
                onClick={() => clearGroup(g)}
              >
                clear
              </button>
            </h3>
          )}
          <ol className="dgs">
            {g.items.map((it) => (
              <Item key={occurrence(it)} it={it} onDismiss={(el) => dismissItem(g, it, el)} />
            ))}
          </ol>
        </section>
      ))}
      {view.overflow > 0 && (
        <p className="f-note">
          +{view.overflow} more: <code>aeolian digest --max 0</code>
        </p>
      )}
      {view.clean.length > 0 && (
        <p className="f-note">nothing for you: {view.clean.map((r) => r.goal).join(' · ')}</p>
      )}
    </>
  )
}
