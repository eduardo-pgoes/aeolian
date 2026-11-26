import { Terminal, type ITerminalOptions } from '@xterm/xterm'
import { DIALS } from './dials'
import { termTheme } from './termTheme'

export interface Cell {
  w: number
  h: number
}

export function termOptions(name: string): ITerminalOptions {
  return {
    fontFamily: '"JetBrains Mono Variable", monospace',
    fontSize: DIALS.termFont,
    lineHeight: DIALS.termLineHeight,
    cursorBlink: false,
    cursorStyle: 'bar',
    cursorInactiveStyle: 'none',
    scrollback: 1000,
    theme: termTheme(name)
  }
}

const PROBE = 10

export async function measureCell(): Promise<Cell> {
  await document.fonts.load(`${DIALS.termFont}px "JetBrains Mono Variable"`).catch(() => [])
  const box = document.createElement('div')
  box.style.cssText = 'position:fixed;left:-10000px;top:0;visibility:hidden'
  document.body.append(box)
  const t = new Terminal({ ...termOptions(''), cols: PROBE, rows: PROBE })
  t.open(box)
  const r = box.querySelector('.xterm-screen')!.getBoundingClientRect()
  t.dispose()
  box.remove()
  return { w: r.width / PROBE, h: r.height / PROBE }
}

// xterm maps the mouse to a cell with the screen's on-screen box but its
// unscaled cell size, so under the canvas zoom every click and drag lands on
// the wrong cell. This puts mouse events back in the screen's own pixels
// before xterm reads them. A drag keeps the screen it started on, since
// xterm follows it on the document.
let dragging: HTMLElement | null = null

function unscale(e: MouseEvent): void {
  const target = e.target instanceof Element ? e.target : null
  const own = target?.closest('.win.focused .xterm')?.querySelector<HTMLElement>('.xterm-screen') ?? null
  if (e.type === 'mousedown') dragging = own
  const screen = dragging ?? own
  if (e.type === 'mouseup') dragging = null
  if (!screen) return
  const r = screen.getBoundingClientRect()
  const sx = r.width / (screen.offsetWidth || r.width)
  const sy = r.height / (screen.offsetHeight || r.height)
  if (Math.abs(sx - 1) < 1e-3 && Math.abs(sy - 1) < 1e-3) return
  const x = r.left + (e.clientX - r.left) / sx
  const y = r.top + (e.clientY - r.top) / sy
  Object.defineProperty(e, 'clientX', { value: x })
  Object.defineProperty(e, 'clientY', { value: y })
  Object.defineProperty(e, 'pageX', { value: x + window.scrollX })
  Object.defineProperty(e, 'pageY', { value: y + window.scrollY })
}

for (const type of ['mousedown', 'mousemove', 'mouseup', 'click', 'dblclick', 'contextmenu', 'wheel']) {
  window.addEventListener(type, (e) => unscale(e as MouseEvent), { capture: true })
}
