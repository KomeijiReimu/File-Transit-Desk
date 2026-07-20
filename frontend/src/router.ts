import { nextTick, watch } from 'vue'
import type { WatchStopHandle } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import type { RouteLocationNormalized, RouteLocationRaw } from 'vue-router'
import { authState, restoreSession } from '@/auth'
import { recordInitialNavigationActivity } from '@/sessionActivity'

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior(to, from, savedPosition) {
    if (savedPosition) return savedPosition
    if (to.path === from.path) return false
    return { top: 0 }
  },
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/LoginView.vue'), meta: { public: true, title: '登录' } },
    {
      path: '/share/:token',
      name: 'share',
      component: () => import('@/views/ShareView.vue'),
      meta: { public: true, title: '临时分享' },
      props: true,
    },
    { path: '/', redirect: '/files' },
    { path: '/files', name: 'files', component: () => import('@/views/FilesView.vue'), meta: { title: '文件浏览' } },
    { path: '/upload', name: 'upload', component: () => import('@/views/UploadView.vue'), meta: { title: '文件上传' } },
    { path: '/chat', name: 'chat', component: () => import('@/views/ChatView.vue'), meta: { title: '在线交流' } },
    { path: '/tokens', name: 'tokens', component: () => import('@/views/TokensView.vue'), meta: { adminOnly: true, title: '令牌管理' } },
    { path: '/transfers', name: 'transfers', component: () => import('@/views/TransfersView.vue'), meta: { adminOnly: true, title: '正在传输' } },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue'), meta: { adminOnly: true, title: '访问记录' } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { adminOnly: true, title: '配置管理' } },
    { path: '/:pathMatch(.*)*', redirect: '/files' },
  ],
})

router.afterEach(async (to, from) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : '文件传输台'
  document.title = title === '文件传输台' ? title : `${title} · 文件传输台`
  if (to.name === from.name) return
  await nextTick()
  window.requestAnimationFrame(() => {
    document.querySelector<HTMLElement>('[data-route-focus]')?.focus({ preventScroll: true })
  })
})

export async function authNavigationGuard(to: RouteLocationNormalized) {
  // 首次进入受保护页面前先恢复会话，避免刷新后短暂闪到登录页。
  if ((to.name === 'login' || !to.meta.public) && (authState.status === 'unknown' || !authState.ready)) {
    recordInitialNavigationActivity(typeof document !== 'undefined' && document.visibilityState === 'visible')
    await restoreSession()
  }

  return authenticationRedirectForRoute(to) || true
}

router.beforeEach(authNavigationGuard)

type AuthenticationRoute = Pick<RouteLocationNormalized, 'name' | 'meta' | 'fullPath' | 'query'>

function safePostLoginRedirect(value: unknown) {
  if (typeof value !== 'string' || !value.startsWith('/') || value.startsWith('//')) return undefined
  if (/^\/login(?:[/?#]|$)/.test(value)) return undefined
  return value
}

export function authenticationRedirectForRoute(to: AuthenticationRoute): RouteLocationRaw | undefined {
  // Unknown is deliberately fail-closed while /me is pending; routing waits
  // for an authenticated, anonymous or unavailable conclusion.
  if (authState.status === 'unknown') return undefined

  // Public shares are independent of Cookie authentication changes.
  if (to.meta.public && to.name !== 'login') return undefined

  if (authState.status === 'unavailable') {
    if (to.name === 'login') {
      const redirect = safePostLoginRedirect(to.query.redirect) || '/files'
      if (to.query.unavailable === '1' && to.query.redirect === redirect) return undefined
      return { name: 'login', query: { ...to.query, unavailable: '1', redirect } }
    }
    return { name: 'login', query: { unavailable: '1', redirect: to.fullPath } }
  }

  if (authState.status === 'anonymous') {
    if (to.name === 'login') return undefined
    return { name: 'login', query: { redirect: to.fullPath } }
  }

  if (authState.status === 'authenticated') {
    if (to.name === 'login') return safePostLoginRedirect(to.query.redirect) || { name: 'files' }
    // 管理页角色在其他标签页降级后，不能等待下一次手动导航。
    if (to.meta.adminOnly && authState.role !== 'admin') return { name: 'files' }
  }
  return undefined
}

let pendingAuthenticationTarget: string | undefined
let authenticationRouteSyncStop: WatchStopHandle | undefined
let authenticationRouteSyncOwners = 0

export async function synchronizeAuthenticationRoute() {
  const current = router.currentRoute.value
  const target = authenticationRedirectForRoute(current)
  if (!target) return
  const targetPath = router.resolve(target).fullPath
  if (targetPath === current.fullPath || targetPath === pendingAuthenticationTarget) return
  pendingAuthenticationTarget = targetPath
  try {
    await router.replace(target)
  } catch {
    // Duplicate/cancelled navigations are expected when auth and route state
    // settle in the same tick; the next state transition will re-evaluate.
  } finally {
    if (pendingAuthenticationTarget === targetPath) pendingAuthenticationTarget = undefined
  }
}

// Installation is deliberately deferred until main.ts has finished evaluating
// the auth -> api -> router module cycle. watch() evaluates its getter during
// setup, so running it at router module scope would read authState in its ESM
// temporal dead zone.
export function installAuthenticationRouteSync() {
  authenticationRouteSyncOwners += 1
  if (!authenticationRouteSyncStop) {
    authenticationRouteSyncStop = watch(
      () => [authState.status, authState.role, authState.ready] as const,
      () => { void synchronizeAuthenticationRoute() },
      { flush: 'post' },
    )
  }

  let released = false
  return () => {
    if (released) return
    released = true
    authenticationRouteSyncOwners = Math.max(0, authenticationRouteSyncOwners - 1)
    if (authenticationRouteSyncOwners !== 0 || !authenticationRouteSyncStop) return
    authenticationRouteSyncStop()
    authenticationRouteSyncStop = undefined
    pendingAuthenticationTarget = undefined
  }
}

export default router
