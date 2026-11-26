import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { Agent, Run } from '../../main/host'
import { stage } from './canvas/stage'
import { closePicker, openPicker } from './field'
import { agentName, look, stateLabel } from './canvas/win'
import { Icon } from './icons'
import { ink } from './inks'
import { bind, chordKeys, chordOf } from './keymap'
import { isLive, isWorking } from './layout'
import { mark, markOf } from './marks'
import { closeBooklet, flyTo, openBooklet, pin, togglePalette, togglePin, toggleSheet } from './nav'
import { Ogi } from './Ogi'
import { useTheme } from './theme'
import { canvasRun, commit, useWorld, world } from './store'

const GUTTER = 24
const RAIL_MARKS = 6

const two = (n: number): string => String(n).padStart(2, '0')

function clock(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000))
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  return h ? `${h}:${two(m)}:${two(s % 60)}` : `${m}:${two(s % 60)}`
}

function ago(iso: string | undefined): string {
  if (!iso) return ''
  const m = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 60000))
  if (m < 60) return `${m}m`
  const h = Math.round(m / 60)
  return h < 48 ? `${h}h` : `${Math.round(h / 24)}d`
}

function Num({ v }: { v: number | string }) {
  return <span className="num">{v}</span>
}

export function Keys({ chord }: { chord?: string }) {
  if (!chord) return null
  return (
    <span className="keys">
      {chordKeys(chord).map((c) => (
        <kbd key={c}>{c}</kbd>
      ))}
    </span>
  )
}

function RunTime({ run }: { run?: Run }) {
  const [, tick] = useState(0)
  const live = !!run && !run.done_at
  useEffect(() => {
    if (!live) return
    const t = setInterval(() => tick((n) => n + 1), 250)
    return () => clearInterval(t)
  }, [live])
  const start = run ? Date.parse(run.created_at) : 0
  const end = run?.done_at ? Date.parse(run.done_at) : Date.now()
  return <span className="num">{clock(run ? end - start : 0)}</span>
}

export function killRun(runId: string): void {
  world.actionError = ''
  window.map.killRun(runId).catch((err) => {
    world.actionError = String(err)
    commit()
  })
}

export function endRun(runId: string, now = false): void {
  world.actionError = ''
  window.map.endRun(runId, now).catch((err) => {
    world.actionError = String(err)
    commit()
  })
}

export const busy = (r: Run): boolean => r.state === 'busy' || r.state === 'queued'

export function runNote(r: Run): string {
  const live = r.agents.filter(isLive).length
  if (live) return `${live} live · ${ago(r.created_at)}`
  const why = r.reason && r.reason !== r.state ? `${r.state} · ${r.reason}` : r.state
  return `${why} · ${ago(r.done_at ?? r.created_at)}`
}

function InboxEntry({ count, on, rail }: { count: number; on: boolean; rail?: boolean }) {
  const what = `${count} finished ${count === 1 ? 'run' : 'runs'} waiting`
  return (
    <button
      className={`inb${rail ? ' rail-inb' : ' btn quiet sm'}${count ? ' has' : ''}`}
      aria-pressed={on}
      aria-label={`inbox · ${what}`}
      title={`${count ? what : 'inbox · nothing waiting'} · ${on ? 'close the inbox' : 'open the inbox'} (ctrl+shift+o)`}
      onClick={() => toggleSheet('inbox')}
    >
      <Icon name="tray" />
      <Num v={count} />
    </button>
  )
}

function Rail({ live, cap, holders }: { live: number; cap: number; holders: Agent[] }) {
  const w = world
  const shown = holders.slice(0, RAIL_MARKS)
  const more = holders.length - shown.length
  const opener = (e: React.MouseEvent): void => {
    if ((e.target as HTMLElement).closest('button')) return
    openBooklet()
  }
  return (
    <div className="rail" onClick={opener}>
      <Ogi px={24} xs={false} play className="r-logo" />
      <span className="rl" />
      <div className="pw" title={`${live} of ${cap} agents working`}>
        <Num v={live} />
        <i>/{cap}</i>
      </div>
      <span className="rl" />
      <InboxEntry count={w.inbox.length} on={w.sheet === 'inbox'} rail />
      <div className="r-marks">
        {shown.map((a) => (
          <button
            key={a.id}
            className={`r-mark ${look(a)}`}
            aria-label={`fly to ${agentName(a)}`}
            title={`${agentName(a)} · ${stateLabel(a)}`}
            style={{ '--c': ink(a.ink) } as React.CSSProperties}
            onClick={() => flyTo(a)}
            dangerouslySetInnerHTML={{ __html: mark(markOf(a)) }}
          />
        ))}
        {more > 0 && (
          <button className="r-more" title={`${more} more live · open the booklet`} onClick={openBooklet}>
            +{more}
          </button>
        )}
      </div>
      <span className="grow" />
      <button className="ib r-btn" aria-label="commands and keys" title="commands and keys (ctrl+shift+k)" onClick={togglePalette}>
        <Icon name="search" />
      </button>
      <button className="ib r-btn" aria-label="open the booklet" title="open the booklet · ctrl+shift+b pins it" onClick={openBooklet}>
        <Icon name="right" />
      </button>
    </div>
  )
}

