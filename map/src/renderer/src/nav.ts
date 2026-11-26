import type { Agent } from '../../main/host'
import { stage } from './canvas/stage'
import { canvasRun, commit, type SheetName, world } from './store'

let bookletReturn: HTMLElement | null = null
let paletteReturn: HTMLElement | null = null
let sheetReturn: HTMLElement | null = null

function here(): HTMLElement | null {
  const a = document.activeElement
  return a instanceof HTMLElement && a !== document.body ? a : null
}

function back(el: HTMLElement | null): void {
  if (el?.isConnected) el.focus({ preventScroll: true })
}

export function openBooklet(): void {
  if (world.bookletOpen || world.drawerPinned) return
  bookletReturn = here()
  world.bookletOpen = true
  commit()
}

export function closeBooklet(restore = true): void {
  if (!world.bookletOpen) return
  world.bookletOpen = false
  commit()
  const el = bookletReturn
  bookletReturn = null
  if (restore) requestAnimationFrame(() => requestAnimationFrame(() => back(el)))
}

export function toggleBooklet(): void {
  if (world.drawerPinned) return togglePin()
  if (world.bookletOpen) closeBooklet()
  else openBooklet()
}

export function togglePin(): void {
  world.drawerPinned = !world.drawerPinned
  world.bookletOpen = false
  commit()
}

export function openPalette(): void {
  if (world.paletteOpen) return
  paletteReturn = here()
  world.paletteOpen = true
  commit()
}

export function closePalette(restore = true): void {
  if (!world.paletteOpen) return
  world.paletteOpen = false
  commit()
  const el = paletteReturn
  paletteReturn = null
  if (restore) back(el)
}

export function togglePalette(): void {
  if (world.paletteOpen) closePalette()
  else openPalette()
}

export function openSheet(name: SheetName): void {
  if (world.sheet === name) return
  if (!world.sheet) sheetReturn = world.bookletOpen ? bookletReturn : here()
  if (world.bookletOpen) closeBooklet(false)
  world.sheet = name
  commit()
}

export function closeSheet(restore = true): void {
  if (!world.sheet) return
  world.sheet = null
  commit()
  const el = sheetReturn
  sheetReturn = null
  if (restore) requestAnimationFrame(() => requestAnimationFrame(() => back(el)))
}

export function toggleSheet(name: SheetName): void {
  if (world.sheet === name) closeSheet()
  else openSheet(name)
}

export function pin(runId: string): void {
  world.pinned = runId === world.runs[0]?.id ? null : runId
  commit()
  stage.sync(canvasRun())
}

export function showOnCanvas(runId: string): void {
  closeSheet(false)
  pin(runId)
  world.field = 'canvas'
  commit()
}

export function followNewest(): void {
  closeSheet(false)
  world.pinned = null
  world.field = 'canvas'
  commit()
  stage.sync(canvasRun())
}

export function flyTo(a: Agent): void {
  if (world.bookletOpen) closeBooklet(false)
  closeSheet(false)
  const runId = a.run_id ?? world.runs.find((r) => r.agents.some((x) => x.id === a.id))?.id
  const away = world.field !== 'canvas'
  if (runId && runId !== (world.pinned ?? world.runs[0]?.id)) pin(runId)
  if (!away) return stage.show(a.id)
  world.field = 'canvas'
  commit()
  requestAnimationFrame(() => requestAnimationFrame(() => stage.show(a.id)))
}
