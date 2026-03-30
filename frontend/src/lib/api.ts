// Thin wrapper around auto-generated Wails v3 bindings.

import {
  GetPageData as _GetPageData,
  GetServerInfo as _GetServerInfo,
  GetConnectionTypes as _GetConnectionTypes,
  GetConnection as _GetConnection,
  AddConnection as _AddConnection,
  SaveConnection as _SaveConnection,
  DeleteConnection as _DeleteConnection,
  TestConnection as _TestConnection,
  AddTunnel as _AddTunnel,
  SaveTunnel as _SaveTunnel,
  DeleteTunnel as _DeleteTunnel,
  SetupERP as _SetupERP,
  SyncERP as _SyncERP,
  SelfUpdate as _SelfUpdate,
  StartOAuth as _StartOAuth,
  GetOAuthStatus as _GetOAuthStatus,
  StartDeviceAuth as _StartDeviceAuth,
  GetDeviceAuthStatus as _GetDeviceAuthStatus,
} from '../../bindings/mux/app.js'

// Re-export types for convenience
export interface ServerInfo {
  version: string
  uptime: string
  port: number
  buildTime: string
  canSelfUpdate: boolean
}

export interface ERPInfo {
  configured: boolean
  endpoint: string
  tokenSet: boolean
  tunnels: number
  connections: number
  resultMessage?: string
  resultSuccess?: boolean
}

export interface TunnelInfo {
  name: string
  type: string                  // "wireguard" or "ssh"
  peerEndpoint?: string
  tunnelAddress?: string
  peerPublicKey?: string
  allowedIPs?: string
  dns?: string
  mtu?: number
  keepAlive?: number
  host?: string
  port?: number
  user?: string
  keyFile?: string
  insecureHostKey?: boolean
  source: string                // "local" or "erp"
  connected: boolean
  privateKeySet: boolean
  presharedKeySet?: boolean
}

export interface SaveTunnelRequest {
  peerPublicKey?: string
  peerEndpoint?: string
  allowedIPs?: string
  tunnelAddress?: string
  dns?: string
  mtu?: string
  keepAlive?: string
  privateKey?: string
  presharedKey?: string
  host?: string
  port?: string
  user?: string
  keyFile?: string
  insecureHostKey?: boolean
}

export interface FieldInfo {
  key: string
  label: string
  value: string
  placeholder: string
  secret: boolean
  small: boolean
  secretStored: boolean
}

export interface ConnInfo {
  name: string
  type: string
  typeLabel: string
  configured: boolean
  source: string
  tunnel: string
  summary: string
  isProxy: boolean
  isOAuth: boolean
  oauthOK: boolean
  isERP: boolean
  isDeviceAuth: boolean
  deviceAuthOK: boolean
  readOnly: boolean
  instructions: string
  fields: FieldInfo[]
}

export interface TypeListEntry {
  type: string
  label: string
}

export interface PageData {
  server: ServerInfo
  erp: ERPInfo
  tunnels: TunnelInfo[]
  connections: ConnInfo[]
  types: TypeListEntry[]
}

export interface TestResult {
  connection: string
  connected: boolean
  message: string
  latency?: string
}

export interface UpdateResult {
  success: boolean
  message: string
}

export interface OAuthStartResult {
  authURL: string
}

export interface OAuthStatus {
  authorized: boolean
  message: string
}

export interface DeviceAuthStart {
  userCode: string
  verificationURI: string
}

export interface DeviceAuthStatus {
  completed: boolean
  message: string
}

export interface SaveConnectionRequest {
  host?: string
  port?: string
  user?: string
  password?: string
  database?: string
  url?: string
  token?: string
  scopes?: string
  tunnel?: string
  instructions?: string
}

// API functions
export const GetPageData = _GetPageData as () => Promise<PageData>
export const GetServerInfo = _GetServerInfo as () => Promise<ServerInfo>
export const GetConnectionTypes = _GetConnectionTypes as () => Promise<TypeListEntry[]>
export const GetConnection = _GetConnection as (name: string) => Promise<ConnInfo | null>
export const AddConnection = _AddConnection as (name: string, type: string) => Promise<ConnInfo>
export const SaveConnection = _SaveConnection as (name: string, fields: SaveConnectionRequest) => Promise<ConnInfo>
export const DeleteConnection = _DeleteConnection as (name: string) => Promise<void>
export const TestConnection = _TestConnection as (name: string) => Promise<TestResult>
export const AddTunnel = _AddTunnel as (name: string, type: string) => Promise<TunnelInfo>
export const SaveTunnel = _SaveTunnel as (name: string, fields: SaveTunnelRequest) => Promise<TunnelInfo>
export const DeleteTunnel = _DeleteTunnel as (name: string) => Promise<void>
export const SetupERP = _SetupERP as (endpoint: string, token: string) => Promise<ERPInfo>
export const SyncERP = _SyncERP as () => Promise<PageData>
export const SelfUpdate = _SelfUpdate as () => Promise<UpdateResult>
export const StartOAuth = _StartOAuth as (name: string) => Promise<OAuthStartResult>
export const GetOAuthStatus = _GetOAuthStatus as (name: string) => Promise<OAuthStatus>
export const StartDeviceAuth = _StartDeviceAuth as (name: string) => Promise<DeviceAuthStart>
export const GetDeviceAuthStatus = _GetDeviceAuthStatus as (name: string) => Promise<DeviceAuthStatus>
