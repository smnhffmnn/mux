import { writable } from 'svelte/store'
import type { PageData, ServerInfo } from './api'

export const pageData = writable<PageData | null>(null)
export const serverInfo = writable<ServerInfo | null>(null)
export const activeView = writable<'connections' | 'tunnels' | 'erp' | 'about'>('connections')
export const loading = writable(false)
