import { stage } from './canvas/stage'
import { closeSheet } from './nav'
import { commit, world } from './store'

export type Field = 'canvas' | 'picker' | 'arena'

export function openPicker(): void {
  closeSheet(false)
  if (world.field === 'picker') return
  if (world.field === 'canvas') stage.leave()
  world.pickerFrom = world.field
  world.field = 'picker'
  commit()
}

export function closePicker(): void {
  if (world.field !== 'picker') return
  world.field = world.pickerFrom
  commit()
}
