// The terminal's paper (background) and ink (foreground) per scheme: the
// harp mockup's --a-bg and --a-fg. xterm (renderer) and the tmux styles
// (main) both read it, so the two stay in step.

export type Scheme = 'dark' | 'light'

export const PAPER: Record<Scheme, { fg: string; bg: string }> = {
  dark: { fg: '#d7d5cf', bg: '#0e0f12' },
  light: { fg: '#1d1f23', bg: '#fcfbf9' }
}

export function isScheme(s: unknown): s is Scheme {
  return s === 'dark' || s === 'light'
}
