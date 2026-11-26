import type { MapApi } from '../../preload'

declare global {
  interface Window {
    map: MapApi
  }
}
