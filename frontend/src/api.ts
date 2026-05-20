import router from '@/router'
import type {
  AdminLoginPayload,
  AuditLog,
  CreateTokenPayload,
  CreateTokenResponse,
  DirectoryInfo,
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

type ApiRequestInit = RequestInit & { suppressAuthRedirect?: boolean }

async function parseResponse<T>(response: Response, suppressAuthRedirect = false): Promise<T> {
  const contentType = response.headers.get('content-type') || ''
  const isJson = contentType.includes('application/json')
  const payload = isJson ? await response.json().catch(() => null) : await response.text().catch(() => '')

  if (!response.ok) {
    const message =
      (payload && typeof payload === 'object' && ('message' in payload || 'error' in payload)
        ? String((payload as { message?: string; error?: string }).message || (payload as { error?: string }).error)
        : '') || `请求失败（${response.status}）`
    if (!suppressAuthRedirect && response.status === 401 && router.currentRoute.value.meta?.public !== true && router.currentRoute.value.name !== 'login') {
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
    const form = new FormData()
    form.set('dirId', dirId)
    form.set('path', path)
    form.append('files', file)
    return request<{ ok: boolean; uploaded?: number }>('/api/files/upload', { method: 'POST', body: form })
  },
  tokens: () => request<TokenInfo[]>('/api/tokens'),
  createToken: (payload: CreateTokenPayload) =>
    request<CreateTokenResponse>('/api/tokens', { method: 'POST', body: JSON.stringify(payload) }),
  revokeToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}/revoke`, { method: 'POST' }),
  deleteToken: (id: string | number) =>
    request<{ ok: boolean }>(`/api/tokens/${encodeURIComponent(String(id))}`, { method: 'DELETE' }),
  auditLogs: (filter: AuditFilter = {}) =>
    request<AuditLog[]>(`/api/audit/logs${query({ limit: filter.limit, action: filter.action, status: filter.status })}`),
  // Public share endpoints (no auth)
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
}

export const downloadUrl = (dirId: string, path: string) => `/api/files/download${query({ dirId, path })}`
export const publicDownloadUrl = (token: string) => `/t/${encodeURIComponent(token)}/download`
