import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const routerMocks = vi.hoisted(() => ({
  replace: vi.fn(),
  route: { meta: {} as Record<string, unknown>, name: 'files', fullPath: '/files' },
}))

vi.mock('@/router', () => ({
  default: {
    currentRoute: { value: routerMocks.route },
    replace: routerMocks.replace,
  },
}))

import { ApiError, api, parseErrorPayload } from '@/api'
import { adminLogin, authState, logout, restoreSession } from '@/auth'
import { advanceAuthenticationEpoch, authenticationEpoch } from '@/authEpoch'
import { acceptExternalAuthSyncEvent } from '@/authSync'
import {
  recordInitialNavigationActivity,
  recordSessionActivity,
  resetSessionActivity,
} from '@/sessionActivity'

type XHREventHandler = ((event?: Event) => void) | null

class MockXMLHttpRequest {
  static instances: MockXMLHttpRequest[] = []
  static throwOnSend: unknown

  method = ''
  url = ''
  requestHeaders: Record<string, string> = {}
  responseHeaders: Record<string, string> = {}
  responseText = ''
  status = 0
  timeout = -1
  withCredentials = false
  body?: Document | XMLHttpRequestBodyInit | null
  upload = { onprogress: null as ((event: ProgressEvent) => void) | null }
  onerror: XHREventHandler = null
  onabort: XHREventHandler = null
  onload: XHREventHandler = null

  constructor() {
    MockXMLHttpRequest.instances.push(this)
  }

  open(method: string, url: string) {
    this.method = method
    this.url = url
  }

  setRequestHeader(name: string, value: string) {
    this.requestHeaders[name] = value
  }

  getResponseHeader(name: string) {
    return this.responseHeaders[name.toLowerCase()] ?? null
  }

  send(body?: Document | XMLHttpRequestBodyInit | null) {
    this.body = body
    if (MockXMLHttpRequest.throwOnSend) throw MockXMLHttpRequest.throwOnSend
  }

  abort() {
    this.onabort?.(new Event('abort'))
  }

  respond(status: number, body: unknown, headers: Record<string, string> = { 'content-type': 'application/json' }) {
    this.status = status
    this.responseText = typeof body === 'string' ? body : JSON.stringify(body)
    this.responseHeaders = Object.fromEntries(Object.entries(headers).map(([key, value]) => [key.toLowerCase(), value]))
    this.onload?.(new Event('load'))
  }

  failNetwork() {
    this.onerror?.(new Event('error'))
  }
}

function controlledSignal() {
  const listeners = new Set<EventListenerOrEventListenerObject>()
  const signal = {
    aborted: false,
    addEventListener: vi.fn((_type: string, listener: EventListenerOrEventListenerObject) => listeners.add(listener)),
    removeEventListener: vi.fn((_type: string, listener: EventListenerOrEventListenerObject) => listeners.delete(listener)),
  } as unknown as AbortSignal
  return {
    signal,
    abort() {
      ;(signal as unknown as { aborted: boolean }).aborted = true
      listeners.forEach((listener) => {
        if (typeof listener === 'function') listener(new Event('abort'))
        else listener.handleEvent(new Event('abort'))
      })
    },
  }
}

const DEFAULT_SESSION_BINDING = '99999999999999999999999999999999'

function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}) {
  let responseBody = body
  if (body && typeof body === 'object' && !Array.isArray(body)) {
    const record = body as Record<string, unknown>
    if (record.sessionBinding === undefined && (record.code === 'session_idle_recoverable' || record.ok === true)) {
      responseBody = { ...record, sessionBinding: DEFAULT_SESSION_BINDING }
    }
  }
  return new Response(JSON.stringify(responseBody), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  })
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

