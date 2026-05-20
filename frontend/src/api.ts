import router from '@/router'
import type {
  AdminLoginPayload,
  AuditLog,
  CreateTokenPayload,
  CreateTokenResponse,
  DirectoryInfo,
  DownloadLeaseResponse,
  ListFilesResponse,
  TokenInfo,
  UserInfo,
} from '@/types'

export class ApiError extends Error {
  status: number
  details?: unknown

  constructor(message: string, status: number, details?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.details = details
  }
}

// suppressAuthRedirect 用于心跳、会话恢复和公开页，避免后台探测接口把用户强制跳走。
type ApiRequestInit = RequestInit & { suppressAuthRedirect?: boolean }

async function parseResponse<T>(response: Response, suppressAuthRedirect = false): Promise<T> {
  const contentType = response.headers.get('content-type') || ''
  const isJson = contentType.includes('application/json')
  const payload = isJson ? await response.json().catch(() => null) : await response.text().catch(() => '')

  if (!response.ok) {
    // 后端统一返回 {error}；兼容 {message} 是为了方便未来接入其他服务端错误格式。
    const message =
      (payload && typeof payload === 'object' && ('message' in payload || 'error' in payload)
        ? String((payload as { message?: string; error?: string }).message || (payload as { error?: string }).error)
        : '') || `请求失败（${response.status}）`
    if (!suppressAuthRedirect && response.status === 401 && router.currentRoute.value.meta?.public !== true && router.currentRoute.value.name !== 'login') {
      // 统一广播会话过期，auth.ts 负责清理本地状态，路由负责回到登录页。
      window.dispatchEvent(new Event('ft:auth-expired'))
      router.replace({ name: 'login', query: { redirect: router.currentRoute.value.fullPath } })
    }
    throw new ApiError(message, response.status, payload)
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
    throw new ApiError('无法连接服务器，请检查后端服务或网络连接。', 0, err)
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
  action?: string
  status?: string
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
  listFiles: (dirId: string, path = '') => request<ListFilesResponse>(`/api/files/list${query({ dirId, path })}`),
  upload: (dirId: string, path: string, files: FileList | File[]) => {
    const form = new FormData()
    form.set('dirId', dirId)
    form.set('path', path)
    Array.from(files).forEach((file) => form.append('files', file))
    return request<{ ok: boolean; uploaded?: number }>('/api/files/upload', { method: 'POST', body: form })
  },
  uploadOne: (dirId: string, path: string, file: File) => {
    // 单文件上传用于队列逐项重试，失败时不会影响队列中其他文件的状态。
    const form = new FormData()
    form.set('dirId', dirId)
    form.set('path', path)
    form.append('files', file)
    return request<{ ok: boolean; uploaded?: number }>('/api/files/upload', { method: 'POST', body: form })
  },
  createDownloadLease: (dirId: string, path: string) =>
    request<DownloadLeaseResponse>('/api/files/download-lease', {
      method: 'POST',
      body: JSON.stringify({ dirId, path }),
    }),
  tokens: () => request<TokenInfo[]>('/api/tokens'),
  createToken: (payload: CreateTokenPayload) =>
    request<CreateTokenResponse>('/api/tokens', { method: 'POST', body: JSON.stringify(payload) }),
  revokeToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}/revoke`, { method: 'POST' }),
  deleteToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}`, { method: 'DELETE' }),
  auditLogs: (filter: AuditFilter = {}) =>
    request<AuditLog[]>(`/api/audit/logs${query({ limit: filter.limit, action: filter.action, status: filter.status })}`),
  // 公开分享接口不带登录态要求，全部依赖 token 或下载票据授权。
  publicTokenInfo: (token: string) =>
    request<TokenInfo>(`/t/${encodeURIComponent(token)}/info`),
  publicUpload: (token: string, file: File) => {
    const form = new FormData()
    form.append('files', file)
    return request<{ ok: boolean; uploaded?: number }>(`/t/${encodeURIComponent(token)}/upload`, {
      method: 'POST',
      body: form,
    })
  },
  createPublicDownloadLease: (token: string) =>
    request<DownloadLeaseResponse>(`/t/${encodeURIComponent(token)}/download-lease`, { method: 'POST' }),
}

export const downloadUrl = (dirId: string, path: string) => `/api/files/download${query({ dirId, path })}`
export const publicDownloadUrl = (token: string) => `/t/${encodeURIComponent(token)}/download`
