import { BrowserWindow } from 'electron'
import { BASE, type HostEvent } from './host'

// The host's event stream, read in the main process and handed to every
// window: 'host:event' for each event, 'host:stream' when the connection
// comes or goes. Windows treat an event only as a cue to re-poll, so a
// missed one costs nothing but latency.

const MIN_BACKOFF_MS = 500
const MAX_BACKOFF_MS = 10_000
// The host pings every 15s; a stream silent for longer than this is dead.
const SILENT_MS = 45_000

let lastId: number | undefined
let connected = false
let stopped = false
let current: AbortController | undefined

function broadcast(channel: string, payload: unknown): void {
  for (const win of BrowserWindow.getAllWindows()) {
    if (!win.isDestroyed()) win.webContents.send(channel, payload)
  }
}

function setConnected(c: boolean): void {
  if (c === connected) return
  connected = c
  broadcast('host:stream', { connected })
}

export function streamState(): { connected: boolean } {
  return { connected }
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

interface Frame {
  id?: string
  event: string
  data: string
}

function dispatch(f: Frame): void {
  if (f.id !== undefined && /^\d+$/.test(f.id)) lastId = Number(f.id)
  if (f.event === 'reset') {
    // The cursor was too far behind: carry on from the newest event and
    // have the windows reload everything.
    try {
      lastId = (JSON.parse(f.data) as { last_id: number }).last_id
    } catch {
      /* keep the old cursor */
    }
    broadcast('host:event', { id: lastId ?? 0, kind: 'reset', at: new Date().toISOString(), data: {} })
    return
  }
  if (!f.data) return
  try {
    broadcast('host:event', JSON.parse(f.data) as HostEvent)
  } catch {
    /* a frame we cannot read is still a cue to re-poll */
    broadcast('host:event', { id: lastId ?? 0, kind: f.event || 'message', at: new Date().toISOString(), data: {} })
  }
}

// read parses the text/event-stream body until it ends, calling alive on
// every chunk so the watchdog knows the stream is still there.
async function read(body: ReadableStream<Uint8Array>, alive: () => void): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  let frame: Frame = { event: '', data: '' }
  for (;;) {
    const { done, value } = await reader.read()
    if (done) return
    alive()
    buf += decoder.decode(value, { stream: true })
    let nl: number
    while ((nl = buf.indexOf('\n')) >= 0) {
      const line = buf.slice(0, nl).replace(/\r$/, '')
      buf = buf.slice(nl + 1)
      if (line === '') {
        if (frame.event || frame.data || frame.id !== undefined) dispatch(frame)
        frame = { event: '', data: '' }
        continue
      }
      if (line.startsWith(':')) continue
      const colon = line.indexOf(':')
      const field = colon < 0 ? line : line.slice(0, colon)
      let value = colon < 0 ? '' : line.slice(colon + 1)
      if (value.startsWith(' ')) value = value.slice(1)
      if (field === 'id') frame.id = value
      else if (field === 'event') frame.event = value
      else if (field === 'data') frame.data = frame.data ? frame.data + '\n' + value : value
    }
  }
}

async function loop(): Promise<void> {
  let backoff = MIN_BACKOFF_MS
  while (!stopped) {
    const ctrl = new AbortController()
    current = ctrl
    let watchdog: ReturnType<typeof setTimeout> | undefined
    const alive = (): void => {
      clearTimeout(watchdog)
      watchdog = setTimeout(() => ctrl.abort(), SILENT_MS)
    }
    try {
      const headers: Record<string, string> = { accept: 'text/event-stream' }
      if (lastId !== undefined) headers['last-event-id'] = String(lastId)
      alive()
      const resp = await fetch(BASE + '/api/events/stream', { headers, signal: ctrl.signal })
      if (!resp.ok || !resp.body) throw new Error(`events: ${resp.status}`)
      setConnected(true)
      backoff = MIN_BACKOFF_MS
      await read(resp.body, alive)
    } catch {
      /* host down or stream cut: retry below */
    } finally {
      clearTimeout(watchdog)
    }
    setConnected(false)
    if (stopped) return
    await sleep(backoff)
    backoff = Math.min(backoff * 2, MAX_BACKOFF_MS)
  }
}

export function start(): void {
  stopped = false
  void loop()
}

export function stop(): void {
  stopped = true
  current?.abort()
}
