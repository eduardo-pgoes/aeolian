import { useEffect, useRef } from 'react'
import { Booklet } from './Booklet'
import { stage } from './canvas/stage'
import { DIALS } from './dials'
import { Arena } from './Arena'
import { togglePalette, toggleSheet } from './nav'
import { Overlay } from './Overlay'
import { Palette } from './Palette'
import { Picker } from './Picker'
import { bind } from './keymap'
import { canvasRun, commit, useWorld, world } from './store'

let polling: Promise<void> | null = null
let again = false

// poll reads the world again; a poll asked for while one runs makes one
// more run after it, never two at once.
function poll(): Promise<void> {
  if (polling) {
    again = true
    return polling
  }
  polling = pollOnce().finally(() => {
    polling = null
    if (again) {
      again = false
      void poll()
    }
  })
  return polling
}

async function pollOnce(): Promise<void> {
  const info = window.map.hostInfo().catch(() => undefined)
  window.map.inbox().then(
    (entries) => {
      world.inbox = entries
      commit()
    },
    () => {}
  )
  try {
    world.runs = await window.map.listRuns(world.pinned)
    world.error = ''
  } catch (err) {
    world.error = String(err)
  }
  world.cap = (await info)?.max_agents ?? world.cap
  world.loaded = true
  commit()
  stage.sync(canvasRun())
}

export function App() {
  const host = useRef<HTMLDivElement>(null)
  const field = useWorld().field

  useEffect(() => {
    stage.mount(host.current!)
    bind('inbox', () => toggleSheet('inbox'))
    bind('digest', () => toggleSheet('digest'))
    bind('palette', togglePalette)
    void poll()
    // Host events only schedule a poll; poll stays the one way state is
    // read. With the stream up, the interval is just a fallback.
    let t = setInterval(poll, DIALS.pollMs)
    let pending = 0
    const kick = (): void => {
      if (!pending) {
        pending = window.setTimeout(() => {
          pending = 0
          void poll()
        }, DIALS.eventDebounceMs)
      }
    }
    const rate = (connected: boolean): void => {
      clearInterval(t)
      t = setInterval(poll, connected ? DIALS.pollFallbackMs : DIALS.pollMs)
    }
    const offEvent = window.map.onEvent(kick)
    const offStream = window.map.onStream(({ connected }) => {
      rate(connected)
      if (connected) kick()
    })
    window.map.streamState().then(({ connected }) => rate(connected), () => {})
    return () => {
      clearInterval(t)
      clearTimeout(pending)
      offEvent()
      offStream()
    }
  }, [])

  return (
    <div className="app">
      <main className="field" data-field={field}>
        <div ref={host} className="stage" inert={field !== 'canvas'} />
        {field === 'picker' && <Picker />}
        {field === 'arena' && <Arena />}
      </main>
      <Overlay />
      <Booklet />
      <Palette />
    </div>
  )
}
