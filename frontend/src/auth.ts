import { computed, reactive } from 'vue'
import { api } from '@/api'
import type { UserInfo, UserRole } from '@/types'

const ROLE_STORAGE_KEY = 'ft-role-hint'
const NAME_STORAGE_KEY = 'ft-name-hint'

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
  // 本地缓存只在 /me 尚未返回时提供名称提示，不参与任何权限判断。
  name: readNameHint(),
  user: null as UserInfo | null,
})

export const isAdmin = computed(() => authState.authenticated && authState.role === 'admin')

function applyUser(user: UserInfo, fallbackName?: string) {
  authState.user = user
  authState.authenticated = user.authenticated !== false
  // 权限角色只接受当前后端响应；缺失或非法角色按普通用户处理。
  const role: UserRole = user.role === 'admin' || user.role === 'user' ? user.role : 'user'
  authState.role = role
  authState.name = user.name || fallbackName || (role === 'admin' ? '管理员' : '访客')
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
  applyUser(user)
}

export async function adminLogin(username: string, password: string) {
  const user = await api.adminLogin({ username, password })
  if (user.authenticated === false) {
    throw new Error('后端未确认登录状态。')
  }
  applyUser(user, username)
}

export async function logout() {
  try {
    await api.logout()
  } finally {
    clearUser()
  }
}
