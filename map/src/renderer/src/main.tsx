import { createRoot } from 'react-dom/client'
import '@xterm/xterm/css/xterm.css'
import '@fontsource-variable/instrument-sans/standard.css'
import '@fontsource-variable/instrument-sans/standard-italic.css'
import '@fontsource-variable/jetbrains-mono'
import '@fontsource-variable/jetbrains-mono/wght-italic.css'
import { initTheme } from './theme'
import { App } from './App'
import './tokens.css'
import './styles.css'

initTheme()
createRoot(document.getElementById('root')!).render(<App />)
