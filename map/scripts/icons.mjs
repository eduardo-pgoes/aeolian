#!/usr/bin/env node
// Regenerates the app icons from the ōgi geometry in src/renderer/src/logo.ts.
//   node scripts/icons.mjs        (Node 23.6+ for the .ts import)
// Writes resources/icon.svg (the 512 master), resources/icon.png and
// resources/icons/<n>x<n>.png. Each size is drawn for its size, not scaled
// down from the master. Rasterizes with rsvg-convert, or resvg.
import { execFileSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { tileSvg } from '../src/renderer/src/logo.ts'

export const SIZES = [16, 22, 24, 32, 48, 64, 128, 256, 512]

const root = join(dirname(fileURLToPath(import.meta.url)), '..')
const out = join(root, 'resources')
mkdirSync(join(out, 'icons'), { recursive: true })

function has(bin) {
  try {
    execFileSync(bin, ['--version'], { stdio: 'ignore' })
    return true
  } catch {
    return false
  }
}
const rsvg = has('rsvg-convert')
if (!rsvg && !has('resvg')) throw new Error('needs rsvg-convert (librsvg) or resvg on PATH')

function raster(svg, px, png) {
  if (rsvg) execFileSync('rsvg-convert', ['-w', String(px), '-h', String(px), '-o', png, svg])
  else execFileSync('resvg', ['-w', String(px), '-h', String(px), svg, png])
}

const tmp = mkdtempSync(join(tmpdir(), 'ogi-'))
try {
  writeFileSync(join(out, 'icon.svg'), tileSvg(512, true))
  for (const px of SIZES) {
    const svg = join(tmp, `${px}.svg`)
    writeFileSync(svg, tileSvg(px, true))
    raster(svg, px, join(out, 'icons', `${px}x${px}.png`))
  }
  raster(join(out, 'icon.svg'), 512, join(out, 'icon.png'))
} finally {
  rmSync(tmp, { recursive: true, force: true })
}
console.log(`icons: ${SIZES.join(', ')} → ${out}`)
