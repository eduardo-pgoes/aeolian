import gsap from 'gsap'
import { ClipboardAddon } from '@xterm/addon-clipboard'
import { Terminal } from '@xterm/xterm'
import type { Agent, Step } from '../../../main/host'
import { DIALS } from '../dials'
import { ink } from '../inks'
import { onKey } from '../keymap'
import { clampSize, isLive, type Rect, type Size } from '../layout'
import { drawIn, mark, markOf, tuning } from '../marks'
import { termOptions } from '../term'
import { onTheme, theme } from '../theme'
import { track } from '../termTheme'
import { dur, RM } from './motion'
import { modeOf, Str } from './strings'
import '../terminal.css'

const PREFIX = 'aeolian-'
const RM_FADE = 0.15
// the body's fade, and the string tensioning left to right as a window tunes in
const FADE_S = 0.24
const TENSION_S = 0.32
const ESC_CR = '\x1b\r'
const RETRY_MS = 3000
const WORDY = /[\p{L}\p{N}]/u
const SGR_MOUSE = /\x1b\[<(\d+);\d+;\d+[Mm]/g
const OLD_HOST = "this host doesn't report session size; restart it with the current aeolian build"

const hostKey = (a: Agent): string => `${a.cols}x${a.rows}`

const hover = (b: number): boolean => (b & 32) !== 0 && (b & 64) === 0 && (b & 3) === 3
const unhovered = (d: string): string => d.replace(SGR_MOUSE, (seq, b: string) => (hover(Number(b)) ? '' : seq))

const byPty = new Map<number, Win>()
window.map.onData((id, data) => byPty.get(id)?.data(data))
window.map.onExit((id) => byPty.get(id)?.exited(id))
// tmux's copy-mode and message styles bake in the theme's paper and the ink.
onTheme(() => {
  for (const [id, w] of byPty) window.map.restyle(id, ink(w.agent.ink), theme())
})

export interface WinActions {
  kill(a: Agent): void
  resume(a: Agent): void
}

export function stateLabel(a: Agent): string {
  if (isLive(a) && a.blocked) return 'waiting on helpers'
  if (!isLive(a) && a.reason && a.reason !== a.state) return `${a.state} · ${a.reason}`
  switch (a.state) {
    case 'busy':
      return 'working'
    case 'idle':
      return 'idle'
    case 'pending':
      return 'queued'
    default:
      return a.state
  }
}

export function look(a: Agent): string {
  if (!isLive(a)) return a.state === 'done' ? 'done' : 'killed'
  return a.blocked ? 'blocked' : a.state
}

export const agentName = (a: Agent): string => (a.parent_id ? a.role || 'helper' : a.step || a.id)

export const firstLine = (s: string | undefined): string => s?.split('\n').find((l) => l.trim())?.trim() ?? ''

class Placed {
  el: HTMLDivElement
  rect: Rect
  far = false
  protected to: Rect
  private lastTf = ''
  private lastBox = ''

  constructor(rect: Rect, layer: HTMLElement, className: string) {
    this.rect = { ...rect }
    this.to = { ...rect }
    this.el = document.createElement('div')
    this.el.className = className
    this.sizeEl()
    layer.append(this.el)
  }

  get target(): Rect {
    return { ...this.to }
  }

  place(rect: Rect, animate: boolean, morph = false): void {
    const moved = rect.x !== this.to.x || rect.y !== this.to.y
    const sized = rect.w !== this.to.w || rect.h !== this.to.h
    this.to = { ...rect }
    const tweenSize = sized && animate && morph
    if (sized && !tweenSize) {
      gsap.killTweensOf(this.rect, 'w,h')
      this.rect.w = rect.w
      this.rect.h = rect.h
      this.sizeEl()
    }
    if (!moved && !tweenSize) return
    const vars = { x: rect.x, y: rect.y, ...(tweenSize ? { w: rect.w, h: rect.h } : {}) }
    if (animate) {
      gsap.to(this.rect, { ...vars, duration: dur(DIALS.settle), ease: 'expo.out', overwrite: 'auto' })
    } else {
      gsap.killTweensOf(this.rect, 'x,y')
      Object.assign(this.rect, vars)
    }
  }

  paint(k: number): void {
    this.paintPlace(k)
  }

  protected paintPlace(k: number): void {
    const r = this.rect
    const tf = `translate(${r.x}px, ${r.y}px)`
    if (tf !== this.lastTf) {
      this.el.style.transform = tf
      this.lastTf = tf
    }
    this.sizeEl()
    const far = r.w * k < DIALS.semanticPx
    if (far !== this.far) {
      this.far = far
      this.el.classList.toggle('far', far)
    }
  }

  protected box(): { w: number; h: number } {
    return this.rect
  }

  protected sizeEl(): void {
    const { w, h } = this.box()
    const key = `${w},${h}`
    if (key === this.lastBox) return
    this.lastBox = key
    this.el.style.width = `${w}px`
    this.el.style.height = `${h}px`
  }

  fade(): void {
    gsap.fromTo(this.el, { opacity: 0 }, { opacity: '', duration: dur(DIALS.fadeIn), clearProps: 'opacity' })
  }

  vanish(): void {
    gsap.killTweensOf(this.rect)
    gsap.to(this.el, { opacity: 0, duration: dur(DIALS.fadeIn), ease: 'power2.in', onComplete: () => this.remove() })
  }

  remove(): void {
    gsap.killTweensOf([this.el, this.rect])
    this.el.remove()
  }
}

export class Win extends Placed {
  agent: Agent
  protected q: Record<
    | 'band'
    | 'bod'
    | 'skin'
    | 'sheet'
    | 'card'
    | 'sign'
    | 'name'
    | 'state'
    | 'meta'
    | 'kill'
    | 'resume'
    | 'body'
    | 'hand'
    | 'old'
    | 'cSign'
    | 'cName'
    | 'cState'
    | 'cLast',
    HTMLElement
  >
  focused = false
  private openBox: { w: number; h: number } | null = null
  private lastMark = ''
  readonly str: Str
  private term?: Terminal
  private pty?: number
  private session = ''
  private attaching = false
  private retryAt = 0
  private gen = 0
  private dirty = false
  private lastHost = ''
  private settleUntil = 0
  dragging = false

  constructor(agent: Agent, rect: Rect, layer: HTMLElement, protected act: WinActions) {
    super(rect, layer, 'win')
    this.agent = agent
    this.el.dataset.id = agent.id
    this.el.innerHTML = `
      <div class="w-bod">
        <div class="w-skin"></div>
        <div class="w-in">
          <header class="w-head">
            <span class="w-sign"></span>
            <span class="w-name"></span>
            <span class="w-state"></span>
            <span class="w-meta"></span>
            <button class="ib w-act w-kill" title="Stop this agent" aria-label="kill"><svg viewBox="0 0 14 14" aria-hidden="true"><path d="M3.5 3.5l7 7M10.5 3.5l-7 7"/></svg></button>
            <button class="ib w-act w-resume" title="Resume this agent's session" aria-label="resume"><svg viewBox="0 0 14 14" aria-hidden="true"><path d="M4.5 3v8l6.5-4z"/></svg></button>
          </header>
          <div class="w-band"><svg class="str" aria-hidden="true"><path/></svg></div>
          <div class="w-body"><div class="w-xt"></div><div class="w-hand"></div><div class="w-old"></div></div>
        </div>
        <div class="w-card">
          <div class="c-head"><span class="c-sign"></span><span class="c-name"></span></div>
          <span class="c-state"></span>
          <span class="c-last"></span>
        </div>
      </div>
      <div class="w-grip" title="Drag to resize the session"></div>`
    const pick = (s: string): HTMLElement => this.el.querySelector<HTMLElement>(s)!
    this.q = {
      band: pick('.w-band'),
      bod: pick('.w-bod'),
      skin: pick('.w-skin'),
      sheet: pick('.w-in'),
      card: pick('.w-card'),
      sign: pick('.w-sign'),
      name: pick('.w-name'),
      state: pick('.w-state'),
      meta: pick('.w-meta'),
      kill: pick('.w-kill'),
      resume: pick('.w-resume'),
      body: pick('.w-xt'),
      hand: pick('.w-hand'),
      old: pick('.w-old'),
      cSign: pick('.c-sign'),
      cName: pick('.c-name'),
      cState: pick('.c-state'),
      cLast: pick('.c-last')
    }
    this.str = new Str(
      this.el.querySelector('svg.str')!,
      () => this.rect,
      () => this.box().w
    )
    this.button(this.q.kill, () => this.act.kill(this.agent))
    this.button(this.q.resume, () => this.act.resume(this.agent))
    this.update(agent)
  }

  private button(b: HTMLElement, press: () => void): void {
    b.addEventListener('pointerdown', (e) => e.stopPropagation())
    b.addEventListener('click', (e) => {
      e.stopPropagation()
      press()
    })
  }

  private fadeBody(duration: number, delay = 0): void {
    gsap.fromTo(this.q.bod, { opacity: 0 }, { opacity: 1, duration, delay, ease: 'power2.out', clearProps: 'opacity' })
  }

  // Tune in: the body fades up and the string tensions left to right.
  grow(): void {
    if (RM) return this.fadeBody(RM_FADE)
    this.fadeBody(FADE_S)
    gsap.fromTo(
      this.q.band,
      { clipPath: 'inset(0% 100% 0% 0%)' },
      { clipPath: 'inset(0% 0% 0% 0%)', duration: TENSION_S, ease: 'power3.out', clearProps: 'clipPath' }
    )
    drawIn(this.el, DIALS.ruleIn)
  }

  growFrom(from: Rect): void {
    Object.assign(this.rect, from)
    this.sizeEl()
    gsap.to(this.rect, { ...this.to, duration: dur(DIALS.settle), ease: 'expo.out', overwrite: 'auto' })
    this.grow()
  }

  pluck(amt = 1): void {
    this.str.pluck(amt)
  }

  get live(): boolean {
    return isLive(this.agent)
  }

  update(agent: Agent): void {
    this.agent = agent
    const a = agent
    this.el.style.setProperty('--c', ink(a.ink))
    this.el.classList.toggle('helper', !!a.parent_id)
    for (const l of ['busy', 'idle', 'pending', 'blocked', 'done', 'killed']) this.el.classList.remove(l)
    this.el.classList.add(look(a))
    this.el.classList.toggle('finished', !this.live)
    this.el.classList.toggle('failed', a.state === 'failed')
    const sign = markOf(a)
    this.str.set(modeOf(a), tuning(sign))
    if (sign !== this.lastMark || !this.q.sign.firstChild) {
      this.lastMark = sign
      this.q.sign.innerHTML = mark(sign)
      this.q.cSign.innerHTML = mark(sign)
      if (!this.focused) drawIn(this.el, 0.08)
    }
    const name = agentName(a)
    this.q.name.textContent = name
    this.q.cName.textContent = name
    this.q.state.textContent = stateLabel(a)
    this.q.cState.textContent = stateLabel(a)
    const session = a.tmux_session.startsWith(PREFIX) ? a.tmux_session.slice(PREFIX.length) : a.tmux_session
    this.q.meta.textContent = this.live ? session : `session ${a.session_id}`
    this.el.title = a.id
    this.q.kill.hidden = !this.live
    this.q.resume.hidden = this.live
    if (this.live) this.open(null)
    this.q.hand.textContent = this.live ? '' : (a.handoff?.text ?? '')
    if (!this.live) this.lastLine(firstLine(a.handoff?.text) || 'killed')
  }

  protected lastLine(s: string): void {
    if (this.q.cLast.textContent !== s) this.q.cLast.textContent = s
  }

  get opened(): boolean {
    return !!this.openBox
  }

  open(box: { w: number; h: number } | null): void {
    this.openBox = box
    this.el.classList.toggle('open', !!box)
    this.sizeEl()
  }

  protected box(): { w: number; h: number } {
    return this.openBox ?? this.rect
  }

  get size(): Size | undefined {
    return this.term && { cols: this.term.cols, rows: this.term.rows }
  }

  get resizable(): boolean {
    return this.pty !== undefined
  }

  // connect returns true when it resized the terminal, so the stage lays the
  // window out again at the new size. polledAt is when the host data in
  // this.agent was read.
  connect(size: Size, want: boolean, polledAt: number): boolean {
    if (!this.live) {
      this.detach()
      return false
    }
    if (!want) return false
    const current = this.term && this.session === this.agent.tmux_session
    if (current && (this.pty !== undefined || this.attaching || Date.now() < this.retryAt)) return this.followHost(polledAt)
    this.lastHost = hostKey(this.agent)
    this.attach(size)
    return false
  }

  oldHost(on: boolean): void {
    const text = on ? OLD_HOST : ''
    if (this.q.old.textContent !== text) this.q.old.textContent = text
  }

  // Every map is its own tmux client, and tmux sizes the window to the latest
  // active one (window-size latest). When the host reports a size this
  // terminal doesn't have, another client won, and this one shows the window
  // cropped on the right until it follows. A report read mid-drag or just
  // after this terminal's own resize may be stale, so it is skipped without
  // being marked seen: the first report read after the settle decides. (It
  // used to be marked seen, and a size another client set during the settle
  // was then never followed.)
  // Following sends tmux exactly the size it reported, so it never changes
  // the window (it used to add a phantom status row, and every follow grew
  // the window by one). The one exception is a host taller than maxRows:
  // the terminal takes maxRows once per report, and the session scrolls.
  private followHost(polledAt: number): boolean {
    const a = this.agent
    const key = hostKey(a)
    if (a.cols === undefined || a.rows === undefined || key === this.lastHost) return false
    if (this.dragging || polledAt < this.settleUntil) return false
    this.lastHost = key
    return this.resize({ cols: a.cols, rows: a.rows })
  }

  resize(want: Size): boolean {
    const t = this.term
    const cur = this.size
    const size = clampSize(want)
    if (!t || !cur || this.pty === undefined || (cur.cols === size.cols && cur.rows === size.rows)) return false
    t.resize(size.cols, size.rows)
    window.map.resize(this.pty, size.cols, size.rows)
    this.settleUntil = Date.now() + DIALS.settleMs
    return true
  }

  private attach(want: Size): void {
    this.detach()
    const gen = ++this.gen
    const size = clampSize(want)
    const term = new Terminal({ ...termOptions(this.agent.ink), cols: size.cols, rows: size.rows })
    this.term = term
    track(term, this.agent.ink)
    this.session = this.agent.tmux_session
    this.attaching = true
    term.loadAddon(new ClipboardAddon())
    term.open(this.q.body)
    term.attachCustomKeyEventHandler((e) => !this.newline(e) && !onKey(e))
    term.onWriteParsed(() => (this.dirty = true))
    term.onData((d) => {
      const out = unhovered(d)
      if (this.pty !== undefined && out) window.map.write(this.pty, out)
    })
    this.el.classList.add('attached')
    if (this.focused) this.focusTerm()
    const scheme = theme()
    window.map.attach(this.session, size.cols, size.rows, ink(this.agent.ink), scheme).then(
      (id) => {
        if (gen !== this.gen) return window.map.close(id)
        this.attaching = false
        this.pty = id
        byPty.set(id, this)
        // The theme flipped while attaching.
        if (theme() !== scheme) window.map.restyle(id, ink(this.agent.ink), theme())
      },
      (err) => {
        if (gen !== this.gen) return
        this.attaching = false
        this.retryAt = Date.now() + RETRY_MS
        term.write(`\r\n${err}\r\n`)
      }
    )
  }

  private newline(e: KeyboardEvent): boolean {
    if (e.key !== 'Enter' || !e.shiftKey || e.ctrlKey || e.altKey || e.metaKey) return false
    if (e.type === 'keydown' && this.pty !== undefined) window.map.write(this.pty, ESC_CR)
    return true
  }

  data(d: string): void {
    this.term?.write(d)
    this.str.feed(d.length)
  }

  exited(id: number): void {
    byPty.delete(id)
    if (this.pty !== id) return
    this.pty = undefined
    this.retryAt = Date.now() + RETRY_MS
  }

  private detach(): void {
    this.gen++
    if (this.pty !== undefined) {
      byPty.delete(this.pty)
      window.map.close(this.pty)
    }
    this.pty = undefined
    this.attaching = false
    this.term?.dispose()
    this.term = undefined
    this.el.classList.remove('attached')
  }

  focus(): void {
    this.focused = true
    this.el.classList.add('focused')
    this.focusTerm()
  }

  private focusTerm(): void {
    if (!this.term) return
    this.term.options.cursorBlink = true
    this.term.options.cursorInactiveStyle = 'outline'
    this.term.focus()
  }

  blur(): void {
    this.focused = false
    this.el.classList.remove('focused')
    if (!this.term) return
    this.term.options.cursorBlink = false
    this.term.options.cursorInactiveStyle = 'none'
    this.term.blur()
  }

  paint(k: number, moving = false): void {
    this.paintPlace(k)
    this.str.far = this.far
    if (!(this.far || moving) || !this.dirty || !this.term) return
    this.dirty = false
    this.lastLine(this.lastTermLine(this.term))
  }

  private lastTermLine(t: Terminal): string {
    const b = t.buffer.active
    const bottom = b.baseY + t.rows - 1
    for (let i = bottom; i >= Math.max(0, bottom - 200); i--) {
      const line = b.getLine(i)?.translateToString(true).trim()
      if (line && WORDY.test(line)) return line
    }
    return ''
  }

  remove(): void {
    gsap.killTweensOf([this.q.band, this.q.bod])
    super.remove()
  }

  dispose(): void {
    this.detach()
    this.remove()
  }
}

export class Ghost extends Placed {
  private lastMark: string | undefined

  constructor(step: Step, rect: Rect, layer: HTMLElement) {
    super(rect, layer, 'ghost')
    this.el.innerHTML = `<span class="g-sign"></span><span class="g-label"></span><span class="g-band"></span><span class="g-sub">pending</span>`
    this.update(step)
    this.fade()
  }

  update(step: Step): void {
    this.el.style.setProperty('--c', ink(step.ink))
    const sign = step.tuning || step.mark || step.name
    if (sign !== this.lastMark) {
      this.lastMark = sign
      this.el.querySelector('.g-sign')!.innerHTML = mark(sign)
    }
    this.el.querySelector('.g-label')!.textContent = step.name
    this.el.querySelector('.g-sub')!.textContent =
      step.reason && step.reason !== step.state ? `${step.state} · ${step.reason}` : step.state
  }
}
