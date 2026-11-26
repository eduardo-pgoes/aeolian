import gsap from 'gsap'
import { DIALS } from '../dials'
import type { Rect } from '../layout'
import { dur, RM } from './motion'
import type { Seen, Str } from './strings'

const NS = 'http://www.w3.org/2000/svg'

export type RuleKind = 'step' | 'dock'

export interface Pt {
  x: number
  y: number
}

export type View = Seen

interface Rule {
  a: string
  b: string
  kind: RuleKind
  draw: number
  solid: number
  // how far a pulse has run, as a fraction; the dotted rule starts there
  open: number
  g: SVGGElement
  base: SVGPolylineElement
  fill: SVGPolylineElement
}

// A handoff: a transverse pulse running down the rule.
interface Pulse {
  rule: Rule
  fwd: boolean
  fills: boolean
  t: number
  line: SVGPolylineElement
}

function svg<K extends keyof SVGElementTagNameMap>(
  tag: K,
  attrs: Record<string, string | number>,
  parent: Element
): SVGElementTagNameMap[K] {
  const e = document.createElementNS(NS, tag)
  for (const k in attrs) e.setAttribute(k, String(attrs[k]))
  parent.append(e)
  return e
}

const lerp = (a: Pt, b: Pt, t: number): Pt => ({ x: a.x + (b.x - a.x) * t, y: a.y + (b.y - a.y) * t })
const dist = (a: Pt, b: Pt): number => Math.hypot(b.x - a.x, b.y - a.y)

function length(pts: Pt[]): number {
  let l = 0
  for (let i = 1; i < pts.length; i++) l += dist(pts[i - 1], pts[i])
  return l
}

function walk(pts: Pt[], t: number): { at: Pt; dir: Pt; head: Pt[] } {
  let left = Math.max(0, Math.min(1, t)) * length(pts)
  const head = [pts[0]]
  for (let i = 1; i < pts.length; i++) {
    const a = pts[i - 1]
    const b = pts[i]
    const d = dist(a, b)
    const dir = d ? { x: (b.x - a.x) / d, y: (b.y - a.y) / d } : { x: 0, y: 0 }
    if (left <= d || i === pts.length - 1) {
      const at = d ? lerp(a, b, Math.min(1, left / d)) : b
      head.push(at)
      return { at, dir, head }
    }
    left -= d
    head.push(b)
  }
  return { at: pts[0], dir: { x: 0, y: 1 }, head }
}
const n = (v: number): string => v.toFixed(1)
const ease3 = (t: number): number => (t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2)

// Where a polyline is at a distance along it, and which way it runs there.
function at(pts: Pt[], s: number): { p: Pt; dir: Pt } {
  for (let i = 1; i < pts.length; i++) {
    const a = pts[i - 1]
    const b = pts[i]
    const d = dist(a, b)
    if (s <= d || i === pts.length - 1) {
      const dir = d ? { x: (b.x - a.x) / d, y: (b.y - a.y) / d } : { x: 0, y: 1 }
      return { p: d ? lerp(a, b, Math.max(0, Math.min(1, s / d))) : b, dir }
    }
    s -= d
  }
  return { p: pts[0], dir: { x: 0, y: 1 } }
}

// The pulse, in mockup px at zoom 1: amplitude, width and lead of the bump.
const PULSE_A = 6
const PULSE_W = 7
const PULSE_LEAD = 18
const PULSE_DS = 1.5
// Strings tick at ~30fps inside the 60fps frame.
const STRING_MS = 1000 / 30 - 2

export const ruleKey = (a: string, b: string): string => `${a}>${b}`

const written = new WeakMap<Element, string>()

function changed(el: Element, value: string): boolean {
  if (written.get(el) === value) return false
  written.set(el, value)
  return true
}

function setPart(l: SVGPolylineElement, pts: Pt[], t: number): void {
  const points = t > 0 ? walk(pts, t).head.map((p) => `${n(p.x)},${n(p.y)}`).join(' ') : ''
  if (changed(l, points)) l.setAttribute('points', points)
}

export function route(kind: RuleKind, A: Rect, B: Rect): Pt[] {
  if (kind === 'dock') {
    const half = DIALS.headPx / 2
    const ax = A.x + A.w
    const bx = Math.max(ax, B.x)
    const by = B.y + half
    if (by >= A.y + half && by <= A.y + A.h - half) return [{ x: ax, y: by }, { x: bx, y: by }]
    const y0 = Math.max(A.y + half, Math.min(A.y + DIALS.dockY + half, A.y + A.h - half))
    const mx = (ax + bx) / 2
    return [{ x: ax, y: y0 }, { x: mx, y: y0 }, { x: mx, y: by }, { x: bx, y: by }]
  }
  const l = Math.max(A.x, B.x)
  const r = Math.min(A.x + A.w, B.x + B.w)
  const [ax, bx] = l < r ? [(l + r) / 2, (l + r) / 2] : [A.x + A.w / 2, B.x + B.w / 2]
  return [
    { x: ax, y: A.y + A.h },
    { x: bx, y: B.y }
  ]
}

export class Overlay {
  readonly el: SVGSVGElement
  // every window's string; the windows own the elements, this ticks them
  readonly strings = new Set<Str>()
  private gRules: SVGGElement
  private gDocks: SVGGElement
  private gPulses: SVGGElement
  private rules = new Map<string, Rule>()
  private pulses = new Set<Pulse>()
  private strummed = 0

