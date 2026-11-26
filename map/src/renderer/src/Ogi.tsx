import { useLayoutEffect, useRef } from 'react'
import { ogiPaths, playOgi, XS } from './logo'
import './logo.css'

/**
 * The ōgi mark in --text. With px it is drawn for that size (the outer rib
 * pair drops at 24 and below); without it, it is the lockup, sized in em next
 * to the wordmark. xs overrides that drop (false keeps all four strings).
 * play runs the draw-in and strum once, on mount.
 */
export function Ogi({ px, xs, play, className = '' }: { px?: number; xs?: boolean; play?: boolean; className?: string }) {
  const ref = useRef<SVGSVGElement>(null)
  // Before paint, so the settled mark never flashes ahead of its draw-in.
  useLayoutEffect(() => (play ? playOgi(ref.current!) : undefined), [play])
  return (
    <svg
      ref={ref}
      className={`ogi${px === undefined ? ' lock' : ''} ${className}`}
      viewBox="-12 -12 24 24"
      width={px}
      height={px}
      aria-hidden="true"
      focusable="false"
    >
      {ogiPaths(px, xs ?? (px !== undefined && px <= XS)).map((p) => (
        <path
          key={p.i}
          data-pi={p.i}
          d={p.d}
          pathLength={1}
          strokeWidth={p.w}
          style={{ '--dl': `calc(var(--d3) * ${p.dl.toFixed(4)})`, '--du': `calc(var(--d3) * ${p.du.toFixed(4)})` } as React.CSSProperties}
        />
      ))}
    </svg>
  )
}
