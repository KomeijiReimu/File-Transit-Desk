import router from '@/router'
import { buildTransferUrl } from '@/utils'
import type {
  AdminLoginPayload,
  AuditLog,
  AuditLogPage,
  CreateTokenPayload,
  CreateTokenResponse,
  DirectoryInfo,
  DownloadLeaseResponse,
  FilePickerListResponse,
  FilePickerRoot,
  FilePickerSelection,
  ListFilesResponse,
  PublicUploadLeaseRequest,
  PublicUploadLeaseResponse,
  ResourcePayload,
  SafeConfig,
  ShareOriginCandidate,
  TokenInfo,
  TransferRecord,
  UploadLeaseRequest,
  UploadLeaseResponse,
  UploadPolicyPayload,
  UploadLimits,
  UserInfo,
} from '@/types'

export class ApiError extends Error {
  status: number
  details?: unknown
  code?: string
  retryAfter?: number
  aborted: boolean

  constructor(message: string, status: number, details?: unknown, code?: string, retryAfter?: number) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.details = details
    this.code = code
    this.retryAfter = retryAfter
    this.aborted = Boolean(details && typeof details === 'object' && 'aborted' in details && (details as { aborted?: boolean }).aborted)
  }
}

// suppressAuthRedirect 用于心跳、会话恢复和公开页，避免后台探测接口把用户强制跳走。
type ApiRequestInit = RequestInit & { suppressAuthRedirect?: boolean }
type UploadOptions = {
  onProgress?: (progress: { loaded: number; total: number; percent: number }) => void
  signal?: AbortSignal
  suppressAuthRedirect?: boolean
  withCredentials?: boolean
  headers?: Record<string, string>
}

type HeaderReader = Pick<Headers, 'get'>

function safeServerMessage(value: unknown) {
  if (typeof value !== 'string' && typeof value !== 'number') return ''
  const message = String(value).trim()
  if (!message || /<!doctype\s+html|<html\b|<\/?[a-z][^>]*>/i.test(message)) return ''
  return message
}

function parseRetryAfter(headers: HeaderReader) {
  const value = headers.get('Retry-After')?.trim()
  if (!value) return undefined
  if (/^\d+(?:\.\d+)?$/.test(value)) {
    const seconds = Number(value)
    return Number.isFinite(seconds) ? Math.max(0, Math.ceil(seconds)) : undefined
  }
  const timestamp = Date.parse(value)
  if (!Number.isFinite(timestamp)) return undefined
  return Math.max(0, Math.ceil((timestamp - Date.now()) / 1000))
}

export function parseErrorPayload(status: number, headers: HeaderReader, body?: unknown, text = '') {
  const record = body && typeof body === 'object' && !Array.isArray(body) ? body as Record<string, unknown> : undefined
  const plainText = typeof body === 'string' ? safeServerMessage(body) : body == null ? safeServerMessage(text) : ''
  const message = safeServerMessage(record?.message) || safeServerMessage(record?.error) || plainText || `请求失败（${status}）`
  const code = safeServerMessage(record?.code) || undefined
  return {
    message,
    code,
    retryAfter: parseRetryAfter(headers),
    details: body,
  }
}

function handleAuthExpired(status: number, suppressAuthRedirect: boolean) {
  if (suppressAuthRedirect || status !== 401 || router.currentRoute.value.meta?.public === true || router.currentRoute.value.name === 'login') return
  // 统一广播会话过期，auth.ts 负责清理本地状态，路由负责回到登录页。
  window.dispatchEvent(new Event('ft:auth-expired'))
  void router.replace({ name: 'login', query: { redirect: router.currentRoute.value.fullPath } })
}

function responsePayload(contentType: string, text: string) {
  const trimmed = text.trim()
  const looksLikeJson = contentType.includes('application/json') || contentType.includes('+json') || trimmed.startsWith('{') || trimmed.startsWith('[')
  return looksLikeJson && text ? safeJSON(text) : text
}

function apiError(status: number, headers: HeaderReader, payload: unknown, text: string, suppressAuthRedirect: boolean) {
  const parsed = parseErrorPayload(status, headers, payload, text)
  handleAuthExpired(status, suppressAuthRedirect)
  return new ApiError(parsed.message, status, parsed.details, parsed.code, parsed.retryAfter)
}

async function parseResponse<T>(response: Response, suppressAuthRedirect = false): Promise<T> {
  const contentType = response.headers.get('content-type') || ''
  const text = await response.text().catch(() => '')
  const payload = responsePayload(contentType, text)

  if (!response.ok) {
    throw apiError(response.status, response.headers, payload, text, suppressAuthRedirect)
  }

  return (payload ?? {}) as T
}

