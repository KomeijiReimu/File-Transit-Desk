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

function jsonResponse(body: unknown, status = 200, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json', ...headers },
  })
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
    routerMocks.replace.mockReset()
    routerMocks.route.meta = {}
    routerMocks.route.name = 'files'
    routerMocks.route.fullPath = '/files'
  })

  afterEach(() => vi.unstubAllGlobals())

  it('omits credentials and suppresses global auth expiry for all public lease requests', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse({ error: '公开令牌无效', code: 'invalid_token' }, 401))
      .mockResolvedValueOnce(jsonResponse({ lease: 'u', uploadUrl: '/u', expiresAt: 'later' }))
      .mockResolvedValueOnce(jsonResponse({ url: '/d', expiresAt: 'later' }))
    vi.stubGlobal('fetch', fetchMock)
    const dispatch = vi.spyOn(window, 'dispatchEvent')

    await expect(api.publicTokenInfo('secret')).rejects.toMatchObject({ status: 401, code: 'invalid_token' })
    await api.createPublicUploadLease('secret', { fileName: 'a.txt', fileSize: 1 })
    await api.createPublicDownloadLease('secret')

    for (const [, options] of fetchMock.mock.calls) {
      expect(options).toMatchObject({ credentials: 'omit' })
    }
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'ft:auth-expired' }))
    expect(routerMocks.replace).not.toHaveBeenCalled()
  })

  it('broadcasts and redirects for a protected 401', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(jsonResponse({ error: '会话过期' }, 401)))
    const dispatch = vi.spyOn(window, 'dispatchEvent')

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
  })

  afterEach(() => vi.unstubAllGlobals())

  it('keeps lease multipart timeout unlimited and credential-free, then cleans up on success', async () => {
    const control = controlledSignal()
    const promise = api.uploadByLease(multipartLease, new File(['ok'], 'ok.txt'), { signal: control.signal })
    const xhr = MockXMLHttpRequest.instances[0]
    expect(xhr.timeout).toBe(0)
    expect(xhr.withCredentials).toBe(false)
    expect(xhr.requestHeaders.Authorization).toBe('Bearer lease-secret')
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
