import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMocks = vi.hoisted(() => {
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

const authSyncMocks = vi.hoisted(() => {
  const state = {} as { listener?: (event: { id: string; reason: string }) => void }
  return {
    state,
    publish: vi.fn(),
    subscribe: vi.fn((listener: (event: { id: string; reason: string }) => void) => {
      state.listener = listener
      return () => undefined
    }),
  }
})

vi.mock('@/api', () => ({
  ApiError: apiMocks.ApiError,
  api: {
    me: apiMocks.me,
    login: apiMocks.login,
    adminLogin: apiMocks.adminLogin,
    logout: apiMocks.logout,
  },
}))

vi.mock('@/authSync', () => ({
  publishAuthSync: authSyncMocks.publish,
  subscribeAuthSync: authSyncMocks.subscribe,
}))

import { ApiError } from '@/api'
import { adminLogin, authState, login, logout, restoreSession } from '@/auth'
import { authenticationEpoch } from '@/authEpoch'

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function setKnownSession() {
  authState.ready = false
  authState.status = 'authenticated'
  authState.authenticated = true
  authState.role = 'admin'
  authState.name = 'known-admin'
  authState.user = {
    authenticated: true,
    role: 'admin',
    name: 'known-admin',
    sessionBinding: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
  }
}

function setColdStart() {
  authState.ready = false
  authState.status = 'unknown'
  authState.authenticated = false
  authState.role = undefined
  authState.name = undefined
  authState.user = null
}

describe('restoreSession', () => {
  beforeEach(() => {
    apiMocks.me.mockReset()
    apiMocks.login.mockReset()
    apiMocks.adminLogin.mockReset()
    apiMocks.logout.mockReset()
    authSyncMocks.publish.mockReset()
    setKnownSession()
  })

  it.each([
    ['network error', 0],
    ['server error', 503],
  ])('preserves the known session on %s and returns the error', async (_label, status) => {
    const error = new ApiError('temporary failure', status)
    apiMocks.me.mockRejectedValueOnce(error)

    await expect(restoreSession()).resolves.toEqual({ restored: false, status: 'authenticated', error })

    expect(authState.ready).toBe(true)
    expect(authState.authenticated).toBe(true)
    expect(authState.role).toBe('admin')
    expect(authState.name).toBe('known-admin')
    expect(authState.user).toMatchObject({ authenticated: true, role: 'admin' })
  })

  it.each([
    ['network error', 0],
    ['server error', 503],
  ])('marks a cold start unavailable on %s instead of anonymous', async (_label, status) => {
    setColdStart()
    const error = new ApiError('temporary failure', status)
    apiMocks.me.mockRejectedValueOnce(error)

    await expect(restoreSession()).resolves.toEqual({ restored: false, status: 'unavailable', error })

    expect(authState.ready).toBe(true)
    expect(authState.status).toBe('unavailable')
    expect(authState.authenticated).toBe(false)
  })

  it('retries an unavailable cold start without requiring credentials', async () => {
    setColdStart()
    apiMocks.me
      .mockRejectedValueOnce(new ApiError('offline', 0))
      .mockResolvedValueOnce({ authenticated: true, role: 'user', name: 'restored' })

    await restoreSession()
    expect(authState.status).toBe('unavailable')
    await expect(restoreSession()).resolves.toMatchObject({ restored: true, status: 'authenticated' })

    expect(authState.status).toBe('authenticated')
    expect(authState.authenticated).toBe(true)
    expect(authState.name).toBe('restored')
  })

  it('does not let a stale restore failure clear a newly authenticated administrator', async () => {
    const pending = deferred<{ authenticated: boolean; role: string; name: string }>()
    apiMocks.me.mockReturnValueOnce(pending.promise)
    apiMocks.adminLogin.mockResolvedValueOnce({ authenticated: true, role: 'admin', name: 'root' })

    const restoring = restoreSession()
    await adminLogin('root', 'secret')
    pending.reject(new ApiError('old session unauthorized', 401))

    await expect(restoring).resolves.toMatchObject({ restored: false, stale: true })
    expect(authState.status).toBe('authenticated')
    expect(authState.role).toBe('admin')
    expect(authState.name).toBe('root')
  })

  it('clears the known session only for an explicit 401', async () => {
    const error = new ApiError('unauthorized', 401)
    apiMocks.me.mockRejectedValueOnce(error)

    await expect(restoreSession()).resolves.toEqual({ restored: false, status: 'anonymous', error })

    expect(authState.ready).toBe(true)
    expect(authState.status).toBe('anonymous')
    expect(authState.authenticated).toBe(false)
    expect(authState.role).toBeUndefined()
    expect(authState.name).toBeUndefined()
    expect(authState.user).toBeNull()
  })

  it('applies a successful restored session', async () => {
    apiMocks.me.mockResolvedValueOnce({ authenticated: true, role: 'user', name: 'restored', sessionBinding: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' })

    await expect(restoreSession()).resolves.toMatchObject({ restored: true })

    expect(authState.ready).toBe(true)
    expect(authState.status).toBe('authenticated')
    expect(authState.authenticated).toBe(true)
    expect(authState.role).toBe('user')
    expect(authState.name).toBe('restored')
  })

  it('clears an administrator synchronously on an external event, invalidates a pending restore, then adopts the revalidated subject', async () => {
    const pending = deferred<{ authenticated: boolean; role: string; name: string; sessionBinding: string }>()
    const replacement = {
      authenticated: true,
      role: 'user',
      name: 'replacement',
      sessionBinding: 'cccccccccccccccccccccccccccccccc',
    }
    apiMocks.me.mockReturnValueOnce(pending.promise).mockResolvedValueOnce(replacement)
    const startingEpoch = authenticationEpoch()

    const oldRestore = restoreSession()
    authSyncMocks.state.listener?.({ id: 'external-event-1', reason: 'login' })

    expect(authenticationEpoch()).toBeGreaterThan(startingEpoch)
    expect(authState.ready).toBe(false)
    expect(authState.status).toBe('unknown')
    expect(authState.authenticated).toBe(false)
    expect(authState.role).toBeUndefined()
    expect(authState.user).toBeNull()

    pending.resolve({
      authenticated: true,
      role: 'admin',
      name: 'stale-admin',
      sessionBinding: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
    })
    await expect(oldRestore).resolves.toMatchObject({ restored: false, stale: true })
    await vi.waitFor(() => {
      expect(apiMocks.me).toHaveBeenCalledTimes(2)
      expect(authState.status).toBe('authenticated')
      expect(authState.role).toBe('user')
      expect(authState.user?.sessionBinding).toBe(replacement.sessionBinding)
    })
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })

  it('finishes an external logout as anonymous after clearing synchronously before /me settles', async () => {
    const pending = deferred<never>()
    apiMocks.me.mockReturnValueOnce(pending.promise)

    authSyncMocks.state.listener?.({ id: 'external-logout-1', reason: 'logout' })
    expect(authState.status).toBe('unknown')
    expect(authState.ready).toBe(false)
    expect(authState.role).toBeUndefined()
    expect(authState.user).toBeNull()

    pending.reject(new ApiError('unauthorized', 401))
    await vi.waitFor(() => {
      expect(authState.status).toBe('anonymous')
      expect(authState.ready).toBe(true)
      expect(apiMocks.me).toHaveBeenCalledTimes(1)
    })
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })

  it('runs a second /me when another auth event arrives during the first revalidation', async () => {
    const first = deferred<{ authenticated: boolean; role: string; name: string; sessionBinding: string }>()
    const latest = {
      authenticated: true,
      role: 'user',
      name: 'latest-user',
      sessionBinding: '56565656565656565656565656565656',
    }
    apiMocks.me.mockReturnValueOnce(first.promise).mockResolvedValueOnce(latest)

    authSyncMocks.state.listener?.({ id: 'overlap-event-1', reason: 'subject_changed' })
    expect(apiMocks.me).toHaveBeenCalledTimes(1)
    authSyncMocks.state.listener?.({ id: 'overlap-event-2', reason: 'logout' })
    expect(authState.status).toBe('unknown')

    first.resolve({
      authenticated: true,
      role: 'admin',
      name: 'stale-admin',
      sessionBinding: '78787878787878787878787878787878',
    })
    await vi.waitFor(() => {
      expect(apiMocks.me).toHaveBeenCalledTimes(2)
      expect(authState.status).toBe('authenticated')
      expect(authState.role).toBe('user')
      expect(authState.name).toBe('latest-user')
    })
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })

  it('does not lose logout immediately after a completed subject revalidation', async () => {
    apiMocks.me
      .mockResolvedValueOnce({
        authenticated: true,
        role: 'user',
        name: 'first-user',
        sessionBinding: '90909090909090909090909090909090',
      })
      .mockRejectedValueOnce(new ApiError('unauthorized', 401))

    authSyncMocks.state.listener?.({ id: 'sequential-event-1', reason: 'subject_changed' })
    await vi.waitFor(() => expect(authState.name).toBe('first-user'))
    authSyncMocks.state.listener?.({ id: 'sequential-event-2', reason: 'logout' })

    expect(authState.status).toBe('unknown')
    await vi.waitFor(() => {
      expect(apiMocks.me).toHaveBeenCalledTimes(2)
      expect(authState.status).toBe('anonymous')
    })
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })

  it('broadcasts local login and logout but never echoes an externally received event', async () => {
    apiMocks.login.mockResolvedValueOnce({
      authenticated: true,
      role: 'user',
      name: 'local-user',
      sessionBinding: 'dddddddddddddddddddddddddddddddd',
    })
    await login('000000')
    expect(authSyncMocks.publish).toHaveBeenCalledWith('login')

    apiMocks.logout.mockResolvedValueOnce({ ok: true })
    await logout()
    expect(authSyncMocks.publish).toHaveBeenCalledWith('logout')

    authSyncMocks.publish.mockClear()
    apiMocks.me.mockResolvedValueOnce({
      authenticated: true,
      role: 'admin',
      name: 'external-admin',
      sessionBinding: 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee',
    })
    authSyncMocks.state.listener?.({ id: 'external-event-2', reason: 'login' })
    await vi.waitFor(() => expect(authState.role).toBe('admin'))
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })

  it.each([
    ['network error', 0],
    ['server error', 503],
  ])('marks an externally invalidated session unavailable on revalidation %s', async (_label, status) => {
    apiMocks.me.mockRejectedValueOnce(new ApiError('temporary failure', status))
    authSyncMocks.state.listener?.({ id: `external-failure-${status}`, reason: 'subject_changed' })

    expect(authState.status).toBe('unknown')
    expect(authState.role).toBeUndefined()
    await vi.waitFor(() => expect(authState.status).toBe('unavailable'))
    expect(authState.authenticated).toBe(false)
    expect(authState.status).not.toBe('anonymous')
    expect(authSyncMocks.publish).not.toHaveBeenCalled()
  })
})
