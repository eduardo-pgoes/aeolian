import { contextBridge, ipcRenderer } from 'electron'
import type { Scheme } from '../shared/paper'
import type { AccountsView, Agent, DigestView, HostEvent, HostInfo, InboxEntry, Run, WorkflowEntry, WorkflowRef } from '../main/host'

const map = {
  listRuns: (include?: string | null): Promise<Run[]> => ipcRenderer.invoke('runs:list', include),
  hostInfo: (): Promise<HostInfo> => ipcRenderer.invoke('host:info'),
  inbox: (): Promise<InboxEntry[]> => ipcRenderer.invoke('inbox:list'),
  digest: (since: string): Promise<DigestView> => ipcRenderer.invoke('digest:get', since),
  ack: (runId: string): Promise<Run> => ipcRenderer.invoke('run:ack', runId),
  dismissDigest: (keys: string[]): Promise<{ keys: string[] }> => ipcRenderer.invoke('digest:dismiss', keys),
  kill: (agentId: string): Promise<Agent> => ipcRenderer.invoke('agent:kill', agentId),
  workflows: (): Promise<WorkflowEntry[]> => ipcRenderer.invoke('workflows:list'),
  startRun: (workflow: WorkflowRef, goal: string, account = ''): Promise<Run> =>
    ipcRenderer.invoke('run:start', workflow, goal, account),
  accounts: (): Promise<AccountsView> => ipcRenderer.invoke('accounts:list'),
  killRun: (runId: string): Promise<Run> => ipcRenderer.invoke('run:kill', runId),
  endRun: (runId: string, force = false): Promise<Run> => ipcRenderer.invoke('run:end', runId, force),
  resume: (agentId: string): Promise<string> => ipcRenderer.invoke('agent:resume', agentId),
  attach: (name: string, cols: number, rows: number, color: string, scheme: Scheme): Promise<number> =>
    ipcRenderer.invoke('term:attach', name, cols, rows, color, scheme),
  restyle: (id: number, color: string, scheme: Scheme): void => ipcRenderer.send('term:restyle', id, color, scheme),
  write: (id: number, data: string): void => ipcRenderer.send('term:write', id, data),
  resize: (id: number, cols: number, rows: number): void => ipcRenderer.send('term:resize', id, cols, rows),
  close: (id: number): void => ipcRenderer.send('term:close', id),
  newWindow: (): void => ipcRenderer.send('window:new'),
  copy: (text: string): void => ipcRenderer.send('clipboard:write', text),
  setTheme: (t: 'dark' | 'light' | null): void => ipcRenderer.send('theme:set', t),
  onData: (cb: (id: number, data: string) => void): void => {
    ipcRenderer.on('term:data', (_e, id: number, data: string) => cb(id, data))
  },
  onExit: (cb: (id: number, code: number) => void): void => {
    ipcRenderer.on('term:exit', (_e, id: number, code: number) => cb(id, code))
  },
  // onEvent and onStream return a function that stops the callback.
  onEvent: (cb: (ev: HostEvent) => void): (() => void) => {
    const fn = (_e: unknown, ev: HostEvent): void => cb(ev)
    ipcRenderer.on('host:event', fn)
    return () => ipcRenderer.off('host:event', fn)
  },
  onStream: (cb: (s: { connected: boolean }) => void): (() => void) => {
    const fn = (_e: unknown, s: { connected: boolean }): void => cb(s)
    ipcRenderer.on('host:stream', fn)
    return () => ipcRenderer.off('host:stream', fn)
  },
  streamState: (): Promise<{ connected: boolean }> => ipcRenderer.invoke('host:stream')
}

export type MapApi = typeof map

contextBridge.exposeInMainWorld('map', map)
