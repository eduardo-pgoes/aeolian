// The string: each window's header seam is a string pinned at both ends.
// Output adds energy, a question holds it straight, done is flat, pending
// is dotted, killed or failed is snapped. Overlay ticks the live ones at
// ~30fps; everything else is drawn once when its mode or width changes.

import type { Agent } from '../../../main/host'
import { DIALS } from '../dials'
import type { Rect } from '../layout'
import { WAVES, type Tuning, type Wave } from '../marks'
import { RM } from './motion'

export type Mode = 'live' | 'held' | 'flat' | 'open' | 'snap'

// Energy decays at τ; amplitude follows energy through a critically damped
// spring (ζ = 1), so nothing overshoots.
const TAU = 0.9
const K = 60
const C = 2 * Math.sqrt(K)
// Output rate to energy, on a log scale: a spinner redraw (~300 B/s) is a
// shimmer, streamed text (~3 KB/s) a clear vibration, a dump saturates.
const GAIN = 0.38
const KNEE = 60
const MAX_E = 1.1
const PLUCK_E = 1.15
const RM_A = 0.5
// a pluck that reaches a string before it goes live waits this long for it
const OWED_MS = 10_000

export function modeOf(a: Agent): Mode {
  switch (a.state) {
    case 'done':
      return 'flat'
    case 'killed':
    case 'failed':
      return 'snap'
    case 'pending':
      return 'open'
  }
  return a.blocked ? 'held' : 'live'
}

export interface Seen {
  k: number
  moving: boolean
  // world to screen
  sx(x: number): number
  sy(y: number): number
  w: number
  h: number
}

export class Str {
  mode: Mode | '' = ''
  // focused or under the pointer: hold the last shape
  still = false
  // zoomed out to a card: the band isn't showing
  far = false
  private p: SVGPathElement
  private wave: Wave = WAVES.open
  private E = 0
  private A = 0
  private V = 0
  private ph = Math.random() * 3
  private r = 0
  private rAt = 0
  private w = 0
  private lam = 12
  private stale = true
  private drawn = ''
  private owed = 0
  private owedAt = 0

  constructor(
    readonly svg: SVGSVGElement,
    private rect: () => Rect,
    private width: () => number
  ) {
    this.p = svg.querySelector('path')!
  }

  set(mode: Mode, tune: Tuning | 'open'): void {
    const wave = WAVES[tune]
    if (mode === this.mode && wave === this.wave) return
    if (mode !== this.mode) {
      if (this.mode) this.svg.classList.remove(this.mode)
      this.svg.classList.add(mode)
      this.mode = mode
      this.E = this.A = this.V = 0
      if (mode === 'live' && this.owed && performance.now() - this.owedAt < OWED_MS) this.pluck(this.owed)
      this.owed = 0
    }
    this.wave = wave
    this.stale = true
  }

  // Bytes of output. The decayed count is the rate the string reads.
  feed(n: number): void {
    const now = performance.now()
    this.r = this.r * Math.exp(-(now - this.rAt) / 1000 / TAU) + n
    this.rAt = now
  }

  pluck(amt: number): void {
    if (RM || this.still) return
    if (this.mode === 'open') {
      this.owed = amt
      this.owedAt = performance.now()
      return
    }
    if (this.mode !== 'live') return
    this.E = Math.min(PLUCK_E, this.E + amt)
  }

  // Keeps the viewBox on the band's width and redraws a shape that went stale.
  fit(): void {
    const w = Math.max(8, Math.round(this.width()))
    if (w !== this.w) {
      this.w = w
      this.lam = Math.max(12, Math.min(26, w / 16))
      this.svg.setAttribute('viewBox', `0 0 ${w} ${DIALS.bandPx}`)
      this.stale = true
    }
    if (!this.stale) return
    this.stale = false
    if (this.mode === 'live') this.draw(RM ? RM_A : this.A, 1)
    else this.shape()
  }

  // Whether this string is worth moving this frame.
  active(v: Seen): boolean {
    if (this.mode !== 'live' || this.still || this.far || v.moving || RM) return false
    const r = this.rect()
    const top = r.y + DIALS.headPx
    return v.sx(r.x + this.w) > 0 && v.sx(r.x) < v.w && v.sy(top + DIALS.bandPx) > 0 && v.sy(top) < v.h
  }

  step(dt: number, now: number, k: number): void {
    const rate = this.r * Math.exp(-(now - this.rAt) / 1000 / TAU)
    const want = Math.min(MAX_E, GAIN * Math.log10(1 + rate / KNEE))
    this.E = Math.max(this.E * Math.exp(-dt / TAU), want)
    this.V += (K * (this.E - this.A) - C * this.V) * dt
    this.A += this.V * dt
    this.ph += dt * (0.5 + this.A * 1.6)
    this.draw(this.A, k)
  }

  private draw(A: number, k: number): void {
    const { w, lam } = this
    const mid = DIALS.bandPx / 2
    const amp = (mid - 1.25) * Math.max(-1.1, Math.min(1.1, A))
    if (Math.abs(amp) < 0.05) return this.path(`M1 ${mid}H${w - 1}`)
    // ~2 screen px per sample, but never coarser than a quarter wavelength.
    const dx = Math.max(2, Math.min(lam / 4, 1.5 / k))
    let d = `M1 ${mid}`
    for (let x = 2; x < w - 1; x += dx) {
      const env = Math.sin((Math.PI * x) / w)
      d += `L${x.toFixed(1)} ${(mid - amp * env * this.wave(x / lam - this.ph)).toFixed(2)}`
    }
    this.path(`${d}L${w - 1} ${mid}`)
  }

  private shape(): void {
    const { w } = this
    const mid = DIALS.bandPx / 2
    if (this.mode === 'snap') {
      const a = w * 0.47
      const b = w * 0.53
      return this.path(`M1 ${mid}H${(a - 5).toFixed(1)}q4 0 5 3.5M${b.toFixed(1)} ${mid - 3.5}q1 3.5 5 3.5H${w - 1}`)
    }
    this.path(`M1 ${mid}H${w - 1}`)
  }

  private path(d: string): void {
    if (d === this.drawn) return
    this.drawn = d
    this.p.setAttribute('d', d)
  }
}
