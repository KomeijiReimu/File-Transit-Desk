import { computed, reactive, watch } from 'vue'
import { ApiError, api } from '@/api'
import {
  advanceAuthenticationEpoch,
  AUTH_EXPIRED_EVENT,
  authenticationEpoch,
  isInternallyHandledAuthExpiry,
  registerAuthenticationClear,
  registerSessionSubjectInvalidation,
  setCurrentSessionBinding,
} from '@/authEpoch'
import { invalidateSessionRecovery } from '@/authRecovery'
import { publishAuthSync, subscribeAuthSync } from '@/authSync'
import type { UserInfo, UserRole } from '@/types'

export { authenticationEpoch, expireSessionOnce } from '@/authEpoch'

const ROLE_STORAGE_KEY = 'ft-role-hint'
const NAME_STORAGE_KEY = 'ft-name-hint'

export type AuthenticationStatus = 'unknown' | 'authenticated' | 'anonymous' | 'unavailable'

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
  status: 'unknown' as AuthenticationStatus,
  authenticated: false,
  role: undefined as UserRole | undefined,
  // 本地缓存只在 /me 尚未返回时提供名称提示，不参与任何权限判断。
  name: readNameHint(),
  user: null as UserInfo | null,
})

export const isAdmin = computed(() => authState.authenticated && authState.role === 'admin')

watch(
  () => authState.user?.sessionBinding,
  (binding) => setCurrentSessionBinding(binding),
  { immediate: true, flush: 'sync' },
)

function applyUser(user: UserInfo, fallbackName?: string) {
  // /me、普通登录和管理员登录都可能替换主体；旧请求不得在新主体下重放。
  advanceAuthenticationEpoch()
  invalidateSessionRecovery()
  authState.user = user
  authState.authenticated = user.authenticated !== false
  authState.status = authState.authenticated ? 'authenticated' : 'anonymous'
  authState.ready = true
  // 权限角色只接受当前后端响应；缺失或非法角色按普通用户处理。
  const role: UserRole = user.role === 'admin' || user.role === 'user' ? user.role : 'user'
  authState.role = role
  authState.name = user.name || fallbackName || (role === 'admin' ? '管理员' : '访客')
  if (authState.authenticated) writeRoleHint(role, authState.name)
}

export function clearUser(mode: 'anonymous' | 'unknown' = 'anonymous') {
  advanceAuthenticationEpoch()
  invalidateSessionRecovery()
  setCurrentSessionBinding(undefined)
  authState.user = null
  authState.authenticated = false
  authState.status = mode
  authState.ready = mode !== 'unknown'
  authState.role = undefined
  authState.name = undefined
  writeRoleHint(undefined)
}

registerAuthenticationClear(clearUser)

if (typeof window !== 'undefined') {
  window.addEventListener(AUTH_EXPIRED_EVENT, (event) => {
    // expireSessionOnce 已同步清理；普通 Event 仍兼容现有上传完成流程。
    if (!isInternallyHandledAuthExpiry(event)) clearUser()
    publishAuthSync('expired')
  })
}

type RestoreSessionResult =
  | { restored: true; status: AuthenticationStatus; user: UserInfo }
  | { restored: false; status: AuthenticationStatus; error?: unknown; stale?: true }

let restoreRunning: Promise<RestoreSessionResult> | undefined
let subjectRevalidationRequested = 0
let subjectRevalidationCompleted = 0
let subjectRevalidationRunning: Promise<void> | undefined

async function restoreSessionOnce(broadcastAuthFailure: boolean): Promise<RestoreSessionResult> {
  const restoreEpoch = authenticationEpoch()
  try {
    // 刷新页面后用 /me 恢复真实登录态；Cookie 由后端过期策略决定。
    const user = await api.me()
    if (authenticationEpoch() !== restoreEpoch) {
      return { restored: false as const, status: authState.status, stale: true as const }
    }
    if (user.authenticated === false) {
      clearUser()
      if (broadcastAuthFailure) publishAuthSync('expired')
      return { restored: false as const, status: authState.status }
    } else {
      applyUser(user)
      return { restored: true as const, status: authState.status, user }
    }
  } catch (error) {
    if (authenticationEpoch() !== restoreEpoch) {
      return { restored: false as const, status: authState.status, error, stale: true as const }
    }
    if (error instanceof ApiError && error.status === 401) {
      clearUser()
      if (broadcastAuthFailure) publishAuthSync('expired')
    } else if (authState.status !== 'authenticated') {
      // 冷启动无法确认 Cookie 时保留独立 unavailable 语义，不能伪装成匿名登出。
      authState.status = 'unavailable'
      authState.authenticated = false
      authState.user = null
      authState.role = undefined
    }
    return { restored: false as const, status: authState.status, error }
  } finally {
    if (authenticationEpoch() === restoreEpoch) authState.ready = true
  }
}

