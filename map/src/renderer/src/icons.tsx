const PATHS = {
  right: 'M5.5 3.5 9 7l-3.5 3.5',
  left: 'M8.5 3.5 5 7l3.5 3.5',
  pin: 'M5 2h4M7 2v4.5M4 6.5h6l-1 2H5zM7 8.5V12',
  plus: 'M3.5 7h7M7 3.5v7',
  tray: 'M2 8.5h3l1 1.5h2l1-1.5h3M2 8.5 3.5 3h7L12 8.5V11H2z',
  search: 'M6.25 2.5a3.75 3.75 0 1 1 0 7.5 3.75 3.75 0 0 1 0-7.5zM9 9l3 3',
  cmd: 'M2.5 3.5h9v7h-9zM4.5 6l1.5 1-1.5 1M7.5 8.5h2',
  fit: 'M2.5 5V2.5H5M9 2.5h2.5V5M11.5 9v2.5H9M5 11.5H2.5V9',
  book: 'M2.5 3h3.5c.6 0 1 .4 1 1v7.5c0-.6-.4-1-1-1H2.5zM11.5 3H8c-.6 0-1 .4-1 1v7.5c0-.6.4-1 1-1h3.5z',
  runs: 'M3 4h8M3 7h8M3 10h5',
  theme: 'M7 2a5 5 0 1 0 0 10zM7 2a5 5 0 0 1 0 10',
  stop: 'M4 4h6v6H4z',
  end: 'M3 7h6M7 4.5 9.5 7 7 9.5M11.5 3.5v7',
  side: 'M2.5 3h9v8h-9zM5.5 3v8',
  close: 'M4 4l6 6M10 4l-6 6'
} as const

export type IconName = keyof typeof PATHS

export function Icon({ name, className }: { name: IconName; className?: string }) {
  return (
    <svg viewBox="0 0 14 14" aria-hidden="true" className={className}>
      <path d={PATHS[name]} />
    </svg>
  )
}
