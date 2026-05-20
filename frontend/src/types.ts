export type UserRole = 'admin' | 'user' | string

export interface UserInfo {
  authenticated: boolean
  name?: string
  role?: UserRole
  expiresAt?: string
  idleExpiresAt?: string
}

export interface AdminLoginPayload {
  username: string
  password: string
}

export interface DirectoryInfo {
  id: string
  name: string
  label?: string
  root?: string
  description?: string
  canUpload?: boolean
  canDownload?: boolean
  readonly?: boolean
  allowUpload?: boolean
  allowDownload?: boolean
}

export interface FileEntry {
  name: string
  path?: string
  type?: 'file' | 'dir' | 'directory'
  isDir?: boolean
  size?: number
  modifiedAt?: string
  mtime?: string
}

export interface ListFilesResponse {
  dir?: DirectoryInfo | string
  path?: string
  entries?: FileEntry[]
  files?: FileEntry[]
  breadcrumbs?: Array<{ name: string; path: string }>
  canUpload?: boolean
  canDownload?: boolean
}

export interface TokenInfo {
  id: string | number
  token?: string
  kind?: 'download' | 'upload'
  type?: 'download' | 'upload'
  dirId?: string
  dir?: string
  dirName?: string
  path?: string
  expiresAt?: string
  maxUses?: number
  uses?: number
  used?: number
  uploadedBytes?: number
  uploadMaxBytes?: number
  revoked?: boolean
  createdAt?: string
  url?: string
  link?: string
  infoUrl?: string
  valid?: boolean
  reason?: string
  actionLabel?: string
}

export interface CreateTokenPayload {
  type: 'download' | 'upload'
  dirId: string
  path: string
  ttlMinutes: number
  maxUses: number
}

export interface CreateTokenResponse extends TokenInfo {
  token: string
  url: string
}

export interface DownloadLeaseResponse {
  url: string
  expiresAt: string
}

export interface AuditLog {
  id?: string | number
  time?: string
  createdAt?: string
  action: string
  actionLabel?: string
  dirId?: string
  path?: string
  ip?: string
  userAgent?: string
  status?: string
  detail?: string
}
