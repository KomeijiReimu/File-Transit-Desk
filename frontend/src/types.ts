export type UserRole = 'admin' | 'user' | string

// 后端 /api/auth/me 和登录接口返回的会话摘要；idleExpiresAt 用于前端展示空闲状态。
export interface UserInfo {
  authenticated: boolean
  // Opaque in-memory subject binding for replay prevention; never persist it.
  sessionBinding?: string
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

export interface UploadLimits {
  uploadMaxMB: number
  uploadMaxFileMB: number
  uploadMaxFiles: number
  uploadMaxBytes: number
  uploadMaxFileBytes: number
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
  type: 'file' | 'directory' | 'symlink' | 'other' | 'unknown' | string
  size?: number | null
  modifiedAt?: string
  hidden?: boolean
  symlink?: boolean
  selectable?: boolean
  readable?: boolean
  metadataKnown?: boolean
  downloadable?: boolean
}

export interface FilePickerListResponse {
  rootId: string
  path: string
  parentPath: string
  sort: 'name' | 'type' | 'size' | 'modifiedAt'
  order: 'asc' | 'desc'
  page?: number
  pageSize?: number
  hasMore?: boolean
  truncated?: boolean
  totalKnown?: boolean
  total?: number | null
  scannedEntries?: number
  scanLimit?: number
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
  type?: 'file' | 'dir' | 'directory' | 'symlink' | 'other' | 'unknown' | string
  isDir?: boolean
  size?: number | null
  modifiedAt?: string
  mtime?: string
  metadataKnown?: boolean
  downloadable?: boolean
  symlink?: boolean
  selectable?: boolean
}

export interface ListFilesResponse {
  dir?: DirectoryInfo | string
  path?: string
  entries?: FileEntry[]
  files?: FileEntry[]
  breadcrumbs?: Array<{ name: string; path: string }>
  canUpload?: boolean
  canDownload?: boolean
  page?: number
  pageSize?: number
  hasMore?: boolean
  truncated?: boolean
  totalKnown?: boolean
  total?: number | null
  scannedEntries?: number
  scanLimit?: number
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
  uploadMaxFileBytes?: number
  uploadRequestMaxBytes?: number
  allowedExtensions?: string[]
  blockedExtensions?: string[]
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

export interface ShareListenDiagnostic {
  source: 'listen'
  network: 'tcp4' | 'tcp6' | 'tcp'
  family: 'ipv4' | 'ipv6' | 'unknown'
  mode: 'wildcard' | 'specific' | 'hostname'
  host: string
  port: number
  address: string
  reachable: 'unknown'
}

export interface ShareOriginCandidate {
  origin: string
  label: string
  source: 'current' | 'configured' | 'interface' | string
  sources?: string[]
  scope?: 'loopback' | 'private' | 'link-local' | 'carrier-grade-nat' | 'global-unicast' | 'other' | 'hostname' | string
  interface?: string
  interfaces?: string[]
  listen?: ShareListenDiagnostic
  listenMatchStatus?: 'match' | 'mismatch' | 'unknown'
  listenMatch?: boolean
  reachable?: 'unknown'
}

export interface DownloadLeaseResponse {
  url: string
  expiresAt: string
}

export interface UploadLeaseRequest {
  dirId: string
  path: string
  fileName: string
  fileSize: number
}

export interface UploadLeaseResponse {
  lease: string
  uploadUrl: string
  rawUploadUrl?: string
  expiresAt: string
}

export interface PublicUploadLeaseRequest {
  fileName: string
  fileSize: number
}

export interface PublicUploadLeaseResponse extends UploadLeaseResponse {}

export interface TransferRecord {
  id: string
  type: 'upload' | 'download' | string
  status: 'active' | 'canceling' | string
  source: string
  dirId?: string
  path?: string
  fileName?: string
  totalBytes?: number
  transferredBytes?: number
  currentSpeedBps?: number
  averageSpeedBps?: number
  startedAt?: string
  updatedAt?: string
  clientIP?: string
  cancelable?: boolean
  bestEffort?: boolean
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
  status?: 'ok' | 'failed' | (string & {})
  detail?: string
}

export interface AuditLogPage {
  logs: AuditLog[]
  page: number
  pageSize: number
  total: number
  totalPages: number
}

export type ChatMessageRole = 'user' | 'admin'
export type ChatMessageStatus = 'active' | 'withdrawn' | 'deleted'
export type ChatChangeKind = 'create' | 'withdraw' | 'delete'

export interface ChatMessage {
  id: number
  authorTag: string
  role: ChatMessageRole
  body: string | null
  status: ChatMessageStatus
  isMine: boolean
  createdAt: string
  withdrawnAt: string | null
  deletedAt: string | null
  canWithdraw: boolean
  withdrawUntil: string | null
  sourceIP: string
}

export interface ChatChange {
  seq: number
  kind: ChatChangeKind
  createdAt: string
  message: ChatMessage
}

export interface ChatHistoryResponse {
  messages: ChatMessage[]
  nextBeforeId: number | null
  hasMore: boolean
  latestChangeSeq: number
  generation: number
}

export interface ChatChangesResponse {
  changes: ChatChange[]
  generation: number
  nextAfterSeq: number
  hasMore: boolean
  latestChangeSeq: number
}

export interface ChatMutationResponse {
  message: ChatMessage
  // eventSeq 只描述本次变更，不能作为全局 changes 游标。
  eventSeq: number
}

export interface ChatBatchDeleteResponse {
  deletedCount: number
  mutations: ChatMutationResponse[]
}

export interface ChatClearResponse {
  clearedCount: number
  generation: number
  latestChangeSeq: number
}

export interface ChatCapabilities {
  maxMessageChars: number
  maxMessageBytes: number
  maxRequestBytes: number
  withdrawWindowSeconds: number
  historyDefaultLimit: number
  historyMaxLimit: number
  changesDefaultLimit: number
  changesMaxLimit: number
}
