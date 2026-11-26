import gsap from 'gsap'
import type { Agent, Run } from '../../../main/host'
import { DIALS } from '../dials'
import { bind, onKey } from '../keymap'
import { ink } from '../inks'
import { hostSize, isLive, layout, sizeFor, stepAgent, windowSize, type Rect, type Size } from '../layout'
import { commit, world } from '../store'
import { measureCell, type Cell } from '../term'
import { onTheme } from '../theme'
import { dur, SP } from './motion'
import { Overlay, ruleKey } from './overlay'
import { Ghost, Win, type WinActions } from './win'

const OWED_MS = 10_000

const clamp = (v: number, a: number, b: number): number => Math.min(b, Math.max(a, v))

class Stage {
  host!: HTMLDivElement
  layer!: HTMLDivElement
  unrun!: HTMLDivElement
  cam = { x: 0, y: 0, k: 0.5 }
  cw = 1
  ch = 1
  inset = 0
  follow = true
  focusId: string | null = null
  openId: string | null = null
  cell?: Cell
  private polledAt = 0
  wins = new Map<string, Win>()
  ghosts = new Map<string, Ghost>()
  overlay!: Overlay
  private stepSlots = new Map<string, string>()
  private handed = new Map<string, string>()
  // a handoff that arrived at a step before its window existed: pluck on entry
  private owed = new Map<string, number>()
  private hoverId: string | null = null
  private run?: Run
  private runId?: string
  private rects: Rect[] = []
  private lastBox = ''
  private resumed = new Map<string, { tmux: string; until: number }>()
  private lastCam = ''
  private movedAt = 0
  private moving = false
  private spaceHeld = false
  private actions: WinActions = {
    kill: (a) => {
      world.actionError = ''
      window.map.kill(a.id).catch((err) => this.fail(err))
    },
    resume: (a) => {
      const w = this.wins.get(a.id)
      if (w && !w.live) this.resume(w)
    }
  }

  mount(host: HTMLDivElement): void {
    if (this.host === host) return
    this.host = host
    host.replaceChildren()
    host.classList.add('canvas')
    const root = document.documentElement.style
    root.setProperty('--head', `${DIALS.headPx}px`)
    root.setProperty('--band', `${DIALS.bandPx}px`)
    root.setProperty('--pad', `${DIALS.padPx}px`)
    root.setProperty('--gut', `${DIALS.gutPx}px`)
    root.setProperty('--card-h', `${DIALS.cardH}px`)

    this.layer = document.createElement('div')
    this.layer.className = 'world'
    host.append(this.layer)

    this.overlay = new Overlay(host, (id) => this.rectOf(id))

    this.unrun = document.createElement('div')
    this.unrun.className = 'unrun'
    this.unrun.innerHTML = `<p class="i-line">the canvas is empty, for now</p><p class="i-go">start a run with <code>aeolian run</code></p>`
    host.append(this.unrun)

    this.input()
    for (const t of [window, host.parentElement!, host] as (Window | HTMLElement)[]) {
      t.addEventListener('scroll', () => {
        if (t === window) window.scrollTo(0, 0)
        else (t as HTMLElement).scrollTop = (t as HTMLElement).scrollLeft = 0
      })
    }
    new ResizeObserver(() => this.resize()).observe(host)
    this.resize()
    gsap.ticker.add(() => this.frame())
    bind('leave', () => this.leave())
    // Inks are baked into --c and rule strokes; a same-run sync re-applies them.
    onTheme(() => this.sync(this.run))
    measureCell().then((cell) => {
      this.cell = cell
      this.sync(this.run)
    })
  }

