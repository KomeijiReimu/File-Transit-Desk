import { computed, reactive } from 'vue'
import { api } from '@/api'
import type { UserInfo, UserRole } from '@/types'

const ROLE_STORAGE_KEY = 'ft-role-hint'
const NAME_STORAGE_KEY = 'ft-name-hint'

function readRoleHint(): UserRole | undefined {
  try {
    const value = localStorage.getItem(ROLE_STORAGE_KEY)
    return value === 'admin' || value === 'user' ? value : undefined
  } catch {
    return undefined
  }
}

function writeRoleHint(role?: UserRole, name?: string) {
  try {
    // 只缓存展示用的角色和名称，不保存任何可用于鉴权的 Cookie 或 token。
    if (role) localStorage.setItem(ROLE_STORAGE_KEY, role)
    else localStorage.removeItem(ROLE_STORAGE_KEY)
    if (name) localStorage.setItem(NAME_STORAGE_KEY, name)
    else localStorage.removeItem(NAME_STORAGE_KEY)
  } catch {
    // ignore storage errors (e.g. private mode)
  }
}

function readNameHint(): string | undefined {
  try {
    return localStorage.getItem(NAME_STORAGE_KEY) || undefined
  } catch {
    return undefined
  }
}

export const authState = reactive({
  ready: false,
  authenticated: false,
  role: undefined as UserRole | undefined,
  name: undefined as string | undefined,
  user: null as UserInfo | null,
})

export const isAdmin = computed(() => authState.role === 'admin')

function applyUser(user: UserInfo, fallbackRole?: UserRole, fallbackName?: string) {
  authState.user = user
  authState.authenticated = user.authenticated !== false
  // 后端 role 优先，其次使用本次登录入口传入的兜底值，最后才用本地展示缓存。
  const role = (user.role as UserRole | undefined) || fallbackRole || readRoleHint() || 'user'
  authState.role = role
  authState.name = user.name || fallbackName || readNameHint() || (role === 'admin' ? '管理员' : '访客')
  if (authState.authenticated) writeRoleHint(role, authState.name)
}

export function clearUser() {
	authState.user = null
	authState.authenticated = false
	authState.role = undefined
	authState.name = undefined
	writeRoleHint(undefined)
}

if (typeof window !== 'undefined') {
	window.addEventListener('ft:auth-expired', () => clearUser())
}

export async function restoreSession() {
  try {
    // 刷新页面后用 /me 恢复真实登录态；失败时清本地状态即可，Cookie 由后端过期策略决定。
    const user = await api.me()
    if (user.authenticated === false) {
      clearUser()
    } else {
      applyUser(user)
    }
  } catch {
    clearUser()
  } finally {
    authState.ready = true
  }
}

export async function login(totp: string) {
  const user = await api.login(totp)
  if (user.authenticated === false) {
    throw new Error('后端未确认登录状态。')
  }
  applyUser(user, 'user')
}

export async function adminLogin(username: string, password: string) {
  const user = await api.adminLogin({ username, password })
  if (user.authenticated === false) {
    throw new Error('后端未确认登录状态。')
  }
  applyUser(user, 'admin', username)
}

export async function logout() {
  try {
    await api.logout()
  } finally {
    clearUser()
  }
}
