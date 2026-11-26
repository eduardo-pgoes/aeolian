import { DIALS } from '../dials'

export const RM = matchMedia('(prefers-reduced-motion: reduce)').matches

export const dur = (s: number): number => (RM ? 0 : s)

export interface Spring {
  css: string
  dur: number
  fn: (p: number) => number
}

const STEP = 1 / 120
const MAX_T = 3
const CSS_POINTS = 50

export function spring({ k, c }: { k: number; c: number }): Spring {
  let x = 0
  let v = 0
  let t = 0
  const s = [0]
  while (t < MAX_T) {
    v += (-k * (x - 1) - c * v) * STEP
    x += v * STEP
    t += STEP
    s.push(x)
    if (t > 0.25 && Math.abs(x - 1) < 0.001 && Math.abs(v) < 0.01) break
  }
  s.push(1)
  const step = Math.max(1, Math.floor(s.length / CSS_POINTS))
  const pts: number[] = []
  for (let i = 0; i < s.length - 1; i += step) pts.push(+s[i].toFixed(4))
  pts.push(1)
  const last = s.length - 1
  const fn = (p: number): number => {
    const f = Math.min(1, Math.max(0, p)) * last
    const i = Math.floor(f)
    return i >= last ? 1 : s[i] + (s[i + 1] - s[i]) * (f - i)
  }
  return { css: `linear(${pts.join(',')})`, dur: Math.round(t * 1000), fn }
}

export const SP = {
  snap: spring(DIALS.snap),
  drawer: spring(DIALS.drawer),
  cam: spring(DIALS.cam)
}

const root = document.documentElement.style
root.setProperty('--spring', SP.drawer.css)
root.setProperty('--spring-dur', `${SP.drawer.dur}ms`)
root.setProperty('--snap', SP.snap.css)
root.setProperty('--snap-dur', `${SP.snap.dur}ms`)