  sync(run: Run | undefined): void {
    // a new run object is a fresh read of the host; a re-sync reuses the last
    if (run !== this.run) this.polledAt = Date.now()
    this.run = run
    const cell = this.cell
    if (!cell) return
    this.unrun.hidden = !!run
    const fresh = run?.id !== this.runId
    if (fresh) this.clear()
    this.runId = run?.id
    if (!run) return
    run = { ...run, agents: run.agents.map((a) => this.patch(a)) }
    const slots = layout(run, cell, (a) => this.sizeOf(a))
    const seen = new Set<string>()
    let resized = false
    for (const s of slots) {
      seen.add(s.key)
      if (s.kind === 'agent') {
        let w = this.wins.get(s.key)
        if (w) {
          const wasLive = w.live
          w.update(s.agent)
          w.place(s.rect, true, wasLive !== w.live)
          if (wasLive && !w.live && this.focusId === s.key) this.unfocus()
        } else {
          w = new Win(s.agent, s.rect, this.layer, this.actions)
          this.wins.set(s.key, w)
          this.overlay.strings.add(w.str)
          this.enter(w, fresh)
        }
        if (w.connect(this.sizeOf(s.agent), this.wantsTerminal(s.agent), this.polledAt)) resized = true
        w.oldHost(this.oldHost(s.agent))
      } else {
        const g = this.ghosts.get(s.key)
        if (g) {
          g.update(s.step)
          g.place(s.rect, true)
        } else this.ghosts.set(s.key, new Ghost(s.step, s.rect, this.layer))
      }
    }
    for (const [id, w] of this.wins) if (!seen.has(id)) this.drop(id, w)
    for (const [id, g] of this.ghosts) {
      if (seen.has(id)) continue
      g.vanish()
      this.ghosts.delete(id)
    }
    this.rects = slots.map((s) => s.rect)
    this.drawRules(run, fresh)
    this.refit()
    // A terminal followed the host's size after the layout above; lay out
    // again so its window grows with it instead of cropping it.
    if (resized) this.sync(this.run)
  }

  private drawRules(run: Run, fresh: boolean): void {
    const steps = [...run.steps].sort((a, b) => a.index - b.index)
    this.stepSlots.clear()
    for (const st of steps) {
      const a = stepAgent(run, st)
      this.stepSlots.set(`step:${st.name}`, a && this.wins.has(a.id) ? a.id : `step:${st.name}`)
    }
    const keys = new Set<string>()
    steps.slice(1).forEach((st, i) => {
      const up = steps[i]
      const a = `step:${up.name}`
      const b = `step:${st.name}`
      keys.add(ruleKey(a, b))
      this.overlay.rule(a, b, 'step', ink(up.ink), { solid: !!stepAgent(run, up)?.handoff, animate: !fresh })
    })
    for (const a of run.agents) {
      if (!a.parent_id || !this.wins.has(a.parent_id) || !this.wins.has(a.id)) continue
      keys.add(ruleKey(a.parent_id, a.id))
      this.overlay.rule(a.parent_id, a.id, 'dock', ink(a.ink), {
        solid: true,
        animate: !fresh,
        delay: DIALS.settle * 0.6
      })
    }
    this.overlay.keep(keys)

    const handed = new Map<string, string>()
    for (const a of run.agents) {
      const sig = a.handoff ? (a.done_at ?? 'handed') : ''
      handed.set(a.id, sig)
      if (fresh || !sig || this.handed.get(a.id) === sig) continue
      this.handoff(run, steps, a)
    }
    this.handed = handed
  }

  private handoff(run: Run, steps: Run['steps'], a: Agent): void {
    if (a.parent_id) return this.pass(a.id, a.parent_id, ink(a.ink))
    const i = steps.findIndex((st) => st.name === a.step)
    const next = steps[i + 1]
    if (i < 0 || !next || stepAgent(run, steps[i])?.id !== a.id) return
    this.pass(`step:${steps[i].name}`, `step:${next.name}`, ink(a.ink))
  }

  private pass(from: string, to: string, color: string): void {
    this.overlay.travel(from, to, color, () => {
      const w = this.wins.get(this.stepSlots.get(to) ?? to)
      if (w) w.pluck()
      else this.owed.set(to, performance.now())
    })
  }

  private rectOf(id: string): Rect | undefined {
    const key = this.stepSlots.get(id) ?? id
    return this.wins.get(key)?.rect ?? this.ghosts.get(key)?.rect
  }

