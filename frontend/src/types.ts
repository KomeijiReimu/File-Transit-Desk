export type UserRole = 'admin' | 'user' | string

// 后端 /api/auth/me 和登录接口返回的会话摘要；idleExpiresAt 用于前端展示空闲状态。
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

// 目录字段兼容新旧后端命名：canUpload/canDownload 与 allowUpload/allowDownload 均可渲染。
export interface DirectoryInfo {
  id: string
  name: string
  type?: 'directory' | 'file' | string
  label?: string
  root?: string
  description?: string
  canUpload?: boolean
  canDownload?: boolean
  readonly?: boolean
  allowUpload?: boolean
  allowDownload?: boolean
}

export interface SafeConfig {
  resources: DirectoryInfo[]
  storage: {
    uploadMaxMB: number
    uploadMaxFileMB: number
    uploadMaxFiles: number
    allowedExtensions: string[]
    blockedExtensions: string[]
  }
  tokens: {
    defaultTTLSeconds: number
    maxTTLSeconds: number
    uploadMaxMB: number
  }
  downloads: {
    leaseTTLSeconds: number
    contentHashMaxMB: number
  }
  configWritable: boolean
}

export interface UploadPolicyPayload {
  allowedExtensions: string[]
  blockedExtensions: string[]
}

export interface FilePickerRoot {
  id: string
  name: string
  path?: string
  allowSelectFiles: boolean
  allowSelectDirs: boolean
}

export interface FilePickerItem {
  name: string
  path: string
  type: 'file' | 'directory' | 'symlink' | 'other'
  size?: number | null
  modifiedAt?: string
  hidden: boolean
  symlink: boolean
  selectable: boolean
  readable: boolean
}

export interface FilePickerListResponse {
  rootId: string
  path: string
  parentPath: string
  sort: 'name' | 'type' | 'size' | 'modifiedAt'
  order: 'asc' | 'desc'
  page: number
  pageSize: number
  hasMore: boolean
  items: FilePickerItem[]
}

export interface FilePickerSelection {
  valid: boolean
  rootId: string
  path: string
  relativePath: string
  type: 'file' | 'directory'
  absolutePath: string
}

export interface ResourcePayload {
  id: string
  name: string
  type: 'directory' | 'file'
  path: string
  allowDownload: boolean
  allowUpload: boolean
}

// 文件列表同样兼容 isDir 与 type 字段，便于不同接口版本共用同一表格组件。
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

// TokenInfo 同时覆盖管理端列表、创建响应和公开分享信息页的可选字段。
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
