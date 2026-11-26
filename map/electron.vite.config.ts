import { defineConfig } from 'electron-vite'
import react from '@vitejs/plugin-react'

const FONT = /\.(woff2?|ttf|otf)$/

export default defineConfig({
  main: {},
  preload: {},
  renderer: {
    plugins: [react()],
    build: {
      // The CSP's default-src 'self' blocks data: fonts, and some subsets are under the inline limit.
      assetsInlineLimit: (file) => (FONT.test(file) ? false : undefined)
    }
  }
})
