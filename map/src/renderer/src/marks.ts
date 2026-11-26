import gsap from 'gsap'
import type { Agent } from '../../main/host'
import { RM } from './canvas/motion'

// A role's identity is its tuning: the waveform its string vibrates in.
export const TUNINGS = ['sine', 'tri', 'saw', 'square', 'pulse', 'arch', 'step', 'grain'] as const
export type Tuning = (typeof TUNINGS)[number]

const fr = (u: number): number => u - Math.floor(u)
const hsh = (n: number): number => {
  const s = Math.sin(n * 127.1 + 311.7) * 43758.5453
  return (s - Math.floor(s)) * 2 - 1
}

export type Wave = (u: number) => number
// The eight shapes; the glyphs here and the strings in canvas/strings.ts both draw them.
export const WAVES: Record<Tuning | 'open', Wave> = {
  sine: (u) => Math.sin(2 * Math.PI * u),
  tri: (u) => (fr(u + 0.25) < 0.5 ? 4 * fr(u + 0.25) - 1 : 3 - 4 * fr(u + 0.25)),
  saw: (u) => 2 * fr(u) - 1,
  square: (u) => (fr(u) < 0.5 ? 1 : -1),
  pulse: (u) => (fr(u) < 0.1 ? -0.6 + 16 * fr(u) : fr(u) < 0.2 ? 1 - 16 * (fr(u) - 0.1) : -0.6),
  arch: (u) => 1 - 2 * Math.abs(Math.sin(Math.PI * u)),
  step: (u) => [-1, -1 / 3, 1 / 3, 1, 1 / 3, -1 / 3][Math.floor(fr(u) * 6)],
  grain: (u) => {
    const i = Math.floor(u * 4)
    const f = u * 4 - i
    return (hsh(i) + (hsh(i + 1) - hsh(i)) * f) * 1.1
  },
  // the open string: no name, nothing tuned in yet
  open: () => 0
}

// Two periods sampled across a 24×14 box, collinear points dropped.
function sample(wave: Wave): string {
  const N = 160
  const pts: [number, number][] = []
  for (let i = 0; i <= N; i++) {
    const t = i / N
    pts.push([2 + 20 * t, 7 - 5 * Math.max(-1, Math.min(1, wave(t * 2)))])
  }
  const out = [pts[0]]
  for (let i = 1; i < N; i++) {
    const a = out[out.length - 1]
    const [b, c] = [pts[i], pts[i + 1]]
    if (Math.abs((b[0] - a[0]) * (c[1] - a[1]) - (b[1] - a[1]) * (c[0] - a[0])) > 0.002) out.push(b)
  }
  out.push(pts[N])
  return 'M' + out.map(([x, y]) => `${x.toFixed(2)} ${y.toFixed(2)}`).join('L')
}

const PATHS = Object.fromEntries(Object.entries(WAVES).map(([k, w]) => [k, sample(w)])) as Record<Tuning | 'open', string>

const isTuning = (s: string): s is Tuning => (TUNINGS as readonly string[]).includes(s)

// FNV-1a, 32-bit: stable across runs and platforms.
function fnv1a(s: string): number {
  let h = 0x811c9dc5
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 0x01000193)
  return h >>> 0
}

// A tuning name is taken as is; any other name is hashed into one of the eight.
// No name at all is the open string.
export function tuning(key: string | undefined): Tuning | 'open' {
  const k = key?.trim().toLowerCase() ?? ''
  if (!k) return 'open'
  return isTuning(k) ? k : TUNINGS[fnv1a(k) % TUNINGS.length]
}

// The identity key an agent is drawn from, the same at every call site.
export const markOf = (a: Agent): string => a.tuning || a.mark || (a.parent_id ? a.role : a.step) || ''

export function mark(key: string | undefined, cls = ''): string {
  const t = tuning(key)
  return `<svg class="glyph ${cls}" data-tuning="${t}" viewBox="0 0 24 14" aria-hidden="true"><path pathLength="1" d="${PATHS[t]}"/></svg>`
}

export function drawIn(root: Element | null | undefined, delay = 0): void {
  if (!root || RM) return
  const parts = root.querySelectorAll('svg.glyph > path')
  if (!parts.length) return
  gsap.fromTo(
    parts,
    { strokeDasharray: 1, strokeDashoffset: 1 },
    {
      strokeDashoffset: 0,
      duration: 0.42,
      ease: 'power2.inOut',
      delay,
      clearProps: 'strokeDasharray,strokeDashoffset'
    }
  )
}