  constructor(
    host: HTMLElement,
    private rectOf: (id: string) => Rect | undefined
  ) {
    this.el = document.createElementNS(NS, 'svg')
    this.el.classList.add('overlay')
    this.gRules = svg('g', {}, this.el)
    this.gDocks = svg('g', { class: 'docks' }, this.el)
    this.gPulses = svg('g', {}, this.el)
    host.append(this.el)
  }

  rule(a: string, b: string, kind: RuleKind, color: string, o: { solid: boolean; animate: boolean; delay?: number }): void {
    const key = ruleKey(a, b)
    const had = this.rules.get(key)
    if (had) {
      had.fill.style.stroke = color
      return
    }
    const g = svg('g', { class: `rule ${kind}` }, kind === 'dock' ? this.gDocks : this.gRules)
    const base = svg('polyline', { class: 'r-base' }, g)
    const fill = svg('polyline', { class: 'r-fill' }, g)
    fill.style.stroke = color
    const r: Rule = { a, b, kind, draw: o.animate ? 0 : 1, solid: kind === 'dock' || o.solid ? 1 : 0, open: 0, g, base, fill }
    this.rules.set(key, r)
    if (o.animate) {
      gsap.to(r, { draw: 1, duration: dur(DIALS.ruleDraw), delay: dur(o.delay ?? 0), ease: 'power2.out' })
    }
  }

  keep(keys: Set<string>): void {
    for (const [key, r] of this.rules) {
      if (keys.has(key)) continue
      this.rules.delete(key)
      gsap.killTweensOf(r)
      for (const pl of this.pulses) if (pl.rule === r) this.dropPulse(pl)
      r.g.remove()
    }
  }

  // A pluck travels down the rule: on a step rule it leaves the rule solid
  // in the sender's ink; on the way back to a parent it only passes through.
  travel(from: string, to: string, color: string, onArrive?: () => void): void {
    const fwd = this.rules.get(ruleKey(from, to))
    const r = fwd ?? this.rules.get(ruleKey(to, from))
    if (!r || !this.rectOf(r.a) || !this.rectOf(r.b)) return
    const fills = !!fwd && r.kind === 'step'
    if (fills) r.fill.style.stroke = color
    if (RM) {
      if (fills) r.solid = 1
      onArrive?.()
      return
    }
    const line = svg('polyline', { class: 'pulse' }, this.gPulses)
    line.style.stroke = color
    const pl: Pulse = { rule: r, fwd: !!fwd, fills, t: 0, line }
    this.pulses.add(pl)
    gsap.to(pl, {
      t: 1,
      duration: DIALS.pulseMs / 1000,
      ease: 'none',
      onComplete: () => {
        if (fills) r.solid = 1
        this.dropPulse(pl)
        onArrive?.()
      }
    })
  }

  private dropPulse(pl: Pulse): void {
    gsap.killTweensOf(pl)
    this.pulses.delete(pl)
    pl.rule.open = 0
    pl.line.remove()
  }

  render(v: View): void {
    for (const r of this.rules.values()) {
      const A = this.rectOf(r.a)
      const B = this.rectOf(r.b)
      const display = A && B ? '' : 'none'
      if (changed(r.g, display)) r.g.style.display = display
      if (!A || !B) continue
      const pts = route(r.kind, A, B).map((pt) => ({ x: v.sx(pt.x), y: v.sy(pt.y) }))
      // The dotted rule ahead of a pulse; behind it, the pulse is the rule.
      if (r.open > 0) setPart(r.base, pts.slice().reverse(), Math.max(0, 1 - r.open))
      else setPart(r.base, pts, r.draw)
      setPart(r.fill, pts, Math.min(r.solid, r.draw))
    }
    for (const pl of this.pulses) this.pulse(pl, v)
    this.strum(v)
  }

  private pulse(pl: Pulse, v: View): void {
    const A = this.rectOf(pl.rule.a)
    const B = this.rectOf(pl.rule.b)
    if (!A || !B) return
    const pts = route(pl.rule.kind, A, B).map((pt) => ({ x: v.sx(pt.x), y: v.sy(pt.y) }))
    if (!pl.fwd) pts.reverse()
    const L = length(pts)
    if (!L) return
    const t = pl.t
    const pp = L * ease3(t)
    const amp = PULSE_A * v.k * (1 - 0.5 * t)
    const sig = Math.max(2, PULSE_W * v.k)
    const lead = Math.max(6, PULSE_LEAD * v.k)
    const to = Math.min(L, pp + lead)
    if (pl.fills) pl.rule.open = to / L
    const out: string[] = []
    for (let s = pl.fills ? 0 : Math.max(0, pp - lead); ; s += PULSE_DS) {
      const d = Math.min(s, to)
      const { p, dir } = at(pts, d)
      const off = amp * Math.exp(-(((d - pp) / sig) ** 2))
      out.push(`${n(p.x + dir.y * off)},${n(p.y - dir.x * off)}`)
      if (d >= to) break
    }
    const points = out.join(' ')
    if (changed(pl.line, points)) pl.line.setAttribute('points', points)
  }

  // Live strings move at ~30fps; static ones redraw only when they change.
  private strum(v: View): void {
    const now = performance.now()
    if (now - this.strummed < STRING_MS) return
    const dt = Math.min(0.05, (now - this.strummed) / 1000)
    this.strummed = now
    for (const s of this.strings) {
      s.fit()
      if (s.active(v)) s.step(dt, now, v.k)
    }
  }

  clear(): void {
    for (const r of this.rules.values()) gsap.killTweensOf(r)
    for (const pl of this.pulses) gsap.killTweensOf(pl)
    this.rules.clear()
    this.pulses.clear()
    this.gRules.replaceChildren()
    this.gDocks.replaceChildren()
    this.gPulses.replaceChildren()
  }
}