async function flushMicrotasks() {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

function authExpiryCalls(spy: ReturnType<typeof vi.spyOn>) {
  return spy.mock.calls.filter(([event]: [Event]) => event.type === 'ft:auth-expired')
}

const multipartLease = { lease: 'lease-secret', uploadUrl: '/legacy', expiresAt: 'later' }

describe('parseErrorPayload', () => {
  afterEach(() => vi.useRealTimers())

  it('extracts JSON message, error and code without echoing HTML', () => {
    expect(parseErrorPayload(400, new Headers(), { message: '参数错误', code: 'bad_input' })).toMatchObject({
      message: '参数错误',
      code: 'bad_input',
    })
    expect(parseErrorPayload(403, new Headers(), { error: '拒绝访问' }).message).toBe('拒绝访问')
    expect(parseErrorPayload(502, new Headers(), '<html><body>proxy secret</body></html>').message).toBe('请求失败（502）')
    expect(parseErrorPayload(500, new Headers(), { message: '<b>unsafe</b>' }).message).toBe('请求失败（500）')
  })

  it('parses Retry-After seconds and HTTP dates', () => {
    expect(parseErrorPayload(429, new Headers({ 'Retry-After': '12' }), { error: '稍后重试' }).retryAfter).toBe(12)
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'))
    expect(parseErrorPayload(429, new Headers({ 'Retry-After': 'Thu, 01 Jan 2026 00:00:05 GMT' }), {}).retryAfter).toBe(5)
  })
})

describe('request credentials and authentication handling', () => {
  beforeEach(() => {
    advanceAuthenticationEpoch()
    resetSessionActivity()
    recordSessionActivity()
    authState.ready = true
    authState.status = 'authenticated'
    authState.authenticated = true
    authState.role = 'user'
    authState.name = 'user'
    authState.user = {
      authenticated: true,
      role: 'user',
      name: 'user',
      sessionBinding: DEFAULT_SESSION_BINDING,
    }
    routerMocks.replace.mockReset()
    routerMocks.route.meta = {}
    routerMocks.route.name = 'files'
    routerMocks.route.fullPath = '/files'
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  it('omits credentials and suppresses global auth expiry for all public lease requests', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ error: '公开令牌无效', code: 'session_idle_recoverable' }, 401))
      .mockResolvedValueOnce(jsonResponse({ lease: 'u', uploadUrl: '/u', expiresAt: 'later' }))
      .mockResolvedValueOnce(jsonResponse({ url: '/d', expiresAt: 'later' }))
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.publicTokenInfo('secret')).rejects.toMatchObject({ status: 401, code: 'session_idle_recoverable' })
    await api.createPublicUploadLease('secret', { fileName: 'a.txt', fileSize: 1 })
    await api.createPublicDownloadLease('secret')

    for (const [, options] of fetchMock.mock.calls) {
      expect(options).toMatchObject({ credentials: 'omit' })
    }
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'ft:auth-expired' }))
    expect(routerMocks.replace).not.toHaveBeenCalled()
    expect(fetchMock).toHaveBeenCalledTimes(3)
    expect(fetchMock.mock.calls.some(([url]) => url === '/api/auth/heartbeat')).toBe(false)
  })

  it('fails closed before fetch for every cookie-protected request when binding is missing', async () => {
    authState.status = 'authenticated'
    authState.authenticated = true
    authState.role = 'admin'
    authState.user = null
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const requests = [
      api.dirs(),
      api.createUploadLease({ dirId: 'default', path: '', fileName: 'a.txt', fileSize: 1 }),
      api.activeTransfers(),
      api.tokens(),
      api.heartbeat(),
      api.logout(),
    ]
    const results = await Promise.allSettled(requests)

    expect(results).toHaveLength(6)
    results.forEach((result) => {
      expect(result.status).toBe('rejected')
      if (result.status === 'rejected') {
        expect(result.reason).toMatchObject({
          status: 409,
          code: 'session_binding_required',
          details: { bindingRequired: true },
        })
        expect(result.reason).not.toMatchObject({ code: 'session_subject_changed' })
      }
    })
    expect(fetchMock).not.toHaveBeenCalled()
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('keeps identity bootstrap and public requests available without a binding', async () => {
    authState.user = null
    authState.status = 'unknown'
    authState.authenticated = false
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/login') return Promise.resolve(jsonResponse({ authenticated: true, role: 'user', sessionBinding: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' }))
      if (url === '/api/auth/admin-login') return Promise.resolve(jsonResponse({ authenticated: true, role: 'admin', sessionBinding: 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' }))
      if (url === '/api/auth/me') return Promise.resolve(jsonResponse({ authenticated: false }))
      if (url === '/t/public/info') return Promise.resolve(jsonResponse({ valid: true }))
      throw new Error(`unexpected URL: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.login('000000')).resolves.toMatchObject({ role: 'user' })
    await expect(api.adminLogin({ username: 'admin', password: 'secret' })).resolves.toMatchObject({ role: 'admin' })
    await expect(api.me()).resolves.toMatchObject({ authenticated: false })
    await expect(api.publicTokenInfo('public')).resolves.toMatchObject({ valid: true })
    expect(fetchMock).toHaveBeenCalledTimes(4)
  })

  it('allows a later operation after binding restoration without resuming the rejected old promise', async () => {
    authState.user = null
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse([]))
    vi.stubGlobal('fetch', fetchMock)

    const oldRequest = api.dirs()
    await expect(oldRequest).rejects.toMatchObject({ status: 409, code: 'session_binding_required' })
    expect(fetchMock).not.toHaveBeenCalled()

    authState.user = {
      authenticated: true,
      role: 'user',
      name: 'restored',
      sessionBinding: 'cccccccccccccccccccccccccccccccc',
    }
    await expect(api.dirs()).resolves.toEqual([])
    expect(fetchMock).toHaveBeenCalledTimes(1)
    await expect(oldRequest).rejects.toMatchObject({ code: 'session_binding_required' })
  })

  it('binds protected cookie requests while excluding public and identity-bootstrap endpoints', async () => {
    const binding = '11111111111111111111111111111111'
    authState.user = { authenticated: true, role: 'user', name: 'user', sessionBinding: binding }
    const fetchMock = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/dirs') return Promise.resolve(jsonResponse([]))
      if (url === '/t/public/info') return Promise.resolve(jsonResponse({ valid: true }))
      if (url === '/api/auth/login') return Promise.resolve(jsonResponse({ authenticated: true, role: 'user', sessionBinding: binding }))
      if (url === '/api/auth/admin-login') return Promise.resolve(jsonResponse({ authenticated: true, role: 'admin', sessionBinding: '22222222222222222222222222222222' }))
      if (url === '/api/auth/me') return Promise.resolve(jsonResponse({ authenticated: true, role: 'user', sessionBinding: binding }))
      if (url === '/api/auth/heartbeat') return Promise.resolve(jsonResponse({ ok: true, sessionBinding: binding }))
      throw new Error(`unexpected URL: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await api.dirs()
    await api.publicTokenInfo('public')
    await api.login('000000')
    await api.adminLogin({ username: 'admin', password: 'secret' })
    await api.me()
    await api.heartbeat()

    const bindingFor = (url: string) => {
      const call = fetchMock.mock.calls.find(([candidate]) => candidate === url)
      return new Headers(call?.[1]?.headers).get('X-Session-Binding')
    }
    expect(bindingFor('/api/dirs')).toBe(binding)
    expect(bindingFor('/api/auth/heartbeat')).toBe(binding)
    expect(bindingFor('/t/public/info')).toBeNull()
    expect(bindingFor('/api/auth/login')).toBeNull()
    expect(bindingFor('/api/auth/admin-login')).toBeNull()
    expect(bindingFor('/api/auth/me')).toBeNull()
  })

  it.each(['409', 'heartbeat binding mismatch'])('never replays an old request when recovery observes a new cookie subject via %s', async (mode) => {
    const oldBinding = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    const newBinding = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
    authState.user = { authenticated: true, role: 'user', name: 'old-user', sessionBinding: oldBinding }
    let dirsCalls = 0
    let heartbeatCalls = 0
    let meCalls = 0
    const fetchMock = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/dirs') {
        dirsCalls += 1
        return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable', sessionBinding: oldBinding }, 401))
      }
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(mode === '409'
          ? jsonResponse({ error: '主体变化', code: 'session_subject_changed' }, 409)
          : jsonResponse({ ok: true, sessionBinding: newBinding }))
      }
      if (url === '/api/auth/me') {
        meCalls += 1
        return Promise.resolve(jsonResponse({ authenticated: true, role: 'admin', name: 'new-admin', sessionBinding: newBinding }))
      }
      throw new Error(`unexpected URL: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.dirs()).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    expect(dirsCalls).toBe(1)
    expect(heartbeatCalls).toBe(1)
    await vi.waitFor(() => {
      expect(meCalls).toBe(1)
      expect(authState.status).toBe('authenticated')
      expect(authState.role).toBe('admin')
      expect(authState.user?.sessionBinding).toBe(newBinding)
    })

    const dirsCall = fetchMock.mock.calls.find(([url]) => url === '/api/dirs')
    const heartbeatCall = fetchMock.mock.calls.find(([url]) => url === '/api/auth/heartbeat')
    const meCall = fetchMock.mock.calls.find(([url]) => url === '/api/auth/me')
    expect(new Headers(dirsCall?.[1]?.headers).get('X-Session-Binding')).toBe(oldBinding)
    expect(new Headers(heartbeatCall?.[1]?.headers).get('X-Session-Binding')).toBe(oldBinding)
    expect(new Headers(meCall?.[1]?.headers).get('X-Session-Binding')).toBeNull()
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('rejects a recoverable response for another binding without heartbeat or replay', async () => {
    const oldBinding = 'cccccccccccccccccccccccccccccccc'
    const newBinding = 'dddddddddddddddddddddddddddddddd'
    authState.user = { authenticated: true, role: 'user', name: 'old-user', sessionBinding: oldBinding }
    let dirsCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/dirs') {
        dirsCalls += 1
        return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable', sessionBinding: newBinding }, 401))
      }
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true, sessionBinding: newBinding }))
      }
      if (url === '/api/auth/me') {
        return Promise.resolve(jsonResponse({ authenticated: true, role: 'admin', name: 'new-admin', sessionBinding: newBinding }))
      }
      throw new Error(`unexpected URL: ${url}`)
    }))

    await expect(api.dirs()).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    expect(dirsCalls).toBe(1)
    expect(heartbeatCalls).toBe(0)
    await vi.waitFor(() => expect(authState.user?.sessionBinding).toBe(newBinding))
  })

  it('treats proactive heartbeat subject changes as revalidation, not logout', async () => {
    const oldBinding = 'abababababababababababababababab'
    const newBinding = 'cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd'
    authState.user = { authenticated: true, role: 'admin', name: 'old-admin', sessionBinding: oldBinding }
    let heartbeatCalls = 0
    let meCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ error: '主体变化', code: 'session_subject_changed' }, 409))
      }
      if (url === '/api/auth/me') {
        meCalls += 1
        return Promise.resolve(jsonResponse({ authenticated: true, role: 'user', name: 'new-user', sessionBinding: newBinding }))
      }
      throw new Error(`unexpected URL: ${url}`)
    }))

    await expect(api.heartbeat()).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    expect(heartbeatCalls).toBe(1)
    await vi.waitFor(() => {
      expect(meCalls).toBe(1)
      expect(authState.status).toBe('authenticated')
      expect(authState.role).toBe('user')
      expect(authState.user?.sessionBinding).toBe(newBinding)
    })
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('invalidates a pending recovery immediately when another tab reports an auth change', async () => {
    const oldBinding = '12121212121212121212121212121212'
    const newBinding = '34343434343434343434343434343434'
    authState.user = { authenticated: true, role: 'admin', name: 'old-admin', sessionBinding: oldBinding }
    const heartbeat = deferred<Response>()
    let dirsCalls = 0
    let meCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/dirs') {
        dirsCalls += 1
        return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable', sessionBinding: oldBinding }, 401))
      }
      if (url === '/api/auth/heartbeat') return heartbeat.promise
      if (url === '/api/auth/me') {
        meCalls += 1
        return Promise.resolve(jsonResponse({ authenticated: true, role: 'user', name: 'new-user', sessionBinding: newBinding }))
      }
      throw new Error(`unexpected URL: ${url}`)
    }))

    const oldRequest = api.dirs()
    await flushMicrotasks()
    expect(acceptExternalAuthSyncEvent({ id: 'cross-tab-pending-recovery', reason: 'login' })).toBe(true)
    expect(authState.status).toBe('unknown')
    expect(authState.role).toBeUndefined()
    expect(authState.user).toBeNull()

    heartbeat.resolve(jsonResponse({ ok: true, sessionBinding: oldBinding }))
    await expect(oldRequest).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    expect(dirsCalls).toBe(1)
    await vi.waitFor(() => {
      expect(meCalls).toBe(1)
      expect(authState.status).toBe('authenticated')
      expect(authState.role).toBe('user')
      expect(authState.user?.sessionBinding).toBe(newBinding)
    })
  })

  it('shares one heartbeat across concurrent recoverable 401s and retries every request once', async () => {
    const heartbeat = deferred<Response>()
    let dirsCalls = 0
    let policyCalls = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return heartbeat.promise
      }
      if (url === '/api/dirs') {
        dirsCalls += 1
        return Promise.resolve(dirsCalls === 1
          ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
          : jsonResponse([{ id: 'one', name: '目录' }]))
      }
      if (url === '/api/upload-policy') {
        policyCalls += 1
        return Promise.resolve(policyCalls === 1
          ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
          : jsonResponse({ maxFileSize: 10 }))
      }
      throw new Error(`unexpected URL: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const dirs = api.dirs()
    const policy = api.uploadLimits()
    await flushMicrotasks()
    expect(heartbeatCalls).toBe(1)
    heartbeat.resolve(jsonResponse({ ok: true }))

    await expect(Promise.all([dirs, policy])).resolves.toHaveLength(2)
    expect(dirsCalls).toBe(2)
    expect(policyCalls).toBe(2)
    expect(heartbeatCalls).toBe(1)
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('shares one heartbeat between proactive activity and a concurrent recoverable 401', async () => {
    const heartbeat = deferred<Response>()
    let heartbeatCalls = 0
    let dirsCalls = 0
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return heartbeat.promise
      }
      dirsCalls += 1
      return Promise.resolve(dirsCalls === 1
        ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
        : jsonResponse([]))
    })
    vi.stubGlobal('fetch', fetchMock)

    const proactive = api.heartbeat()
    const dirs = api.dirs()
    await flushMicrotasks()
    expect(heartbeatCalls).toBe(1)
    heartbeat.resolve(jsonResponse({ ok: true }))

    await expect(Promise.all([proactive, dirs])).resolves.toHaveLength(2)
    expect(heartbeatCalls).toBe(1)
    expect(dirsCalls).toBe(2)
  })

  it('denies public heartbeat at the bottom layer when no activity permit exists', async () => {
    resetSessionActivity()
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)

    await expect(api.heartbeat()).rejects.toMatchObject({
      status: 0,
      code: 'session_activity_required',
      details: { denied: true },
    })

    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('lets a late no-permit poll reuse a successful activity-authorized completed heartbeat', async () => {
    resetSessionActivity()
    const firstPollResponse = deferred<Response>()
    let dirsCalls = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      dirsCalls += 1
      return dirsCalls === 1 ? firstPollResponse.promise : Promise.resolve(jsonResponse([]))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const poll = api.dirs()
    recordSessionActivity()
    await expect(api.heartbeat()).resolves.toMatchObject({ ok: true })
    firstPollResponse.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))

    await expect(poll).resolves.toEqual([])
    expect(heartbeatCalls).toBe(1)
    expect(dirsCalls).toBe(2)
    expect(authState.status).toBe('authenticated')
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it.each([
    ['network error', () => Promise.reject(new TypeError('offline')), 0],
    ['503 response', () => Promise.resolve(jsonResponse({ error: '暂时不可用' }, 503)), 503],
  ])('reuses an activity-authorized completed %s for a late no-permit poll', async (_label, heartbeatResponse, expectedStatus) => {
    resetSessionActivity()
    const firstPollResponse = deferred<Response>()
    let dirsCalls = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return heartbeatResponse()
      }
      dirsCalls += 1
      return dirsCalls === 1 ? firstPollResponse.promise : Promise.resolve(jsonResponse([]))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const pollFailure = api.dirs().then(() => undefined, (error: unknown) => error)
    recordSessionActivity()
    let heartbeatFailure: unknown
    try {
      await api.heartbeat()
    } catch (error) {
      heartbeatFailure = error
    }
    expect(heartbeatFailure).toMatchObject({ status: expectedStatus })
    firstPollResponse.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))

    await expect(pollFailure).resolves.toBe(heartbeatFailure)
    expect(heartbeatCalls).toBe(1)
    expect(dirsCalls).toBe(1)
    expect(authState.status).toBe('authenticated')
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('still expires a late no-permit poll from an activity-authorized completed auth failure', async () => {
    resetSessionActivity()
    const firstPollResponse = deferred<Response>()
    let dirsCalls = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ error: '会话已失效', code: 'session_expired' }, 401))
      }
      dirsCalls += 1
      return dirsCalls === 1 ? firstPollResponse.promise : Promise.resolve(jsonResponse([]))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const pollFailure = api.dirs().then(() => undefined, (error: unknown) => error)
    recordSessionActivity()
    await expect(api.heartbeat()).rejects.toMatchObject({ status: 401 })
    firstPollResponse.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))

    await expect(pollFailure).resolves.toMatchObject({ status: 401 })
    expect(heartbeatCalls).toBe(1)
    expect(dirsCalls).toBe(1)
    expect(authState.status).toBe('anonymous')
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).toHaveBeenCalledTimes(1)
  })

  it('does not let an activity-free heartbeat source authorize recovery', async () => {
    resetSessionActivity()
    let heartbeatCalls = 0
    let dirsCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      dirsCalls += 1
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    }))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.heartbeat()).rejects.toMatchObject({ code: 'session_activity_required' })
    await expect(api.dirs()).rejects.toMatchObject({ status: 401 })

    expect(heartbeatCalls).toBe(0)
    expect(dirsCalls).toBe(1)
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
  })

  it('can recover the cookie-authenticated session probe without redirecting it', async () => {
    let meCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      meCalls += 1
      return Promise.resolve(meCalls === 1
        ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
        : jsonResponse({ authenticated: true, role: 'user' }))
    }))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.me()).resolves.toMatchObject({ authenticated: true })

    expect(meCalls).toBe(2)
    expect(heartbeatCalls).toBe(1)
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('recovers a visible cold-start session in grace from its one initial navigation permit', async () => {
    resetSessionActivity()
    recordInitialNavigationActivity(true)
    authState.ready = false
    authState.status = 'unknown'
    authState.authenticated = false
    authState.user = null
    let meCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      meCalls += 1
      return Promise.resolve(meCalls === 1
        ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
        : jsonResponse({ authenticated: true, role: 'user', name: 'restored', sessionBinding: DEFAULT_SESSION_BINDING }))
    }))

    await expect(restoreSession()).resolves.toMatchObject({ restored: true, status: 'authenticated' })

    expect(meCalls).toBe(2)
    expect(heartbeatCalls).toBe(1)
    expect(authState.status).toBe('authenticated')
  })

  it('does not grant or recover a hidden cold-start session', async () => {
    resetSessionActivity()
    recordInitialNavigationActivity(false)
    authState.ready = false
    authState.status = 'unknown'
    authState.authenticated = false
    authState.user = null
    let meCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      meCalls += 1
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    }))

    await expect(restoreSession()).resolves.toMatchObject({ restored: false, status: 'anonymous' })

    expect(meCalls).toBe(1)
    expect(heartbeatCalls).toBe(0)
    expect(authState.status).toBe('anonymous')
  })

  it('does not let three-second polling refresh an expired activity permit', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(0)
    resetSessionActivity()
    recordSessionActivity()
    let heartbeatCalls = 0
    let businessCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      businessCalls += 1
      return Promise.resolve(Date.now() < 78_000
        ? jsonResponse([])
        : jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    }))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    for (let now = 3_000; now <= 75_000; now += 3_000) {
      vi.setSystemTime(now)
      await expect(api.dirs()).resolves.toEqual([])
    }
    vi.setSystemTime(78_000)
    await expect(api.dirs()).rejects.toMatchObject({ status: 401, code: 'session_idle_recoverable' })

    expect(businessCalls).toBe(26)
    expect(heartbeatCalls).toBe(0)
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).toHaveBeenCalledTimes(1)
  })

  it('broadcasts only once when a shared recovery is definitively rejected', async () => {
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ error: '会话已失效', code: 'session_expired' }, 401))
      }
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const results = await Promise.allSettled([api.dirs(), api.uploadLimits()])

    expect(results.every((result) => result.status === 'rejected')).toBe(true)
    expect(heartbeatCalls).toBe(1)
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).toHaveBeenCalledTimes(1)
  })

  it('does not recover twice when the retried request is still unauthorized', async () => {
    let businessCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      businessCalls += 1
      return Promise.resolve(jsonResponse({
        error: businessCalls === 1 ? '空闲超时' : '仍未授权',
        code: 'session_idle_recoverable',
      }, 401))
    }))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.dirs()).rejects.toMatchObject({ status: 401 })

    expect(businessCalls).toBe(2)
    expect(heartbeatCalls).toBe(1)
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).toHaveBeenCalledTimes(1)
  })

  it('surfaces recovery network and 5xx errors without expiring auth', async () => {
    let networkHeartbeat = true
    const fetchMock = vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        if (networkHeartbeat) {
          networkHeartbeat = false
          return Promise.reject(new TypeError('offline'))
        }
        return Promise.resolve(jsonResponse({ error: '暂时不可用' }, 503))
      }
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.dirs()).rejects.toMatchObject({ status: 0 })
    await expect(api.uploadLimits()).rejects.toMatchObject({ status: 503 })

    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('releases recovery after a heartbeat timeout so a later request can try again', async () => {
    vi.useFakeTimers()
    let businessCalls = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string, options?: RequestInit) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        if (heartbeatCalls === 1) {
          return new Promise<Response>((_resolve, reject) => {
            options?.signal?.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')), { once: true })
          })
        }
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      businessCalls += 1
      return Promise.resolve(businessCalls < 3
        ? jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401)
        : jsonResponse([]))
    })
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const timedOut = api.dirs()
    const timedOutExpectation = expect(timedOut).rejects.toMatchObject({ status: 0, aborted: true })
    await flushMicrotasks()
    expect(heartbeatCalls).toBe(1)
    await vi.advanceTimersByTimeAsync(9_000)
    await timedOutExpectation

    await expect(api.dirs()).resolves.toEqual([])
    expect(heartbeatCalls).toBe(2)
    expect(businessCalls).toBe(3)
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
  })

  it('does not replay an old ordinary-user request after switching to an administrator', async () => {
    const heartbeat = deferred<Response>()
    let dirsCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return heartbeat.promise
      }
      if (url === '/api/auth/admin-login') {
        return Promise.resolve(jsonResponse({ authenticated: true, role: 'admin', name: 'root', sessionBinding: 'abababababababababababababababab' }))
      }
      dirsCalls += 1
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    }))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const oldRequest = api.dirs()
    const oldResult = expect(oldRequest).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    await flushMicrotasks()
    expect(heartbeatCalls).toBe(1)
    await adminLogin('root', 'secret')
    heartbeat.resolve(jsonResponse({ ok: true }))
    await oldResult

    expect(dirsCalls).toBe(1)
    expect(authState.status).toBe('authenticated')
    expect(authState.role).toBe('admin')
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('does not recover or replay an old request after logout begins', async () => {
    const heartbeat = deferred<Response>()
    let dirsCalls = 0
    let heartbeatCalls = 0
    vi.stubGlobal('fetch', vi.fn((url: string) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return heartbeat.promise
      }
      if (url === '/api/auth/logout') return Promise.resolve(jsonResponse({ ok: true }))
      dirsCalls += 1
      return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
    }))

    const oldRequest = api.dirs()
    const oldResult = expect(oldRequest).rejects.toMatchObject({ status: 409, code: 'session_subject_changed' })
    await flushMicrotasks()
    expect(heartbeatCalls).toBe(1)
    await logout()
    heartbeat.resolve(jsonResponse({ ok: true }))
    await oldResult

    expect(dirsCalls).toBe(1)
    expect(authState.status).toBe('anonymous')
    expect(authState.authenticated).toBe(false)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('clears and broadcasts only once for concurrent expiry in the same epoch, even on a public route', async () => {
    routerMocks.route.meta = { public: true }
    routerMocks.route.name = 'share'
    routerMocks.route.fullPath = '/share/token'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: '会话已失效' }, 401)))
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    const requestEpoch = authenticationEpoch()

    const results = await Promise.allSettled([api.dirs(), api.uploadLimits()])

    expect(results.every((result) => result.status === 'rejected')).toBe(true)
    expect(authState.status).toBe('anonymous')
    expect(authState.authenticated).toBe(false)
    expect(authenticationEpoch()).toBe(requestEpoch + 1)
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('clears and broadcasts on the login route without starting redundant navigation', async () => {
    routerMocks.route.meta = { public: true }
    routerMocks.route.name = 'login'
    routerMocks.route.fullPath = '/login'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: '会话已失效' }, 401)))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.dirs()).rejects.toMatchObject({ status: 401 })

    expect(authState.status).toBe('anonymous')
    expect(authExpiryCalls(dispatch)).toHaveLength(1)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('replays a modifying JSON request at most once in the same epoch', async () => {
    let updateCalls = 0
    let handlerExecutions = 0
    let heartbeatCalls = 0
    const fetchMock = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/auth/heartbeat') {
        heartbeatCalls += 1
        return Promise.resolve(jsonResponse({ ok: true }))
      }
      updateCalls += 1
      if (updateCalls === 1) {
        return Promise.resolve(jsonResponse({ error: '空闲超时', code: 'session_idle_recoverable' }, 401))
      }
      handlerExecutions += 1
      return Promise.resolve(jsonResponse({ allowedExtensions: ['txt'], blockedExtensions: ['exe'] }))
    })
    vi.stubGlobal('fetch', fetchMock)
    const payload = { allowedExtensions: ['txt'], blockedExtensions: ['exe'] }

    await expect(api.updateUploadPolicy(payload)).resolves.toMatchObject(payload)

    const modifyingCalls = fetchMock.mock.calls.filter(([url]) => url === '/api/config/upload-policy')
    expect(modifyingCalls).toHaveLength(2)
    expect(modifyingCalls.map(([, options]) => options?.body)).toEqual([
      JSON.stringify(payload),
      JSON.stringify(payload),
    ])
    expect(handlerExecutions).toBe(1)
    expect(heartbeatCalls).toBe(1)
  })

  it('broadcasts and redirects for a protected 401', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: '会话过期' }, 401)))
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    routerMocks.replace.mockImplementationOnce(() => {
      expect(authState.status).toBe('anonymous')
    })

    await expect(api.dirs()).rejects.toMatchObject({ status: 401, message: '会话过期' })

    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({ type: 'ft:auth-expired' }))
    expect(routerMocks.replace).toHaveBeenCalledWith({ name: 'login', query: { redirect: '/files' } })
  })
})

