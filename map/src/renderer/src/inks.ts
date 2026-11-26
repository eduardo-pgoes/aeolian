// Inks are the hues in tokens.css (--sakura, --wisteria, ...), one set per
// theme. ink() resolves a name to the concrete hex of the current theme, so
// callers that bake the color in (inline --c, SVG strokes, xterm) must
// re-apply it on onTheme(). Any other name is the plain ink, which is --text.

export const INKS = ['sakura', 'wisteria', 'sky', 'kaki', 'matcha'] as const

let table: Record<string, string> = {}
let tableTheme: string | undefined

function resolve(): Record<string, string> {
  const root = document.documentElement
  const t = root.dataset.theme ?? ''
  if (t !== tableTheme) {
    const css = getComputedStyle(root)
    table = Object.fromEntries(['text', ...INKS].map((n) => [n, css.getPropertyValue(`--${n}`).trim()]))
    tableTheme = t
  }
  return table
}

export function isInk(name: string): boolean {
  return (INKS as readonly string[]).includes(name)
}

export function ink(name: string): string {
  const t = resolve()
  return (isInk(name) && t[name]) || t.text
}