  private enter(w: Win, fresh: boolean): void {
    const a = w.agent
    const parent = a.parent_id ? this.wins.get(a.parent_id) : undefined
    const ghost = a.step ? this.ghosts.get(`step:${a.step}`) : undefined
    if (fresh) w.fade()
    else if (parent) w.grow()
    else if (ghost) w.growFrom(ghost.rect)
    else w.fade()
    const key = !a.parent_id && a.step ? `step:${a.step}` : ''
    const owed = this.owed.get(key)
    if (owed === undefined) return
    this.owed.delete(key)
    if (performance.now() - owed < OWED_MS) gsap.delayedCall(dur(DIALS.ruleIn + 0.32), () => w.pluck())
  }

  private sizeOf(a: Agent): Size {
    return this.wins.get(a.id)?.size ?? hostSize(a)
  }

  private wantsTerminal(a: Agent): boolean {
    return isLive(a) && !!a.tmux_session && (a.cols !== undefined || this.resumed.has(a.id))
  }

  private oldHost(a: Agent): boolean {
    return (
      isLive(a) &&
      !!a.tmux_session &&
      a.cols === undefined &&
      a.rows === undefined &&
      a.ink === undefined &&
      !this.resumed.has(a.id)
    )
  }

  private patch(a: Agent): Agent {
    const r = this.resumed.get(a.id)
    if (!r) return a
    if ((isLive(a) && a.tmux_session === r.tmux) || Date.now() > r.until) {
      this.resumed.delete(a.id)
      return a
    }
    return { ...a, state: 'idle', tmux_session: r.tmux }
  }

  private drop(id: string, w: Win): void {
    if (this.focusId === id) this.unfocus()
    if (this.openId === id) this.openId = null
    if (this.hoverId === id) this.hoverId = null
    this.overlay.strings.delete(w.str)
    w.dispose()
    this.wins.delete(id)
  }

  private click(id: string): void {
    const w = this.wins.get(id)
    if (!w) return
    if (w.live) this.focus(w)
    else this.openCard(w)
  }

  private openCard(w: Win): void {
    if (this.openId === w.agent.id || !this.cell) return
    this.close()
    this.unfocus()
    const box = windowSize(hostSize(w.agent), this.cell)
    this.openId = w.agent.id
    w.open(box)
    this.follow = false
    this.fly(...this.framing({ ...w.target, ...box }))
  }

  private close(): void {
    if (!this.openId) return
    this.wins.get(this.openId)?.open(null)
    this.openId = null
  }

  highlight(id: string, on: boolean): void {
    this.wins.get(id)?.el.classList.toggle('hl', on)
  }

  show(id: string): void {
    const w = this.wins.get(id)
    if (!w) return
    if (w.live) return this.focus(w)
    this.close()
    this.unfocus()
    this.follow = false
    this.fly(...this.framing(w.target))
  }

  private framing(r: Rect): [number, number, number] {
    const f = DIALS.focusFill
    const k = clamp(Math.min(((this.cw - this.inset) * f) / r.w, (this.ch * f) / r.h), DIALS.minZoom, DIALS.focusMax)
    return [r.x + r.w / 2 - this.inset / 2 / k, r.y + r.h / 2, k]
  }

  private resume(w: Win): void {
    if (w.el.classList.contains('resuming')) return
    w.el.classList.add('resuming')
    world.actionError = ''
    const id = w.agent.id
    window.map
      .resume(id)
      .then((tmux) => {
        this.resumed.set(id, { tmux, until: Date.now() + 2 * DIALS.settleMs })
        if (this.openId === id) this.close()
        this.sync(this.run)
        const now = this.wins.get(id)
        if (now) this.focus(now)
      })
      .catch((err) => this.fail(err))
      .finally(() => w.el.classList.remove('resuming'))
  }

  focus(w: Win): void {
    this.close()
    if (this.focusId && this.focusId !== w.agent.id) this.wins.get(this.focusId)?.blur()
    this.focusId = w.agent.id
    world.focusId = this.focusId
    commit()
    this.follow = false
    this.fly(...this.framing(w.target))
    w.focus()
    this.still()
  }

  private unfocus(): void {
    if (!this.focusId) return
    this.wins.get(this.focusId)?.blur()
    this.focusId = null
    world.focusId = null
    commit()
    this.still()
  }

  // Nothing moves in the focused window or under the pointer.
  private still(): void {
    for (const [id, w] of this.wins) w.str.still = id === this.focusId || id === this.hoverId
  }

