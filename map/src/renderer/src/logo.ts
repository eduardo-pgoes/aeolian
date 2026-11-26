// ōgi, the fan: two guards and four strings on one pivot, in --text only.
// Its draw-in ends in a damped left-to-right strum.
//
// This module has no imports and touches the DOM only inside playOgi(), so
// scripts/icons.mjs imports it straight from node to render the app icons.

type Pt = [number, number]

interface Part {
  d?: string // a plain stroke
  deg?: number // or a string (a rib) at this angle off vertical, which can ring
  xs?: boolean // dropped at XS and below
  dl: number // draw-in delay and duration, as fractions of --d3
  du: number
  at?: number // when the string is plucked, as a fraction of --d3
}

/** At this size and below the outer rib pair drops; four strings clot into a smudge. */
export const XS = 24
/** --d3, the timing the draw-in was cut against. playOgi() reads the live token. */
const D3 = 640
const PIVOT = 6.5
const R1 = 13
// Strings start this far from the pivot so they never clot there; below 64px
// the strokes are wider than the gaps near the pivot, so they start further out.
const R0 = 6
const R0_SMALL = 7.5
// At XS the remaining pair spreads to even thirds between the guards (±50°);
// at ±13.3° the two blur into one gray bar at 16px.
const XS_DEG = 16.7
const XS_R0 = 7
// Between XS and 64px the four strings are drawn at this weight in px, lighter than the guards.
const MID_RIB = 1.1
// Instrument Sans 600 stem, as a fraction of the font size.
const STEM = 0.09
/** Lockup: the mark is set at 1.2em, well above the x-height. */
export const LOCK_EM = 1.2
// Lockup stroke is 70% of the stem, in the 24-unit viewBox.
const LOCK_SW = (STEM * 0.7 * 24) / LOCK_EM
// Above 64px the strings are lighter than the guards, so they read as strings and not more pen.
const THIN = 0.62

const RING = { dur: 1300 / D3, tau: 0.3, amp: 1.15 }

const rad = (d: number): number => (d * Math.PI) / 180
const rib = (deg: number, dl: number, at: number, xs = false): Part => ({ deg, xs, dl, du: 260 / D3, at })
const ends = (deg: number, xs: boolean, small: boolean): [Pt, Pt] => {
  const r0 = xs ? XS_R0 : small ? R0_SMALL : R0
  const d = xs ? Math.sign(deg) * XS_DEG : deg
  const s = Math.sin(rad(d))
  const c = Math.cos(rad(d))
  return [
    [r0 * s, PIVOT - r0 * c],
    [R1 * s, PIVOT - R1 * c]
  ]
}
const f = (ms: number): number => ms / D3

const PARTS: Part[] = [
  { d: 'M0 6.5L-9.96-1.86', dl: 0, du: f(300) },
  rib(-32, f(60), f(960), true),
  rib(-13.3, f(120), f(1040)),
  rib(13.3, f(180), f(1120)),
  rib(32, f(240), f(1200), true),
  { d: 'M0 6.5L9.96-1.86', dl: f(300), du: f(300) },
  { d: 'M-9.96-1.86A13 13 0 0 1 9.96-1.86', dl: f(480), du: f(440) }
]

/** A string's path, displaced by a ring of amplitude A at phase ph (harp's envelope × wave). */
function stringD([[x0, y0], [x1, y1]]: [Pt, Pt], A = 0, ph = 0): string {
  const dx = x1 - x0
  const dy = y1 - y0
  const L = Math.hypot(dx, dy)
  const nx = dy / L
  const ny = -dx / L
  const N = A ? 32 : 1
  let d = ''
  for (let i = 0; i <= N; i++) {
    const u = i / N
    const v = A ? Math.sin(Math.PI * u) * RING.amp * A * Math.sin(2 * Math.PI * (u - ph)) : 0
    d += `${i ? 'L' : 'M'}${(x0 + dx * u + nx * v).toFixed(3)} ${(y0 + dy * u + ny * v).toFixed(3)}`
  }
  return d
}

/** Rendered stroke in px: a 1.5px floor, then about 2.2% of the size. */
const strokePx = (px: number): number => Math.max(1.5, 0.8 + px * 0.022)

export interface OgiPath {
  i: number
  d: string
  w: number // stroke width in viewBox units
  string: boolean
  dl: number
  du: number
}

/** The paths at a pixel size, or in the lockup next to the wordmark when px is omitted. xs overrides the outer-pair drop. */
export function ogiPaths(px?: number, xs = px !== undefined && px <= XS): OgiPath[] {
  const small = px !== undefined && px < 64
  const sw = px === undefined ? LOCK_SW : (strokePx(px) * 24) / px
  const rib = xs ? sw : small ? (MID_RIB * 24) / px : sw * THIN
  const out: OgiPath[] = []
  PARTS.forEach((p, i) => {
    if (xs && p.xs) return
    const string = p.deg !== undefined
    const d = string ? stringD(ends(p.deg!, xs, small)) : p.d!
    out.push({ i, d, w: string ? rib : sw, string, dl: p.dl, du: p.du })
  })
  return out
}

const pathTags = (paths: OgiPath[], ink: string, scale = 1): string =>
  paths
    .map((p) => `<path d="${p.d}" stroke="${ink}" stroke-width="${(p.w * scale).toFixed(3)}"/>`)
    .join('')

