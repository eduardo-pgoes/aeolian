import type { ITheme, Terminal } from '@xterm/xterm'
import { ink, isInk } from './inks'
import { PAPER, type Scheme } from '../../shared/paper'
import { onTheme, theme } from './theme'

export type { Scheme }

interface Palette {
  ansi: readonly string[]
  fg: string
  bg: string
}

// The harp mockup's --a0..--a15 per scheme; fg and bg come from PAPER, which
// the tmux styles in main share.
const PALETTES: Record<Scheme, Palette> = {
  dark: {
    ansi: [
      '#1c1f24', '#ff6b60', '#8fcb5b', '#f0c35a', '#58aee6', '#b192f2', '#5fcfc3', '#c8c5bd',
      '#62656c', '#ff9087', '#addf80', '#ffd986', '#86c7f0', '#cbb2f8', '#8ce2d8', '#f4f2ec'
    ],
    ...PAPER.dark
  },
  light: {
    ansi: [
      '#1f2125', '#c4372d', '#3d7d21', '#986700', '#1f6fa3', '#6c4cc0', '#127f78', '#8b8880',
      '#7a7c82', '#de4a3e', '#4c962c', '#b37f00', '#2a84b6', '#8466d6', '#169890', '#c9c6be'
    ],
    ...PAPER.light
  }
}

const NAMES = [
  'black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
  'brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
  'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'
] as const

// An agent's ink (by name) colors its cursor and selection, resolved for the
// current theme. Plain terminals use the foreground.
export function termTheme(name: string, s: Scheme = theme()): ITheme {
  const p = PALETTES[s]
  const mark = isInk(name) ? ink(name) : p.fg
  const out: ITheme = {
    foreground: p.fg,
    background: p.bg,
    cursor: mark,
    cursorAccent: p.bg,
    selectionBackground: `${mark}40`
  }
  NAMES.forEach((n, i) => (out[n] = p.ansi[i]))
  return out
}

const live = new Map<Terminal, string>()

onTheme(() => {
  const s = theme()
  for (const [term, name] of live) {
    if (term.element && !term.element.isConnected) live.delete(term)
    else term.options.theme = termTheme(name, s)
  }
})

// Keeps a terminal's theme in step with <html data-theme>. A disposed
// terminal drops out on the next theme change.
export function track(term: Terminal, name: string): void {
  live.set(term, name)
}
