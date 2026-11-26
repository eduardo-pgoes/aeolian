import { execFile } from 'child_process'
import type { WebContents } from 'electron'
import * as pty from 'node-pty'
import { isScheme, PAPER, type Scheme } from '../shared/paper'

const SOCKET = 'aeolian'
const SESSION_PREFIX = 'aeolian-'

interface Terminal {
  pty: pty.IPty
  owner: number
  name: string
}

const terminals = new Map<number, Terminal>()
let nextId = 1

const SESSION_NAME = /^[A-Za-z0-9_.-]+$/

const HEX = /^#[0-9a-f]{6}$/i

// mix blends hex a into hex b, a share of p.
function mix(a: string, b: string, p: number): string {
  const ch = (h: string, i: number): number => parseInt(h.slice(1 + i * 2, 3 + i * 2), 16)
  const out = [0, 1, 2].map((i) => Math.round(ch(a, i) * p + ch(b, i) * (1 - p)))
  return '#' + out.map((v) => v.toString(16).padStart(2, '0')).join('')
}

// style dresses the session's copy mode and messages in the card's ink on
// the current scheme's paper. It runs before the client attaches, so
// sessions started before a change pick it up too, and again on a theme flip.
function style(name: string, color: string, scheme: Scheme): string[][] {
  const { fg, bg } = PAPER[isScheme(scheme) ? scheme : 'light']
  const c = HEX.test(color) ? color : fg
  const wash = mix(c, bg, 0.25)
  const s = `=${name}:`
  return [
    ['set', '-t', s, 'message-style', `fg=${fg},bg=${wash}`],
    ['set', '-wt', s, 'mode-style', `fg=${fg},bg=${wash}`],
    ['set', '-wt', s, 'copy-mode-match-style', `fg=${fg},bg=${mix(c, bg, 0.45)}`],
    ['set', '-wt', s, 'copy-mode-current-match-style', `fg=${bg},bg=${c}`]
  ]
}

// look is everything attach sets up. The card's head is the status line; the
// mouse scrolls history and selects in copy mode, and a selection reaches
// the system clipboard through OSC 52.
function look(name: string, color: string, scheme: Scheme): string[] {
  const s = `=${name}:`
  return [
    ['set', '-s', 'focus-events', 'on'],
    ['set', '-s', 'escape-time', '20'],
    ['set', '-s', 'set-clipboard', 'on'],
    ['set', '-t', s, 'status', 'off'],
    ['set', '-t', s, 'mouse', 'on'],
    ...style(name, color, scheme)
  ].flatMap((cmd) => [...cmd, ';'])
}

// Each terminal is its own tmux client; ending it detaches that client and
// nothing else. This file must only ever run `attach`, never with -d.
export function attach(
  owner: WebContents,
  name: string,
  cols: number,
  rows: number,
  color: string,
  scheme: Scheme
): number {
  if (!name.startsWith(SESSION_PREFIX) || !SESSION_NAME.test(name)) {
    throw new Error(`not a keeper session: ${name}`)
  }
  const argv = ['-L', SOCKET, ...look(name, color, scheme), 'attach', '-t', `=${name}`]
  const p = pty.spawn('tmux', argv, {
    name: 'xterm-256color',
    cols,
    rows,
    cwd: process.env.HOME,
    env: clientEnv()
  })
  const id = nextId++
  const ownerId = owner.id
  terminals.set(id, { pty: p, owner: ownerId, name })
  p.onData((data) => {
    if (!owner.isDestroyed()) owner.send('term:data', id, data)
  })
  p.onExit(({ exitCode }) => {
    terminals.delete(id)
    if (!owner.isDestroyed()) owner.send('term:exit', id, exitCode)
  })
  return id
}

function clientEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const [k, v] of Object.entries(process.env)) {
    if (v !== undefined && k !== 'TMUX' && k !== 'TMUX_PANE') env[k] = v
  }
  env.TERM = 'xterm-256color'
  return env
}

function owned(sender: number, id: number): Terminal | undefined {
  const t = terminals.get(id)
  return t?.owner === sender ? t : undefined
}

export function write(sender: number, id: number, data: string): void {
  owned(sender, id)?.pty.write(data)
}

export function resize(sender: number, id: number, cols: number, rows: number): void {
  if (cols > 0 && rows > 0) owned(sender, id)?.pty.resize(cols, rows)
}

// restyle re-runs just the style commands for a terminal's session, after
// the theme flips or its ink changes.
export function restyle(sender: number, id: number, color: string, scheme: Scheme): void {
  const t = owned(sender, id)
  if (!t) return
  const argv = ['-L', SOCKET, ...style(t.name, color, scheme).flatMap((cmd) => [...cmd, ';'])]
  execFile('tmux', argv, { env: clientEnv() }, (err) => {
    if (err) console.warn(`tmux restyle ${t.name}: ${err.message}`)
  })
}

export function close(sender: number, id: number): void {
  const t = owned(sender, id)
  if (t) end(id, t)
}

export function closeOwnedBy(owner: number): void {
  for (const [id, t] of terminals) if (t.owner === owner) end(id, t)
}

export function closeAll(): void {
  for (const [id, t] of terminals) end(id, t)
}

function end(id: number, t: Terminal): void {
  terminals.delete(id)
  t.pty.kill()
}