/** A plain, static SVG of the mark at px, stroked in currentColor unless ink is given. */
export function ogiSvg(px: number, ink = 'currentColor', xs = px <= XS): string {
  return (
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="-12 -12 24 24" width="${px}" height="${px}" ` +
    `fill="none" stroke-linecap="round" stroke-linejoin="round">${pathTags(ogiPaths(px, xs), ink)}</svg>`
  )
}

/**
 * The dock tile at px: the canvas ground with the dot grid fading out behind
 * the mark. At 512 it is the master from the logo studies. Below 64 the grid
 * and the top highlight are noise, so they drop, and the mark grows to fill
 * the tile and keeps a 1.5px stroke.
 */
export function tileSvg(px: number, dark = true, xs = px <= XS): string {
  const g = dark ? ['#1d1f24', '#0b0c0e'] : ['#fcfbf9', '#e4e3dd']
  const ink = dark ? '#eceae4' : '#141518'
  const dot = dark ? '#363a41' : '#c4c1b8'
  const edge = dark ? 'rgb(255 255 255/.08)' : 'rgb(20 22 28/.10)'
  const hi = dark ? 'rgb(255 255 255/.08)' : 'rgb(255 255 255/.9)'
  const small = px < 64
  // Mark scale in the 512 frame: 11 at full size, up to 17 at 16px.
  const s = small ? 11 + (6 * (64 - Math.max(16, px))) / 48 : 11
  const unit = (px / 512) * s // px per viewBox unit
  const guard = Math.max(1.05, (small ? 1.5 : 1.35) / unit)
  const rib = xs ? guard : small ? MID_RIB / unit : Math.max(1.05 * 0.7, 1.05 / unit)
  const paths = PARTS.filter((p) => !(xs && p.xs))
    .map((p) => {
      const d = p.deg !== undefined ? stringD(ends(p.deg, xs, small)) : p.d
      return `<path d="${d}" stroke-width="${(p.deg !== undefined ? rib : guard).toFixed(3)}"/>`
    })
    .join('')
  const grid = small
    ? ''
    : `<rect width="512" height="512" fill="url(#tp)" mask="url(#tm)" opacity="${dark ? 0.9 : 0.7}"/>` +
      `<path d="M114 1.5H398" stroke="${hi}" stroke-width="3" fill="none"/>`
  return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512" width="${px}" height="${px}">
<defs>
<linearGradient id="tg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${g[0]}"/><stop offset="1" stop-color="${g[1]}"/></linearGradient>
<pattern id="tp" width="32" height="32" patternUnits="userSpaceOnUse"><circle cx="16" cy="16" r="2" fill="${dot}"/></pattern>
<radialGradient id="tr" cx=".5" cy=".5" r=".72"><stop offset=".42" stop-color="#000"/><stop offset="1" stop-color="#fff"/></radialGradient>
<mask id="tm"><rect width="512" height="512" fill="url(#tr)"/></mask>
<clipPath id="tc"><rect width="512" height="512" rx="114"/></clipPath>
</defs>
<g clip-path="url(#tc)"><rect width="512" height="512" fill="url(#tg)"/>${grid}</g>
<rect x=".75" y=".75" width="510.5" height="510.5" rx="113.25" fill="none" stroke="${edge}" stroke-width="1.5"/>
<g transform="translate(256 256) scale(${s.toFixed(3)})" fill="none" stroke="${ink}" stroke-linecap="round" stroke-linejoin="round">${paths}</g>
</svg>`
}

/**
 * Plays the draw-in and the strum on an <svg class="ogi"> rendered from
 * ogiPaths(). Call it once, on load. Under reduced motion the mark is left
 * settled. Returns a cleanup that stops it and leaves the mark at rest.
 */
export function playOgi(svg: SVGSVGElement): () => void {
  if (matchMedia('(prefers-reduced-motion: reduce)').matches) return () => {}
  const d3 = parseFloat(getComputedStyle(svg).getPropertyValue('--d3')) || D3
  const timers: number[] = []
  const ringing = new Map<SVGPathElement, { ends: [Pt, Pt]; t0: number; last: number; ph: number }>()
  let raf = 0

  const tick = (now: number): void => {
    for (const [el, r] of ringing) {
      const t = (now - r.t0) / 1000
      if (t * 1000 >= RING.dur * d3) {
        el.setAttribute('d', stringD(r.ends))
        ringing.delete(el)
        continue
      }
      // ~35ms attack, then the energy decays at τ; phase speed follows amplitude.
      const A = (1 - Math.exp(-t / 0.035)) * Math.exp(-t / RING.tau)
      r.ph += Math.min(0.05, (now - r.last) / 1000) * (0.5 + A * 1.6) * 3.2
      r.last = now
      el.setAttribute('d', stringD(r.ends, A, r.ph))
    }
    raf = ringing.size ? requestAnimationFrame(tick) : 0
  }

  svg.querySelectorAll<SVGPathElement>('path[data-pi]').forEach((el) => {
    const p = PARTS[Number(el.dataset.pi)]
    if (p.at === undefined) return
    // The string's pins, as it was rendered for its size: "Mx0 y0Lx1 y1".
    const [x0, y0, x1, y1] = (el.getAttribute('d')!.match(/-?[\d.]+/g) ?? []).map(Number)
    const ends: [Pt, Pt] = [
      [x0, y0],
      [x1, y1]
    ]
    timers.push(
      window.setTimeout(() => {
        const now = performance.now()
        ringing.set(el, { ends, t0: now, last: now, ph: Math.random() })
        if (!raf) raf = requestAnimationFrame(tick)
      }, p.at * d3)
    )
  })
  svg.classList.add('draw')

  return () => {
    timers.forEach(clearTimeout)
    cancelAnimationFrame(raf)
    for (const [el, r] of ringing) el.setAttribute('d', stringD(r.ends))
    ringing.clear()
    svg.classList.remove('draw')
  }
}