async function request<T>(url: string, options: ApiRequestInit = {}): Promise<T> {
  const { suppressAuthRedirect = false, ...fetchOptions } = options
  const headers = new Headers(options.headers)
  const isFormData = options.body instanceof FormData
  if (!isFormData && options.body && !headers.has('Content-Type')) {
    // multipart 让浏览器自动补 boundary；其他有 body 的请求默认按 JSON 发送。
    headers.set('Content-Type', 'application/json')
  }
  try {
    return await parseResponse<T>(
      await fetch(url, {
        credentials: 'include',
        ...fetchOptions,
        headers,
      }),
      suppressAuthRedirect,
    )
  } catch (err) {
    if (err instanceof ApiError) throw err
    if (err instanceof DOMException && err.name === 'AbortError') throw new ApiError('请求已取消。', 0, { aborted: true })
    throw new ApiError('无法连接服务器，请检查后端服务或网络连接。', 0, err)
  }
}

async function publicRequest<T>(url: string, options: ApiRequestInit = {}): Promise<T> {
  return request<T>(url, {
    ...options,
    credentials: 'omit',
    suppressAuthRedirect: true,
  })
}

function uploadForm<T>(url: string, form: FormData, options: UploadOptions = {}): Promise<T> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const { onProgress, signal, suppressAuthRedirect = false, withCredentials = true, headers } = options
    let settled = false
    const abortHandler = () => xhr.abort()
    const cleanup = () => signal?.removeEventListener('abort', abortHandler)
    const rejectOnce = (err: ApiError) => {
      if (settled) return
      settled = true
      cleanup()
      reject(err)
    }
    const resolveOnce = (value: T) => {
      if (settled) return
      settled = true
      cleanup()
      resolve(value)
    }
    if (signal?.aborted) {
      rejectOnce(new ApiError('上传已取消。', 0, { aborted: true }))
      return
    }
    signal?.addEventListener('abort', abortHandler, { once: true })
    try {
      xhr.open('POST', url)
      // 上传不设置固定总时长；大型文件可以持续传输数小时，取消仅由用户或 AbortSignal 触发。
      xhr.timeout = 0
      xhr.withCredentials = withCredentials
      Object.entries(headers || {}).forEach(([key, value]) => xhr.setRequestHeader(key, value))
      xhr.upload.onprogress = (event) => {
        const total = event.lengthComputable && event.total > 0 ? event.total : 0
        const loaded = event.loaded || 0
        const percent = total > 0 ? Math.min(100, Math.round((loaded / total) * 100)) : 0
        onProgress?.({ loaded, total, percent })
      }
      xhr.onerror = () => rejectOnce(new ApiError('上传连接中断。请检查网络、后端是否仍在运行，或文件是否超过服务端/代理上传上限。', 0))
      xhr.onabort = () => rejectOnce(new ApiError('上传已取消。', 0, { aborted: true }))
      xhr.onload = () => {
        const contentType = xhr.getResponseHeader('content-type') || ''
        const text = xhr.responseText || ''
        const payload = responsePayload(contentType, text)
        if (xhr.status < 200 || xhr.status >= 300) {
          rejectOnce(apiError(xhr.status, { get: (name) => xhr.getResponseHeader(name) }, payload, text, suppressAuthRedirect))
          return
        }
        resolveOnce((payload || {}) as T)
      }
      xhr.send(form)
    } catch (err) {
      rejectOnce(new ApiError('上传请求无法发送。', 0, err))
    }
  })
}

