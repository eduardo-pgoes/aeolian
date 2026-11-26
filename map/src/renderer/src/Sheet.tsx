import { type ReactNode, useEffect, useRef } from 'react'
import { Icon } from './icons'
import { world } from './store'

export type SheetDock = 'over' | 'side'

export interface SheetProps {
  label: string
  dock: SheetDock
  modal?: boolean
  head?: ReactNode
  foot?: ReactNode
  focusKey?: string
  onClose?: () => void
  closeTitle?: string
  className?: string
  children: ReactNode
}

const TABBABLE = 'button, [href], input, textarea, select, [tabindex]:not([tabindex="-1"])'

function tabbable(root: HTMLElement): HTMLElement[] {
  return [...root.querySelectorAll<HTMLElement>(TABBABLE)].filter(
    (el) => !el.matches(':disabled') && !el.closest('[inert]') && el.getClientRects().length > 0
  )
}

export function Scrim({ onClose }: { onClose: () => void }) {
  return <div className="scrim" aria-hidden="true" onPointerDown={onClose} />
}

export function Sheet({ label, dock, modal = false, head, foot, focusKey, onClose, closeTitle, className, children }: SheetProps) {
  const ref = useRef<HTMLElement>(null)

  useEffect(() => {
    const el = ref.current!
    if (modal && !el.contains(document.activeElement)) el.focus({ preventScroll: true })
  }, [modal, focusKey])

  useEffect(() => {
    const key = (e: KeyboardEvent): void => {
      const el = ref.current
      if (!el || e.defaultPrevented || world.paletteOpen || el.closest('[inert]')) return
      const a = document.activeElement
      const inside = !!a && el.contains(a)
      if (e.key === 'Escape' && onClose) {
        if (!inside && !(modal && (!a || a === document.body))) return
        e.preventDefault()
        onClose()
        return
      }
      if (e.key !== 'Tab' || !modal || !inside) return
      const all = tabbable(el)
      if (!all.length) return e.preventDefault()
      const first = all[0]
      const last = all[all.length - 1]
      if (e.shiftKey && (a === first || a === el)) {
        e.preventDefault()
        last.focus()
      } else if (!e.shiftKey && a === last) {
        e.preventDefault()
        first.focus()
      }
    }
    addEventListener('keydown', key)
    return () => removeEventListener('keydown', key)
  }, [onClose, modal])

  return (
    <section
      ref={ref}
      className={`sheet ${dock}${className ? ` ${className}` : ''}`}
      role={modal ? 'dialog' : 'complementary'}
      aria-modal={modal || undefined}
      aria-label={label}
      tabIndex={-1}
    >
      {(head || onClose) && (
        <header className="sheet-head">
          {head}
          {onClose && (
            <button className="ib sheet-x" aria-label={`close the ${label}`} title={closeTitle ?? 'close (esc)'} onClick={onClose}>
              <Icon name="close" />
            </button>
          )}
        </header>
      )}
      <div className="sheet-body">{children}</div>
      {foot && <footer className="sheet-foot">{foot}</footer>}
    </section>
  )
}