function restoreSessionWithPolicy(broadcastAuthFailure: boolean) {
  if (restoreRunning) return restoreRunning
  const pending = restoreSessionOnce(broadcastAuthFailure)
  restoreRunning = pending
  const release = () => {
    if (restoreRunning === pending) restoreRunning = undefined
  }
  void pending.then(release, release)
  return pending
}

export function restoreSession() {
  return restoreSessionWithPolicy(true)
}

function releaseCompletedRestore(pending: Promise<RestoreSessionResult>) {
  if (restoreRunning === pending) restoreRunning = undefined
}

async function runSubjectRevalidationLoop() {
  while (subjectRevalidationCompleted < subjectRevalidationRequested) {
    const existing = restoreRunning
    if (existing) {
      try {
        await existing
      } catch {
        if (authState.status === 'unknown') {
          authState.status = 'unavailable'
          authState.ready = true
        }
      } finally {
        releaseCompletedRestore(existing)
      }
    }

    // Events received while an older restore was pending can be coalesced into
    // one probe for the newest Cookie. Events received during this probe raise
    // requested again and force another loop iteration.
    const generation = subjectRevalidationRequested
    let revalidation: Promise<RestoreSessionResult> | undefined
    try {
      revalidation = restoreSessionWithPolicy(false)
      await revalidation
    } catch {
      // restoreSessionOnce normally converts failures to a result. Keep this
      // fail-safe so an unexpected exception cannot leave auth stuck unknown.
      if (authState.status === 'unknown') {
        authState.status = 'unavailable'
        authState.ready = true
      }
    } finally {
      if (revalidation) releaseCompletedRestore(revalidation)
      subjectRevalidationCompleted = Math.max(subjectRevalidationCompleted, generation)
    }
  }
}

function ensureSubjectRevalidationLoop() {
  if (subjectRevalidationRunning) return
  const running = runSubjectRevalidationLoop()
  subjectRevalidationRunning = running
  const release = () => {
    if (subjectRevalidationRunning === running) subjectRevalidationRunning = undefined
    if (subjectRevalidationCompleted < subjectRevalidationRequested) ensureSubjectRevalidationLoop()
  }
  void running.then(release, release)
}

function queueSubjectRevalidation() {
  subjectRevalidationRequested += 1
  ensureSubjectRevalidationLoop()
}

function handleSubjectInvalidation(broadcast: boolean) {
  // Clearing is synchronous: role-gated views can remove sensitive DOM before
  // any /me network result arrives.
  clearUser('unknown')
  if (broadcast) publishAuthSync('subject_changed')
  queueSubjectRevalidation()
}

registerSessionSubjectInvalidation(() => handleSubjectInvalidation(true))
subscribeAuthSync(() => handleSubjectInvalidation(false))

export async function login(totp: string) {
  const loginEpoch = advanceAuthenticationEpoch()
  invalidateSessionRecovery()
  const user = await api.login(totp)
  if (authenticationEpoch() !== loginEpoch) throw new Error('认证状态已变更，请重试。')
  if (user.authenticated === false) {
    clearUser()
    throw new Error('后端未确认登录状态。')
  }
  applyUser(user)
  publishAuthSync('login')
}

export async function adminLogin(username: string, password: string) {
  const loginEpoch = advanceAuthenticationEpoch()
  invalidateSessionRecovery()
  const user = await api.adminLogin({ username, password })
  if (authenticationEpoch() !== loginEpoch) throw new Error('认证状态已变更，请重试。')
  if (user.authenticated === false) {
    clearUser()
    throw new Error('后端未确认登录状态。')
  }
  applyUser(user, username)
  publishAuthSync('login')
}

export async function logout() {
  const logoutEpoch = advanceAuthenticationEpoch()
  invalidateSessionRecovery()
  try {
    await api.logout()
  } finally {
    if (authenticationEpoch() === logoutEpoch) {
      clearUser()
      publishAuthSync('logout')
    }
  }
}
