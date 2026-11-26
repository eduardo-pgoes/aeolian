// <html data-theme> is always 'dark' or 'light'. It follows the OS unless
// an override is set; the override survives restarts in localStorage.

import { useSyncExternalStore } from 'react'
import { bind } from './keymap'

export type Theme = 'dark' | 'light'

const KEY = 'aeolian.theme'
const os = window.matchMedia('(prefers-color-scheme: dark)')
let override: Theme | null = null
const listeners = new Set<() => void>()

function stored(): Theme | null {
  try {
    const v = localStorage.getItem(KEY)
    return v === 'dark' || v === 'light' ? v : null
  } catch {
    return null
  }
}

function apply(): void {
  const root = document.documentElement
  const t = theme()
  if (root.dataset.theme === t) return
  root.dataset.theme = t
  for (const cb of listeners) cb()
}

export function theme(): Theme {
  return override ?? (os.matches ? 'dark' : 'light')
}

// Calls cb after data-theme flips. Returns a function that stops it.
export function onTheme(cb: () => void): () => void {
  listeners.add(cb)
  return () => listeners.delete(cb)
}

export function useTheme(): Theme {
  return useSyncExternalStore(onTheme, theme)
}

// null clears the override and goes back to following the OS.
export function setTheme(t: Theme | null): void {
  override = t
  try {
    if (t) localStorage.setItem(KEY, t)
    else localStorage.removeItem(KEY)
  } catch {
    // Storage can be unavailable; the override then lasts for this session.
  }
  window.map.setTheme(t)
  apply()
}

// Follow OS -> the opposite of the OS -> the OS's own theme, pinned -> follow OS.
// The first two presses always flip what you see; the third only unpins.
export function cycleTheme(): void {
  const sys: Theme = os.matches ? 'dark' : 'light'
  const other: Theme = sys === 'dark' ? 'light' : 'dark'
  setTheme(override === null ? other : override === other ? sys : null)
}

export function themeNote(): { now: Theme; follows: boolean; next: string } {
  const sys: Theme = os.matches ? 'dark' : 'light'
  const other: Theme = sys === 'dark' ? 'light' : 'dark'
  const next = override === null ? other : override === other ? `${sys}, pinned` : 'follow the OS'
  return { now: theme(), follows: override === null, next }
}

export function initTheme(): void {
  bind('theme', cycleTheme)
  override = stored()
  window.map.setTheme(override)
  apply()
  os.addEventListener('change', apply)
  // Another window changed the override.
  window.addEventListener('storage', (e) => {
    if (e.key !== KEY) return
    override = stored()
    apply()
  })
}