  private hover(id: string | null): void {
    if (id === this.hoverId) return
    this.hoverId = id
    this.still()
  }

  leave(): void {
    this.close()
    this.unfocus()
    const active = document.activeElement as HTMLElement | null
    active?.blur?.()
    this.fit()
  }

  private clear(): void {
    for (const [id, w] of this.wins) this.drop(id, w)
    for (const g of this.ghosts.values()) g.remove()
    this.ghosts.clear()
    this.overlay.clear()
    this.stepSlots.clear()
    this.handed.clear()
    this.owed.clear()
    this.rects = []
    this.lastBox = ''
    this.follow = true
  }

  private refit(): void {
    const box = JSON.stringify(this.rects)
    if (box === this.lastBox) return
    this.lastBox = box
    if (this.follow && !this.focusId) this.fit()
  }

  setInset(px: number): void {
    if (px === this.inset) return
    this.inset = px
    if (this.follow && !this.focusId) this.fit()
  }

  private fail(err: unknown): void {
    world.actionError = String(err)
    commit()
  }

  private resize(): void {
    const r = this.host.getBoundingClientRect()
    this.cw = Math.max(1, r.width)
    this.ch = Math.max(1, r.height)
    if (this.follow && !this.focusId) this.fit(0)
  }

  private frame(): void {
    const { x, y, k } = this.cam
    const tx = this.cw / 2 - x * k
    const ty = this.ch / 2 - y * k
    const key = `${tx.toFixed(2)},${ty.toFixed(2)},${k.toFixed(4)}`
    const now = performance.now()
    if (key !== this.lastCam) {
      this.lastCam = key
      this.movedAt = now
      if (!this.moving) this.motion(true)
      this.layer.style.transform = `translate(${tx}px, ${ty}px) scale(${k})`
      this.layer.style.setProperty('--ik', (1 / k).toFixed(4))
      let s = 28 * k
      while (s < 14) s *= 2
      this.host.style.backgroundSize = `${s}px ${s}px`
      this.host.style.backgroundPosition = `${tx % s}px ${ty % s}px`
    } else if (this.moving && now - this.movedAt > DIALS.camRestMs) this.motion(false)
    for (const w of this.wins.values()) w.paint(k, this.moving)
    for (const g of this.ghosts.values()) g.paint(k)
    this.overlay.render({
      k,
      moving: this.moving,
      w: this.cw,
      h: this.ch,
      sx: (wx) => tx + wx * k,
      sy: (wy) => ty + wy * k
    })
  }

  private motion(on: boolean): void {
    this.moving = on
    this.layer.classList.toggle('moving', on)
  }

  fly(x: number, y: number, k: number, d = SP.cam.dur / 1000): void {
    gsap.to(this.cam, { x, y, k, duration: dur(d), ease: SP.cam.fn, overwrite: true })
  }

  fit(d = SP.cam.dur / 1000): void {
    this.follow = true
    const rs = this.rects
    if (!rs.length) return
    const x0 = Math.min(...rs.map((r) => r.x))
    const y0 = Math.min(...rs.map((r) => r.y))
    const x1 = Math.max(...rs.map((r) => r.x + r.w))
    const y1 = Math.max(...rs.map((r) => r.y + r.h))
    const p = DIALS.fitPad
    const w = this.cw - this.inset - 2 * p
    const k = clamp(Math.min(w / (x1 - x0), (this.ch - 2 * p) / (y1 - y0)), DIALS.minZoom, 1)
    this.fly((x0 + x1) / 2 - this.inset / 2 / k, (y0 + y1) / 2, k, d)
  }

  private zoomAt(sx: number, sy: number, f: number): void {
    const { x, y, k } = this.cam
    const wx = (sx - this.cw / 2) / k + x
    const wy = (sy - this.ch / 2) / k + y
    const k2 = clamp(k * f, DIALS.minZoom, DIALS.maxZoom)
    gsap.killTweensOf(this.cam)
    this.follow = false
    this.cam.k = k2
    this.cam.x = wx - (sx - this.cw / 2) / k2
    this.cam.y = wy - (sy - this.ch / 2) / k2
  }