function uploadRaw<T>(rawUploadUrl: string, lease: string, file: File, options: UploadOptions = {}): Promise<T> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest()
    const { onProgress, signal } = options
    let settled = false
    const abortHandler = () => xhr.abort()
    const cleanup = () => signal?.removeEventListener('abort', abortHandler)
    const rejectOnce = (err: ApiError) => {
      if (settled) return
      settled = true
      cleanup()
      reject(err)
    }
    const resolveOnce = (value: T) => {
      if (settled) return
      settled = true
      cleanup()
      resolve(value)
    }
    if (signal?.aborted) {
      rejectOnce(new ApiError('上传已取消。', 0, { aborted: true }))
      return
    }
    signal?.addEventListener('abort', abortHandler, { once: true })
    try {
      xhr.open('POST', buildTransferUrl(rawUploadUrl))
      // 原始上传是一文件一请求，不设置会中断长时间传输的固定总超时。
      xhr.timeout = 0
      xhr.withCredentials = false
      xhr.setRequestHeader('Authorization', `Bearer ${lease}`)
      xhr.upload.onprogress = (event) => {
        const total = event.lengthComputable && event.total > 0 ? event.total : 0
        const loaded = event.loaded || 0
        const percent = total > 0 ? Math.min(100, Math.round((loaded / total) * 100)) : 0
        onProgress?.({ loaded, total, percent })
      }
      xhr.onerror = () => rejectOnce(new ApiError('上传连接中断。请检查网络、后端是否仍在运行，或文件是否超过服务端/代理上传上限。', 0))
      xhr.onabort = () => rejectOnce(new ApiError('上传已取消。', 0, { aborted: true }))
      xhr.onload = () => {
        const contentType = xhr.getResponseHeader('content-type') || ''
        const text = xhr.responseText || ''
        const payload = responsePayload(contentType, text)
        if (xhr.status < 200 || xhr.status >= 300) {
          // Bearer lease 已独立授权，401 只反馈给当前上传，绝不改变页面登录状态。
          rejectOnce(apiError(xhr.status, { get: (name) => xhr.getResponseHeader(name) }, payload, text, true))
          return
        }
        resolveOnce((payload || {}) as T)
      }
      xhr.send(file)
    } catch (err) {
      rejectOnce(new ApiError('上传请求无法发送。', 0, err))
    }
  })
}