export function Booklet() {
  const w = useWorld()
  useTheme()
  const run = canvasRun()
  const all = w.runs.flatMap((r) => r.agents).filter(isLive)
  const holders = [...all.filter((a) => a.blocked), ...all.filter((a) => !a.blocked)]
  const working = all.filter(isWorking)
  const live = working.length
  const beads = [...working, ...all.filter((a) => !isWorking(a))]
  const held = Math.min(all.length, Math.max(w.cap, live)) - live
  const tracks = [...(run?.agents ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at))
  const handoffs = run?.agents.filter((a) => a.handoff).length ?? 0
  const pinned = w.drawerPinned
  const shown = w.runs.filter((r) => busy(r) || r.id === run?.id)
  const open = pinned || w.bookletOpen
  const drawer = useRef<HTMLElement>(null)
  const rail = useRef<HTMLDivElement>(null)
  const book = useRef<HTMLDivElement>(null)

  useEffect(() => bind('pinDrawer', togglePin), [])

  useLayoutEffect(() => {
    const edge = pinned ? drawer.current! : rail.current!
    const inset = (): void => {
      const px = Math.round(edge.getBoundingClientRect().right + GUTTER)
      stage.setInset(px)
      document.documentElement.style.setProperty('--field-inset', `${px}px`)
    }
    inset()
    addEventListener('resize', inset)
    return () => removeEventListener('resize', inset)
  }, [pinned])

  useEffect(() => {
    if (w.bookletOpen && !book.current!.contains(document.activeElement)) book.current!.focus({ preventScroll: true })
  }, [w.bookletOpen])

  useEffect(() => {
    if (!w.bookletOpen) return
    const outside = (e: PointerEvent): void => {
      const t = e.target as HTMLElement
      if (drawer.current!.contains(t) || t.closest('.pal-wrap')) return
      closeBooklet(false)
    }
    const esc = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape' || e.defaultPrevented || world.paletteOpen) return
      const a = document.activeElement
      if (a && a !== document.body && !drawer.current!.contains(a)) return
      e.preventDefault()
      closeBooklet()
    }
    addEventListener('pointerdown', outside, true)
    addEventListener('keydown', esc)
    return () => {
      removeEventListener('pointerdown', outside, true)
      removeEventListener('keydown', esc)
    }
  }, [w.bookletOpen])

  return (
    <nav
      ref={drawer}
      aria-label="booklet"
      className={`drawer${open ? ' open' : ''}${pinned ? ' pinned' : ''}`}
    >
      <div className="leaf">
        <div ref={rail} className="rail-slot" inert={open}>
          <Rail live={live} cap={w.cap} holders={holders} />
        </div>

        <div ref={book} className="bk" inert={!open} tabIndex={-1}>
          <header className="bk-top">
            <h1>
              <Ogi />
              aeolian
            </h1>
            <span className="grow" />
            <InboxEntry count={w.inbox.length} on={w.sheet === 'inbox'} />
            <button
              className="ib"
              aria-pressed={pinned}
              aria-label={pinned ? 'unpin the booklet' : 'pin the booklet open'}
              title={`${pinned ? 'unpin' : 'pin open'} (ctrl+shift+b)`}
              onClick={togglePin}
            >
              <Icon name="pin" />
            </button>
            {!pinned && (
              <button className="ib" aria-label="close the booklet" title="close (esc)" onClick={() => closeBooklet()}>
                <Icon name="left" />
              </button>
            )}
          </header>

          <div className="bk-body">
            <section className="bk-sec">
              <div className="bk-h">
                <span className="lab">run</span>
                {run && <span className="rid">{run.id.slice(0, 8)}</span>}
              </div>
              {run ? (
                <>
                  <p className="run-goal" title={run.goal}>
                    {run.goal}
                  </p>
                  <div className="run-state">
                    <span className={`st${run.state === 'failed' || run.state === 'killed' ? ' fail' : ''}`}>
                      {run.reason && run.reason !== run.state ? `${run.state} · ${run.reason}` : run.state}
                    </span>
                    {run.account && (
                      <span className="account" title="Claude account for this run">
                        {run.account}
                      </span>
                    )}
                    {busy(run) && (
                      <span className="racts">
                        <button
                          className="btn quiet sm"
                          title="End this run as done: its lead is asked to hand back first (shift: at once)"
                          onClick={(e) => endRun(run.id, e.shiftKey)}
                        >
                          end
                        </button>
                        <button
                          className="btn quiet sm danger"
                          title="Stop this run and all its agents"
                          onClick={() => killRun(run.id)}
                        >
                          kill
                        </button>
                      </span>
                    )}
                  </div>
                </>
              ) : (
                w.loaded && !w.error && <p className="run-goal none">no runs yet</p>
              )}
              {w.error && <p className="error">{w.error}</p>}
              {w.actionError && <p className="error">{w.actionError}</p>}
            </section>

            <section className="bk-sec reads">
              <div className="read">
                <span className="lab">time</span>
                <RunTime run={run} />
              </div>
              <div className="read">
                <span className="lab">handoffs</span>
                <Num v={two(handoffs)} />
              </div>
              <div className="read power">
                <span className="lab">power</span>
                <span className="num">
                  {live}
                  <i>/{w.cap}</i>
                </span>
                <span
                  className="beads"
                  aria-label={`${live} working of ${w.cap}${held ? `, ${held} live but not working` : ''}`}
                >
                  {Array.from({ length: Math.max(w.cap, live) }, (_, i) => {
                    const a = i < live + held ? beads[i] : undefined
                    if (!a) return <i key={i} />
                    return (
                      <i
                        key={i}
                        className={i < live ? 'on' : 'held'}
                        title={agentName(a)}
                        style={{ '--c': ink(a.ink) } as React.CSSProperties}
                      />
                    )
                  })}
                </span>
              </div>
            </section>

            <section className="bk-sec tracks">
              <div className="bk-h">
                <span className="lab">
                  on the canvas {tracks.length > 0 && <b>{tracks.length}</b>}
                </span>
              </div>
              {run && !tracks.length && <p className="empty">nobody yet</p>}
              <ol>
                {tracks.map((a, i) => (
                  <li key={a.id}>
                    <button
                      className={`who ${look(a)}${w.focusId === a.id ? ' focused' : ''}${a.parent_id ? ' helper' : ''}`}
                      style={{ '--c': ink(a.ink) } as React.CSSProperties}
                      title={a.id}
                      onPointerEnter={() => stage.highlight(a.id, true)}
                      onPointerLeave={() => stage.highlight(a.id, false)}
                      onFocus={() => stage.highlight(a.id, true)}
                      onBlur={() => stage.highlight(a.id, false)}
                      onClick={() => flyTo(a)}
                    >
                      <span className="tn">{two(i + 1)}</span>
                      <span className="wsign" dangerouslySetInnerHTML={{ __html: mark(markOf(a)) }} />
                      <span className="wid">{agentName(a)}</span>
                      <span className="wstate">{stateLabel(a)}</span>
                    </button>
                  </li>
                ))}
              </ol>
            </section>

            <section className="bk-sec runs">
              <div className="bk-h">
                <span className="lab">
                  active runs {shown.length > 0 && <b>{shown.length}</b>}
                </span>
                <button
                  className="btn quiet sm newrun"
                  aria-pressed={w.field === 'picker'}
                  title={w.field === 'picker' ? 'Back to where you were' : 'Pick a workflow and a goal for a new run'}
                  onClick={w.field === 'picker' ? closePicker : openPicker}
                >
                  <Icon name="plus" />
                  new run
                </button>
              </div>
              <ol>
                {shown.length === 0 && <li className="rnote">nothing running</li>}
                {shown.map((r) => {
                  const on = r.id === run?.id
                  return (
                    <li key={r.id}>
                      <button
                        className={`run ${r.state}${on ? ' on' : ''}`}
                        aria-current={on}
                        title={`${r.goal}\n${r.id}`}
                        onClick={() => pin(r.id)}
                      >
                        <i className="dot" />
                        <span className="rwf">{r.goal}</span>
                        <span className="rnote">{on ? 'on canvas' : runNote(r)}</span>
                      </button>
                    </li>
                  )
                })}
              </ol>
            </section>
          </div>

          <footer className="bk-foot">
            <button className="btn quiet sm" onClick={togglePalette}>
              commands &amp; keys
              <Keys chord={chordOf('palette')} />
            </button>
            {pinned && <span className="lab">pinned</span>}
          </footer>
        </div>
      </div>
    </nav>
  )
}