describe('XHR upload behavior', () => {
  beforeEach(() => {
    MockXMLHttpRequest.instances = []
    MockXMLHttpRequest.throwOnSend = undefined
    vi.stubGlobal('XMLHttpRequest', MockXMLHttpRequest)
    authState.user = {
      authenticated: true,
      role: 'user',
      name: 'user',
      sessionBinding: 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee',
    }
  })

  afterEach(() => vi.unstubAllGlobals())

  it('keeps Bearer transfer networking available when the Cookie session binding is absent', async () => {
    authState.user = null
    const promise = api.uploadByLease(multipartLease, new File(['ok'], 'ok.txt'))
    const xhr = MockXMLHttpRequest.instances[0]

    expect(xhr.withCredentials).toBe(false)
    expect(xhr.requestHeaders.Authorization).toBe('Bearer lease-secret')
    expect(xhr.requestHeaders['X-Session-Binding']).toBeUndefined()
    expect(xhr.body).toBeInstanceOf(FormData)
    xhr.respond(200, { ok: true })
    await expect(promise).resolves.toMatchObject({ ok: true })
  })

  it('keeps lease multipart timeout unlimited and credential-free, then cleans up on success', async () => {
    const control = controlledSignal()
    const promise = api.uploadByLease(multipartLease, new File(['ok'], 'ok.txt'), { signal: control.signal })
    const xhr = MockXMLHttpRequest.instances[0]
    expect(xhr.timeout).toBe(0)
    expect(xhr.withCredentials).toBe(false)
    expect(xhr.requestHeaders.Authorization).toBe('Bearer lease-secret')
    expect(xhr.requestHeaders['X-Session-Binding']).toBeUndefined()
    xhr.respond(200, { ok: true, uploaded: 1 })

    await expect(promise).resolves.toMatchObject({ ok: true })
    expect(control.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))
  })

  it('parses multipart HTTP errors and cleans the abort listener', async () => {
    const control = controlledSignal()
    const promise = api.uploadByLease(multipartLease, new File(['x'], 'x.txt'), { signal: control.signal })
    MockXMLHttpRequest.instances[0].respond(429, { error: '请求过多', code: 'rate_limited' }, {
      'content-type': 'application/json',
      'retry-after': '3',
    })

    await expect(promise).rejects.toMatchObject({ status: 429, code: 'rate_limited', retryAfter: 3 })
    expect(control.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))
  })

  it('uses credential-free unlimited raw upload and cleans up on success', async () => {
    const control = controlledSignal()
    const promise = api.uploadByLease({
      lease: 'lease-secret',
      uploadUrl: '/legacy',
      rawUploadUrl: '/raw',
      expiresAt: 'later',
    }, new File(['raw'], 'raw.txt'), { signal: control.signal })
    const xhr = MockXMLHttpRequest.instances[0]
    expect(xhr.timeout).toBe(0)
    expect(xhr.withCredentials).toBe(false)
    expect(xhr.requestHeaders.Authorization).toBe('Bearer lease-secret')
    expect(xhr.requestHeaders['X-Session-Binding']).toBeUndefined()
    xhr.respond(200, { ok: true })

    await expect(promise).resolves.toMatchObject({ ok: true })
    expect(control.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))
  })

  it('keeps Bearer multipart fallback credential-free', async () => {
    const promise = api.uploadByLease({ lease: 'lease-secret', uploadUrl: '/legacy', expiresAt: 'later' }, new File(['x'], 'x.txt'))
    const xhr = MockXMLHttpRequest.instances[0]
    expect(xhr.timeout).toBe(0)
    expect(xhr.withCredentials).toBe(false)
    expect(xhr.requestHeaders.Authorization).toBe('Bearer lease-secret')
    xhr.respond(200, { ok: true })
    await expect(promise).resolves.toMatchObject({ ok: true })
  })

  it('keeps recoverable-looking Bearer transfer 401s out of session recovery', async () => {
    const fetchMock = vi.fn()
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    const multipart = api.uploadByLease(multipartLease, new File(['x'], 'multipart.txt'))
    MockXMLHttpRequest.instances[0].respond(401, { error: 'lease expired', code: 'session_idle_recoverable' })
    await expect(multipart).rejects.toMatchObject({ status: 401, code: 'session_idle_recoverable' })

    const raw = api.uploadByLease({
      lease: 'raw-lease',
      uploadUrl: '/legacy',
      rawUploadUrl: '/raw',
      expiresAt: 'later',
    }, new File(['x'], 'raw.txt'))
    MockXMLHttpRequest.instances[1].respond(401, { error: 'lease expired', code: 'session_idle_recoverable' })
    await expect(raw).rejects.toMatchObject({ status: 401, code: 'session_idle_recoverable' })

    expect(fetchMock).not.toHaveBeenCalled()
    expect(authExpiryCalls(dispatch)).toHaveLength(0)
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('cleans listeners for network errors and aborts', async () => {
    const networkControl = controlledSignal()
    const networkPromise = api.uploadByLease(multipartLease, new File(['x'], 'x.txt'), { signal: networkControl.signal })
    MockXMLHttpRequest.instances[0].failNetwork()
    await expect(networkPromise).rejects.toMatchObject({ status: 0 })
    expect(networkControl.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))

    const abortControl = controlledSignal()
    const abortPromise = api.uploadByLease({ lease: 'lease', uploadUrl: '/u', rawUploadUrl: '/raw', expiresAt: 'later' }, new File(['x'], 'x.txt'), { signal: abortControl.signal })
    abortControl.abort()
    await expect(abortPromise).rejects.toMatchObject({ aborted: true })
    expect(abortControl.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))
  })

  it('turns multipart and raw synchronous send failures into ApiError and cleans listeners', async () => {
    MockXMLHttpRequest.throwOnSend = new DOMException('bad state', 'InvalidStateError')
    const multipartControl = controlledSignal()
    const multipartPromise = api.uploadByLease(multipartLease, new File(['x'], 'x.txt'), { signal: multipartControl.signal })
    await expect(multipartPromise).rejects.toMatchObject({ status: 0, message: '上传请求无法发送。' })
    expect(multipartControl.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))

    const rawControl = controlledSignal()
    const rawPromise = api.uploadByLease({ lease: 'lease', uploadUrl: '/u', rawUploadUrl: '/raw', expiresAt: 'later' }, new File(['x'], 'x.txt'), { signal: rawControl.signal })
    await expect(rawPromise).rejects.toBeInstanceOf(ApiError)
    await expect(rawPromise).rejects.toMatchObject({ status: 0, message: '上传请求无法发送。' })
    expect(rawControl.signal.removeEventListener).toHaveBeenCalledWith('abort', expect.any(Function))
  })
})
