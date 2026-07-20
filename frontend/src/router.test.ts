import type { RouteLocationNormalized } from 'vue-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'

const routerApiMocks = vi.hoisted(() => {
  class MockApiError extends Error {
    status: number

    constructor(message: string, status: number) {
      super(message)
      this.status = status
    }
  }

  return {
    ApiError: MockApiError,
    me: vi.fn(),
    login: vi.fn(),
    adminLogin: vi.fn(),
    logout: vi.fn(),
  }
})

vi.mock('@/api', () => ({
  ApiError: routerApiMocks.ApiError,
  api: {
    me: routerApiMocks.me,
    login: routerApiMocks.login,
    adminLogin: routerApiMocks.adminLogin,
    logout: routerApiMocks.logout,
  },
}))

import { ApiError } from '@/api'
import { authState, restoreSession } from '@/auth'
import router, { authNavigationGuard, installAuthenticationRouteSync } from '@/router'
import { acceptExternalAuthSyncEvent } from '@/authSync'
import { recentSessionActivity, resetSessionActivity } from '@/sessionActivity'

function target(overrides: Partial<RouteLocationNormalized> = {}) {
  return {
    name: 'files',
    meta: {},
    fullPath: '/files?path=docs',
    query: { path: 'docs' },
    ...overrides,
  } as RouteLocationNormalized
}

function resetColdStart() {
  authState.ready = false
  authState.status = 'unknown'
  authState.authenticated = false
  authState.role = undefined
  authState.name = undefined
  authState.user = null
}

