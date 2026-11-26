import { useState } from 'react'
import type { InboxEntry } from '../../main/host'
import { showOnCanvas } from './nav'
import { commit, useWorld, world } from './store'

const two = (n: number): string => String(n).padStart(2, '0')

function finishTime(iso: string | null): string {
  if (!iso) return ''
  const at = new Date(iso)
  const time = `${two(at.getHours())}:${two(at.getMinutes())}`
  if (at.toDateString() === new Date().toDateString()) return time
  return `${at.getFullYear()}-${two(at.getMonth() + 1)}-${two(at.getDate())} ${time}`
}

function ack(runId: string): void {
  world.actionError = ''
  commit()
  window.map.ack(runId).catch((err) => {
    world.actionError = String(err)
    commit()
  })
}

function Entry({ e }: { e: InboxEntry }) {
  const [open, setOpen] = useState(false)
  const worktrees = e.worktrees ?? []
  const tail = e.state === 'failed' ? (e.log_tail ?? []) : []

  return (
    <li className={`entry ${e.state}${open ? ' open' : ''}`}>
      <button className="e-head" aria-expanded={open} title={`${e.goal}\n${e.run_id}`} onClick={() => setOpen(!open)}>
        <span className="e-goal">{e.goal}</span>
        <span className="e-meta">
          <span className={`state ${e.state}`}>
            {e.reason && e.reason !== e.state ? `${e.state} · ${e.reason}` : e.state}
          </span>
          <span className="e-step">{e.step}</span>
          <span className="e-time">{finishTime(e.finished_at)}</span>
        </span>
      </button>
      <div className="e-acts">
        {e.source && <span className="e-wait">waiting on {e.source}</span>}
        <button className="btn quiet sm" onClick={() => showOnCanvas(e.run_id)}>
          put on canvas
        </button>
        {!e.source && (
          <button className="btn quiet sm" title="Take this run out of the inbox" onClick={() => ack(e.run_id)}>
            ack
          </button>
        )}
      </div>
      {open && (
        <div className="e-body">
          {e.handoff ? <p className="e-handoff">{e.handoff}</p> : <p className="e-none">no handoff</p>}
          {worktrees.length > 0 && (
            <ul className="e-trees">
              {worktrees.map((t) => (
                <li key={t.path}>
                  <button className="pathb" title="Copy the path" onClick={() => window.map.copy(t.path)}>
                    {t.path}
                  </button>
                  <span>{t.branch}</span>
                </li>
              ))}
            </ul>
          )}
          {tail.length > 0 && <pre className="e-tail">{tail.join('\n')}</pre>}
        </div>
      )}
    </li>
  )
}

export function Inbox() {
  const w = useWorld()

  if (!w.inbox.length) {
    return (
      <div className="sh-empty">
        <h2>nothing waiting</h2>
        <p>
          finished runs land here to ack or put back on the canvas · <kbd>esc</kbd> closes
        </p>
      </div>
    )
  }

  return (
    <>
      <header className="f-head">
        <h2>
          {w.inbox.length} finished {w.inbox.length === 1 ? 'run' : 'runs'} waiting
        </h2>
      </header>
      {w.actionError && <p className="error">{w.actionError}</p>}
      <ol className="entries">
        {w.inbox.map((e) => (
          <Entry key={e.run_id} e={e} />
        ))}
      </ol>
    </>
  )
}
