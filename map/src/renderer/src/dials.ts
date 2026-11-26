export const DIALS = {
  minZoom: 0.05,
  maxZoom: 2.4,
  camRestMs: 150,
  fitPad: 64,
  focusFill: 0.92,
  focusMax: 1,
  semanticPx: 330,

  snap: { k: 380, c: 24 },
  drawer: { k: 220, c: 22 },
  cam: { k: 140, c: 20 },
  // a handoff's pulse down the rule (--d3)
  pulseMs: 640,

  settle: 0.24,
  fadeIn: 0.16,
  ruleIn: 0.13,
  ruleDraw: 0.22,
  dockDraw: 0.14,

  termFont: 13,
  termLineHeight: 1.2,
  // A terminal's rows are the tmux window's rows: attach turns tmux's status
  // line off (main/terminals.ts), so there is no extra row to count.
  // Wide is fine, tall is not: every size the map asks for is clamped to
  // maxRows, and a taller session shrinks to it on attach (it scrolls in tmux).
  startCols: 200,
  startRows: 32,
  minCols: 20,
  minRows: 4,
  maxRows: 48,

  headPx: 30,
  bandPx: 12,
  padPx: 16,
  // the gap between the string and the first terminal row (--gut)
  gutPx: 8,
  gapX: 56,
  gapY: 88,
  // a helper's top sits on its parent's string
  dockY: 42,
  cardW: 560,
  cardH: 200,

  // pollMs is the poll while the host's event stream is down;
  // pollFallbackMs while it is up, when events trigger the polls.
  pollMs: 1000,
  pollFallbackMs: 10_000,
  eventDebounceMs: 80,
  settleMs: 2500
}
