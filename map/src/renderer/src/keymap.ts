export type Action = 'leave' | 'pinDrawer' | 'inbox' | 'digest' | 'theme' | 'palette'

export const KEYMAP: { chord: string; action: Action; label: string }[] = [
  { chord: 'ctrl+shift+k', action: 'palette', label: 'commands and keys' },
  { chord: 'ctrl+shift+f', action: 'leave', label: 'leave the terminal, fit the run' },
  { chord: 'ctrl+shift+b', action: 'pinDrawer', label: 'pin the booklet' },
  { chord: 'ctrl+shift+o', action: 'inbox', label: 'open or close the inbox' },
  { chord: 'ctrl+shift+d', action: 'digest', label: 'open or close the digest' },
  { chord: 'ctrl+shift+t', action: 'theme', label: 'flip the theme · third press follows the OS again' }
]

export const GESTURES = ['click a window to type into it · a card to read it', 'drag or space to pan · wheel to zoom']

const actions: Partial<Record<Action, () => void>> = {}

export function bind(action: Action, run: () => void): void {
  actions[action] = run
}

export function trigger(action: Action): void {
  actions[action]?.()
}

export function chordOf(action: Action): string | undefined {
  return KEYMAP.find((r) => r.action === action)?.chord
}

export function chordKeys(chord: string): string[] {
  return chord.split('+').map((k) => (k === 'shift' ? '⇧' : k.length === 1 ? k.toUpperCase() : k))
}

function pressed(e: KeyboardEvent): string {
  const mods = [e.ctrlKey && 'ctrl', e.altKey && 'alt', e.shiftKey && 'shift', e.metaKey && 'meta'].filter(Boolean)
  return [...mods, e.key.toLowerCase()].join('+')
}

export function chordAction(e: KeyboardEvent): Action | undefined {
  return KEYMAP.find((r) => r.chord === pressed(e))?.action
}

export function onKey(e: KeyboardEvent): boolean {
  const row = KEYMAP.find((r) => r.chord === pressed(e))
  if (!row) return false
  e.preventDefault()
  e.stopPropagation()
  if (e.type === 'keydown') actions[row.action]?.()
  return true
}
