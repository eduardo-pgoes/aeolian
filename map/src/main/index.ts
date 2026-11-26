import { app, BrowserWindow, clipboard, ipcMain, Menu, nativeTheme } from 'electron'
import { join } from 'node:path'
import * as events from './events'
import * as host from './host'
import * as terminals from './terminals'
import type { Scheme } from '../shared/paper'
import icon from '../../resources/icon.png?asset'

function openWindow(): void {
  const win = new BrowserWindow({
    width: 1200,
    height: 800,
    title: 'aeolian map',
    frame: false,
    icon,
    webPreferences: {
      preload: join(__dirname, '../preload/index.js'),
      sandbox: false
    }
  })
  const contentsId = win.webContents.id
  win.webContents.on('did-start-loading', () => terminals.closeOwnedBy(contentsId))
  win.on('closed', () => terminals.closeOwnedBy(contentsId))
  if (process.env.ELECTRON_RENDERER_URL) {
    win.loadURL(process.env.ELECTRON_RENDERER_URL)
  } else {
    win.loadFile(join(__dirname, '../renderer/index.html'))
  }
}

// Only these accelerators are taken from the terminal; Electron's default
// menu would also steal Ctrl+R and Ctrl+W from the agent.
const menu = Menu.buildFromTemplate([
  {
    label: 'Map',
    submenu: [
      { label: 'New Window', accelerator: 'CmdOrCtrl+Shift+N', click: openWindow },
      { role: 'toggleDevTools' },
      { type: 'separator' },
      { role: 'quit' }
    ]
  }
])

ipcMain.handle('runs:list', (_e, include?: string | null) => host.listRuns(include))
ipcMain.handle('host:stream', () => events.streamState())
ipcMain.handle('host:info', () => host.info())
ipcMain.handle('inbox:list', () => host.inbox())
ipcMain.handle('digest:get', (_e, since: string) => host.digest(since))
ipcMain.handle('run:ack', (_e, id: string) => host.ack(id))
ipcMain.handle('digest:dismiss', (_e, keys: string[]) => host.dismissDigest(keys))
ipcMain.handle('agent:kill', (_e, id: string) => host.kill(id))
ipcMain.handle('workflows:list', () => host.listWorkflows())
ipcMain.handle('run:start', (_e, workflow: host.WorkflowRef, goal: string, account?: string) =>
  host.startRun(workflow, goal, account)
)
ipcMain.handle('accounts:list', () => host.listAccounts())
ipcMain.handle('run:kill', (_e, id: string) => host.killRun(id))
ipcMain.handle('run:end', (_e, id: string, force: boolean) => host.endRun(id, force))
ipcMain.handle('agent:resume', (_e, id: string) => host.resume(id))
ipcMain.handle('term:attach', (e, name: string, cols: number, rows: number, color: string, scheme: Scheme) =>
  terminals.attach(e.sender, name, cols, rows, color, scheme)
)
ipcMain.on('term:write', (e, id: number, data: string) => terminals.write(e.sender.id, id, data))
ipcMain.on('term:resize', (e, id: number, cols: number, rows: number) =>
  terminals.resize(e.sender.id, id, cols, rows)
)
ipcMain.on('term:restyle', (e, id: number, color: string, scheme: Scheme) =>
  terminals.restyle(e.sender.id, id, color, scheme)
)
ipcMain.on('term:close', (e, id: number) => terminals.close(e.sender.id, id))
ipcMain.on('window:new', openWindow)
ipcMain.on('clipboard:write', (_e, text: string) => clipboard.writeText(text))
// The renderer's theme override, or null to follow the OS. Native chrome
// (menus, scrollbars, dialogs) follows it.
ipcMain.on('theme:set', (_e, t: 'dark' | 'light' | null) => {
  nativeTheme.themeSource = t === 'dark' || t === 'light' ? t : 'system'
})

app.whenReady().then(() => {
  Menu.setApplicationMenu(menu)
  openWindow()
  events.start()
})

app.on('window-all-closed', () => app.quit())
app.on('will-quit', () => {
  events.stop()
  terminals.closeAll()
})