function setAuthenticated(role: 'user' | 'admin' = 'user') {
  authState.ready = true
  authState.status = 'authenticated'
  authState.authenticated = true
  authState.role = role
  authState.name = role
  authState.user = {
    authenticated: true,
    role,
    name: role,
    sessionBinding: role === 'admin'
      ? 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
      : 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
  }
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe('router authentication guard', () => {
  let stopAuthenticationRouteSync: (() => void) | undefined

  beforeEach(async () => {
    routerApiMocks.me.mockReset()
    resetSessionActivity()
    resetColdStart()
    await router.replace('/share/router-test-reset')
    stopAuthenticationRouteSync = installAuthenticationRouteSync()
  })

  afterEach(() => stopAuthenticationRouteSync?.())

  it.each([
    ['network error', 0],
    ['server error', 503],
  ])('routes a cold-start %s through unavailable semantics and allows retrying the original target', async (_label, status) => {
    routerApiMocks.me.mockRejectedValueOnce(new ApiError('temporary failure', status))

    await expect(authNavigationGuard(target())).resolves.toEqual({
      name: 'login',
      query: { unavailable: '1', redirect: '/files?path=docs' },
    })
    expect(authState.status).toBe('unavailable')

    routerApiMocks.me.mockResolvedValueOnce({ authenticated: true, role: 'user', name: 'restored' })
    await expect(restoreSession()).resolves.toMatchObject({ restored: true, status: 'authenticated' })
    await expect(authNavigationGuard(target())).resolves.toBe(true)
  })

  it('uses normal anonymous semantics only for an explicit 401', async () => {
    routerApiMocks.me.mockRejectedValueOnce(new ApiError('unauthorized', 401))

    await expect(authNavigationGuard(target())).resolves.toEqual({
      name: 'login',
      query: { redirect: '/files?path=docs' },
    })
    expect(authState.status).toBe('anonymous')
    expect(authState.authenticated).toBe(false)
  })

  it.each([
    ['visible', true],
    ['hidden', false],
  ] as const)('grants initial navigation activity only when the cold page is %s', async (visibility, expected) => {
    vi.spyOn(document, 'visibilityState', 'get').mockReturnValue(visibility)
    routerApiMocks.me.mockRejectedValueOnce(new ApiError('unauthorized', 401))

    await authNavigationGuard(target())

    expect(Boolean(recentSessionActivity())).toBe(expected)
  })

  it('registers chat as a protected destination for both user roles', () => {
    const route = router.getRoutes().find((entry) => entry.name === 'chat')
    expect(route?.path).toBe('/chat')
    expect(route?.meta.public).not.toBe(true)
    expect(route?.meta.adminOnly).not.toBe(true)
  })

  it('keeps a protected route fail-closed while external logout revalidates, then redirects anonymous to login', async () => {
    setAuthenticated('user')
    await router.replace('/chat')
    const pending = deferred<never>()
    routerApiMocks.me.mockReturnValueOnce(pending.promise)

    expect(acceptExternalAuthSyncEvent({ id: 'router-external-logout', reason: 'logout' })).toBe(true)
    expect(authState.status).toBe('unknown')
    expect(authState.user).toBeNull()
    expect(router.currentRoute.value.fullPath).toBe('/chat')

    pending.reject(new ApiError('unauthorized', 401))
    await vi.waitFor(() => {
      expect(authState.status).toBe('anonymous')
      expect(router.currentRoute.value.name).toBe('login')
      expect(router.currentRoute.value.query.redirect).toBe('/chat')
    })
  })

  it.each([
    ['/login?redirect=/chat', '/chat'],
    ['/login', '/files'],
  ])('leaves login for the restored external subject using %s', async (loginPath, expected) => {
    authState.ready = true
    authState.status = 'anonymous'
    await router.replace(loginPath)
    routerApiMocks.me.mockResolvedValueOnce({
      authenticated: true,
      role: 'user',
      name: 'external-user',
      sessionBinding: 'cccccccccccccccccccccccccccccccc',
    })

    expect(acceptExternalAuthSyncEvent({ id: `router-external-login-${expected}`, reason: 'login' })).toBe(true)
    await vi.waitFor(() => expect(router.currentRoute.value.fullPath).toBe(expected))
    expect(authState.status).toBe('authenticated')
  })

  it('redirects an admin-only route immediately when external revalidation downgrades the role', async () => {
    setAuthenticated('admin')
    await router.replace('/config')
    routerApiMocks.me.mockResolvedValueOnce({
      authenticated: true,
      role: 'user',
      name: 'ordinary-user',
      sessionBinding: 'dddddddddddddddddddddddddddddddd',
    })

    expect(acceptExternalAuthSyncEvent({ id: 'router-role-downgrade', reason: 'subject_changed' })).toBe(true)
    await vi.waitFor(() => expect(router.currentRoute.value.fullPath).toBe('/files'))
    expect(authState.role).toBe('user')
  })

  it('routes failed external revalidation to unavailable login state', async () => {
    setAuthenticated('user')
    await router.replace('/chat')
    routerApiMocks.me.mockRejectedValueOnce(new ApiError('offline', 503))

    expect(acceptExternalAuthSyncEvent({ id: 'router-external-unavailable', reason: 'subject_changed' })).toBe(true)
    await vi.waitFor(() => {
      expect(authState.status).toBe('unavailable')
      expect(router.currentRoute.value.name).toBe('login')
      expect(router.currentRoute.value.query.unavailable).toBe('1')
      expect(router.currentRoute.value.query.redirect).toBe('/chat')
    })
  })

  it('never moves a public share because of auth revalidation', async () => {
    setAuthenticated('admin')
    await router.replace('/share/public-token')
    routerApiMocks.me.mockRejectedValueOnce(new ApiError('unauthorized', 401))

    expect(acceptExternalAuthSyncEvent({ id: 'router-public-share-logout', reason: 'logout' })).toBe(true)
    await vi.waitFor(() => expect(authState.status).toBe('anonymous'))
    await nextTick()
    expect(router.currentRoute.value.fullPath).toBe('/share/public-token')
  })

  it('keeps repeated route-sync installation single-watcher and cleanup-safe', async () => {
    setAuthenticated('user')
    await router.replace('/chat')
    const replace = vi.spyOn(router, 'replace')
    const releaseSecondOwner = installAuthenticationRouteSync()
    try {
      authState.user = null
      authState.authenticated = false
      authState.role = undefined
      authState.status = 'anonymous'
      authState.ready = true

      await vi.waitFor(() => expect(router.currentRoute.value.name).toBe('login'))
      expect(replace).toHaveBeenCalledTimes(1)
    } finally {
      releaseSecondOwner()
    }
  })
})
