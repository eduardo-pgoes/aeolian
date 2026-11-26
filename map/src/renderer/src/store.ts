import { useSyncExternalStore } from 'react'
import type { InboxEntry, Run } from '../../main/host'
import type { Field } from './field'

export type SheetName = 'digest' | 'inbox'

export const world = {
  runs: [] as Run[],
  cap: 0,
  inbox: [] as InboxEntry[],
  field: 'canvas' as Field,
  pickerFrom: 'canvas' as Exclude<Field, 'picker'>,
  pinned: null as string | null,
  drawerPinned: false,
  bookletOpen: false,
  paletteOpen: false,
  sheet: null as SheetName | null,
  loaded: false,
  error: '',
  actionError: '',
  focusId: null as string | null
}

export function canvasRun(): Run | undefined {
  return world.runs.find((r) => r.id === world.pinned) ?? world.runs[0]
}

let version = 0
let queued = false
const subs = new Set<() => void>()

export function commit(): void {
  if (queued) return
  queued = true
  requestAnimationFrame(() => {
    queued = false
    version++
    subs.forEach((f) => f())
  })
}

function subscribe(cb: () => void): () => void {
  subs.add(cb)
  return () => void subs.delete(cb)
}

export function useWorld(): typeof world {
  useSyncExternalStore(subscribe, () => version)
  return world
}
