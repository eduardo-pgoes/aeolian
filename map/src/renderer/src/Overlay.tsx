import { useEffect, useRef, useState } from 'react'
import { Keys } from './Booklet'
import { Digest } from './Digest'
import { Inbox } from './Inbox'
import { chordOf } from './keymap'
import { closeSheet, openSheet } from './nav'
import { Scrim, Sheet } from './Sheet'
import { type SheetName, useWorld } from './store'

const LEAVE_MS = 160

const close = (): void => closeSheet()

function Tabs({ on, waiting }: { on: SheetName; waiting: number }) {
  return (
    <div className="seg" role="tablist" aria-label="sheets">
      <button role="tab" aria-selected={on === 'digest'} className={on === 'digest' ? 'on' : ''} onClick={() => openSheet('digest')}>
        digest
        <Keys chord={chordOf('digest')} />
      </button>
      <button role="tab" aria-selected={on === 'inbox'} className={on === 'inbox' ? 'on' : ''} onClick={() => openSheet('inbox')}>
        inbox
        {waiting > 0 && <span className="ct">{waiting}</span>}
        <Keys chord={chordOf('inbox')} />
      </button>
    </div>
  )
}

export function Overlay() {
  const w = useWorld()
  const name = w.sheet
  const prev = useRef<SheetName | null>(name)
  const [leaving, setLeaving] = useState<SheetName | null>(null)

  useEffect(() => {
    const was = prev.current
    prev.current = name
    if (name || !was) {
      setLeaving(null)
      return
    }
    setLeaving(was)
    const t = setTimeout(() => setLeaving(null), LEAVE_MS)
    return () => clearTimeout(t)
  }, [name])

  const tab = name ?? leaving
  if (!tab) return null

  return (
    <div className={`over-wrap${name ? '' : ' out'}`} inert={!name}>
      <Scrim onClose={close} />
      <Sheet
        label={tab}
        dock="over"
        modal
        focusKey={tab}
        onClose={close}
        head={
          <>
            <Tabs on={tab} waiting={w.inbox.length} />
            <span className="grow" />
            <span className="lab sheet-hint">esc closes</span>
          </>
        }
      >
        {tab === 'digest' ? <Digest /> : <Inbox />}
      </Sheet>
    </div>
  )
}