  private input(): void {
    const h = this.host
    h.addEventListener('pointerover', (e) => {
      this.hover((e.target as Element).closest<HTMLElement>('.win')?.dataset.id ?? null)
    })
    h.addEventListener('pointerleave', () => this.hover(null))
    h.addEventListener(
      'wheel',
      (e) => {
        if ((e.target as HTMLElement).closest('.win.focused .w-body, .win.open .w-hand')) return
        e.preventDefault()
        const r = h.getBoundingClientRect()
        const unit = e.deltaMode === 1 ? 16 : 1
        this.zoomAt(e.clientX - r.left, e.clientY - r.top, Math.exp(-e.deltaY * unit * (e.ctrlKey ? 0.01 : 0.0016)))
      },
      { passive: false }
    )

    let press: { x: number; y: number; id: number; win: string | null; pan: boolean; moved: boolean } | null = null
    let grip: { w: Win; x: number; y: number; rw: number; rh: number; id: number } | null = null
    h.addEventListener('pointerdown', (e) => {
      if (e.button !== 0 && e.button !== 1) return
      const t = e.target as HTMLElement
      const gripped = e.button === 0 && t.closest<HTMLElement>('.w-grip')?.closest<HTMLElement>('.win')?.dataset.id
      const gw = gripped ? this.wins.get(gripped) : undefined
      if (gw?.resizable) {
        e.preventDefault()
        e.stopPropagation()
        h.setPointerCapture(e.pointerId)
        grip = { w: gw, x: e.clientX, y: e.clientY, rw: gw.rect.w, rh: gw.rect.h, id: e.pointerId }
        gw.dragging = true
        this.follow = false
        h.classList.add('resizing')
        return
      }
      const winEl = t.closest<HTMLElement>('.win')
      const win = winEl?.dataset.id ?? null
      const pan = this.spaceHeld || e.button === 1 || !winEl
      if (!pan && win && win === this.focusId && this.wins.get(win)?.live) return
      if (!pan && win && win === this.openId) return
      press = { x: e.clientX, y: e.clientY, id: e.pointerId, win, pan, moved: false }
      e.preventDefault()
      if (pan) h.setPointerCapture(e.pointerId)
    })
    h.addEventListener('pointermove', (e) => {
      if (grip && grip.id === e.pointerId && this.cell) {
        const k = this.cam.k
        const size = sizeFor(grip.rw + (e.clientX - grip.x) / k, grip.rh + (e.clientY - grip.y) / k, this.cell)
        if (grip.w.resize(size)) this.sync(this.run)
        return
      }
      if (!press || press.id !== e.pointerId) return
      const dx = e.clientX - press.x
      const dy = e.clientY - press.y
      if (!press.moved && Math.hypot(dx, dy) < 4) return
      if (!press.moved) {
        press.moved = true
        if (!press.pan) return
        gsap.killTweensOf(this.cam)
        this.follow = false
        h.classList.add('panning')
      }
      if (!press.pan) return
      this.cam.x -= dx / this.cam.k
      this.cam.y -= dy / this.cam.k
      press.x = e.clientX
      press.y = e.clientY
    })
    const release = (e: PointerEvent): boolean => {
      if (!grip || grip.id !== e.pointerId) return false
      grip.w.dragging = false
      grip = null
      h.classList.remove('resizing')
      return true
    }
    h.addEventListener('pointerup', (e) => {
      if (release(e)) return
      if (!press || press.id !== e.pointerId) return
      const p = press
      press = null
      h.classList.remove('panning')
      if (p.moved || this.spaceHeld) return
      if (p.win) this.click(p.win)
      else this.leave()
    })
    h.addEventListener('pointercancel', (e) => {
      release(e)
      press = null
      h.classList.remove('panning')
    })
    addEventListener('keydown', (e) => onKey(e))
    addEventListener('keydown', (e) => {
      if (e.key === ' ' && e.target === document.body) {
        e.preventDefault()
        this.space(true)
      }
    })
    addEventListener('keyup', (e) => e.key === ' ' && this.space(false))
    addEventListener('blur', () => this.space(false))
  }

  private space(on: boolean): void {
    this.spaceHeld = on
    this.host.classList.toggle('space', on)
  }
}

export const stage = new Stage()