function safeJSON(text: string) {
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

const query = (params: Record<string, string | number | undefined>) => {
  // 忽略空值，避免 path='' 这类根路径被序列化成多余查询参数。
  const search = new URLSearchParams()
  Object.entries(params).forEach(([key, value]) => {
    if (value !== undefined && value !== '') search.set(key, String(value))
  })
  const text = search.toString()
  return text ? `?${text}` : ''
}

export interface AuditFilter {
  limit?: number
  page?: number
  pageSize?: number
  action?: string
  status?: string
  keyword?: string
}

export const api = {
  login: (totp: string) =>
    request<UserInfo>('/api/auth/login', { method: 'POST', body: JSON.stringify({ totp, code: totp }) }),
  adminLogin: (payload: AdminLoginPayload) =>
    request<UserInfo>('/api/auth/admin-login', { method: 'POST', body: JSON.stringify(payload) }),
  me: () => request<UserInfo>('/api/auth/me', { suppressAuthRedirect: true }),
  heartbeat: () =>
    request<{ ok: boolean; idleExpiresAt?: string }>('/api/auth/heartbeat', {
      method: 'POST',
      suppressAuthRedirect: true,
    }),
  logout: () => request<{ ok: boolean }>('/api/auth/logout', { method: 'POST' }),
  dirs: () => request<DirectoryInfo[]>('/api/dirs'),
  uploadLimits: () => request<UploadLimits>('/api/upload-policy'),
  listFiles: (dirId: string, path = '', page = 1, pageSize = 100) =>
    request<ListFilesResponse>(`/api/files/list${query({ dirId, path, page, pageSize })}`),
  createUploadLease: (payload: UploadLeaseRequest) =>
    request<UploadLeaseResponse>('/api/files/upload-lease', { method: 'POST', body: JSON.stringify(payload) }),
  uploadByLease: (lease: UploadLeaseResponse, file: File, options: UploadOptions = {}) => {
    if (lease.rawUploadUrl) {
      return uploadRaw<{ ok: boolean; uploaded?: number }>(lease.rawUploadUrl, lease.lease, file, {
        ...options,
        suppressAuthRedirect: true,
      })
    }
    const form = new FormData()
    form.append('files', file)
    return uploadForm<{ ok: boolean; uploaded?: number }>(buildTransferUrl(lease.uploadUrl), form, {
      ...options,
      suppressAuthRedirect: true,
      withCredentials: false,
      headers: { ...(options.headers || {}), Authorization: `Bearer ${lease.lease}` },
    })
  },
  createDownloadLease: (dirId: string, path: string) =>
    request<DownloadLeaseResponse>('/api/files/download-lease', {
      method: 'POST',
      body: JSON.stringify({ dirId, path }),
    }),
  tokens: () => request<TokenInfo[]>('/api/tokens'),
  shareOrigins: (currentOrigin: string) => request<ShareOriginCandidate[]>(`/api/share-origins${query({ currentOrigin })}`),
  createToken: (payload: CreateTokenPayload) =>
    request<CreateTokenResponse>('/api/tokens', { method: 'POST', body: JSON.stringify(payload) }),
  revokeToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}/revoke`, { method: 'POST' }),
  deleteToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}`, { method: 'DELETE' }),
  auditLogs: (filter: AuditFilter = {}) =>
    request<AuditLog[]>(`/api/audit/logs${query({ limit: filter.limit, action: filter.action, status: filter.status, keyword: filter.keyword })}`),
  auditLogPage: (filter: AuditFilter = {}, signal?: AbortSignal) =>
    request<AuditLogPage>(`/api/audit/logs${query({ page: filter.page, pageSize: filter.pageSize, action: filter.action, status: filter.status, keyword: filter.keyword })}`, { signal }),
  activeTransfers: () => request<{ transfers: TransferRecord[] }>('/api/transfers/active'),
  cancelTransfer: (id: string) => request<{ ok: boolean }>(`/api/transfers/${encodeURIComponent(id)}/cancel`, { method: 'POST' }),
  safeConfig: () => request<SafeConfig>('/api/config'),
  updateUploadPolicy: (payload: UploadPolicyPayload) =>
    request<UploadPolicyPayload>('/api/config/upload-policy', { method: 'PUT', body: JSON.stringify(payload) }),
  filePickerRoots: () => request<FilePickerRoot[]>('/api/config/file-picker/roots'),
  filePickerList: (rootId: string, path = '', page = 1, pageSize = 100, sort = 'name', order = 'asc') =>
    request<FilePickerListResponse>(`/api/config/file-picker/list${query({ rootId, path, page, pageSize, sort, order })}`),
  validateFilePickerSelection: (rootId: string, path: string, expectedType: 'file' | 'directory') =>
    request<FilePickerSelection>('/api/config/file-picker/validate', {
      method: 'POST',
      body: JSON.stringify({ rootId, path, expectedType }),
    }),
  createResource: (payload: ResourcePayload) =>
    request<DirectoryInfo>('/api/config/resources', { method: 'POST', body: JSON.stringify(payload) }),
  updateResource: (id: string, payload: ResourcePayload) =>
    request<DirectoryInfo>(`/api/config/resources/${encodeURIComponent(id)}`, { method: 'PUT', body: JSON.stringify(payload) }),
  deleteResource: (id: string) =>
    request<{ ok: boolean }>(`/api/config/resources/${encodeURIComponent(id)}`, { method: 'DELETE' }),
  // 公开分享接口不带登录态要求，全部依赖 token 或下载票据授权。
  publicTokenInfo: (token: string) =>
    publicRequest<TokenInfo>(`/t/${encodeURIComponent(token)}/info`),
  publicUpload: (token: string, file: File, options: UploadOptions = {}) => {
    const form = new FormData()
    form.append('files', file)
    const fallbackMultipart = () => uploadForm<{ ok: boolean; uploaded?: number }>(buildTransferUrl(`/t/${encodeURIComponent(token)}/upload`), form, {
      ...options,
      suppressAuthRedirect: true,
      withCredentials: false,
    })
    const payload: PublicUploadLeaseRequest = { fileName: file.name, fileSize: file.size }
    return api.createPublicUploadLease(token, payload)
      .then((lease) => {
        if (lease.rawUploadUrl) {
          return uploadRaw<{ ok: boolean; uploaded?: number }>(lease.rawUploadUrl, lease.lease, file, {
            ...options,
            suppressAuthRedirect: true,
          })
        }
        if (lease.uploadUrl) {
          return uploadForm<{ ok: boolean; uploaded?: number }>(buildTransferUrl(lease.uploadUrl), form, {
            ...options,
            suppressAuthRedirect: true,
            withCredentials: false,
            headers: { ...(options.headers || {}), Authorization: `Bearer ${lease.lease}` },
          })
        }
        return fallbackMultipart()
      })
      .catch((err) => {
        if (err instanceof ApiError && (err.status === 404 || err.status === 405)) return fallbackMultipart()
        throw err
      })
  },
  createPublicUploadLease: (token: string, payload: PublicUploadLeaseRequest) =>
    publicRequest<PublicUploadLeaseResponse>(`/t/${encodeURIComponent(token)}/upload-lease`, {
      method: 'POST',
      body: JSON.stringify(payload),
    }),
  createPublicDownloadLease: (token: string) =>
    publicRequest<DownloadLeaseResponse>(`/t/${encodeURIComponent(token)}/download-lease`, { method: 'POST' }),
}
