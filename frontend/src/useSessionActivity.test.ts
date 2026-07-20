import { mount } from '@vue/test-utils'
import { defineComponent, h } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const activityMocks = vi.hoisted(() => {
  class MockApiError extends Error {
    status: number

    constructor(message: string, status: number) {
      super(message)
      this.status = status
    }
  }

  return {
    ApiError: MockApiError,
    heartbeat: vi.fn(),
    replace: vi.fn(),
    route: { meta: {} as Record<string, unknown>, name: 'files', fullPath: '/files' },
  }
})

vi.mock('@/api', () => ({
  ApiError: activityMocks.ApiError,
  api: { heartbeat: activityMocks.heartbeat },
}))

vi.mock('vue-router', () => ({
  useRoute: () => activityMocks.route,
  useRouter: () => ({ replace: activityMocks.replace }),
}))

import { ApiError } from '@/api'
import { authState } from '@/auth'
import { recentSessionActivity, recordSessionActivity, resetSessionActivity } from '@/sessionActivity'
import { acquireUploadSessionHold, useSessionActivity } from '@/useSessionActivity'

const Harness = defineComponent({
  setup() {
    useSessionActivity()
    return () => h('div')
  },
})

let visibility: DocumentVisibilityState = 'visible'

async function flushMicrotasks() {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

function eventCalls(spy: ReturnType<typeof vi.spyOn>, type: string) {
  return spy.mock.calls.filter(([event]: [Event]) => event.type === type)
}

describe('useSessionActivity', () => {
  beforeEach(() => {
    visibility = 'visible'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility)
    activityMocks.heartbeat.mockReset().mockResolvedValue({ ok: true })
    activityMocks.replace.mockReset()
    activityMocks.route.meta = {}
    activityMocks.route.name = 'files'
    activityMocks.route.fullPath = '/files'
    resetSessionActivity()
    authState.ready = true
    authState.status = 'authenticated'
    authState.authenticated = true
    authState.role = 'user'
    authState.name = 'user'
    authState.user = { authenticated: true, role: 'user', name: 'user' }
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('does not keep a hidden page alive and resumes on visibility or focus', async () => {
    visibility = 'hidden'
    const wrapper = mount(Harness)
    await flushMicrotasks()

    window.dispatchEvent(new Event('input'))
    window.dispatchEvent(new Event('pointerdown'))
    await flushMicrotasks()
    expect(activityMocks.heartbeat).not.toHaveBeenCalled()

    visibility = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    await flushMicrotasks()
    expect(activityMocks.heartbeat).toHaveBeenCalledTimes(1)

    window.dispatchEvent(new Event('focus'))
    await flushMicrotasks()
    expect(activityMocks.heartbeat).toHaveBeenCalledTimes(2)
    wrapper.unmount()
  })

  it('treats input as real activity, throttles bursts, and has no unconditional timer', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(1_000)
    recordSessionActivity()
    const wrapper = mount(Harness)
    await flushMicrotasks()
    expect(activityMocks.heartbeat).toHaveBeenCalledTimes(1)
    activityMocks.heartbeat.mockClear()

    window.dispatchEvent(new Event('input'))
    window.dispatchEvent(new Event('keydown'))
    window.dispatchEvent(new Event('scroll'))
    await flushMicrotasks()
    expect(activityMocks.heartbeat).not.toHaveBeenCalled()

    await vi.advanceTimersByTimeAsync(30_000)
    expect(activityMocks.heartbeat).not.toHaveBeenCalled()
    window.dispatchEvent(new Event('input'))
    await flushMicrotasks()
    expect(activityMocks.heartbeat).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('grants a recovery permit from one visible input without any polling side effect', async () => {
    resetSessionActivity()
    const wrapper = mount(Harness)
    await flushMicrotasks()
    expect(activityMocks.heartbeat).not.toHaveBeenCalled()
    expect(recentSessionActivity()).toBeUndefined()

    window.dispatchEvent(new Event('input'))
    await flushMicrotasks()

    expect(recentSessionActivity()).toMatchObject({ generation: expect.any(Number) })
    expect(activityMocks.heartbeat).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('keeps an authorized upload held when heartbeat reports 401', async () => {
    recordSessionActivity()
    activityMocks.heartbeat.mockRejectedValue(new ApiError('expired', 401))
    const release = acquireUploadSessionHold()
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    const wrapper = mount(Harness)
    await flushMicrotasks()

    expect(eventCalls(dispatch, 'ft:upload-session-expired')).toHaveLength(1)
    expect(eventCalls(dispatch, 'ft:auth-expired')).toHaveLength(0)
    expect(authState.authenticated).toBe(true)
    expect(activityMocks.replace).not.toHaveBeenCalled()

    release()
    wrapper.unmount()
  })

  it.each(['visible', 'hidden'] as const)('does not heartbeat or interrupt a held upload while a long-running page stays %s and idle', async (pageState) => {
    vi.useFakeTimers()
    vi.setSystemTime(1_000)
    visibility = pageState
    const release = acquireUploadSessionHold()
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    const wrapper = mount(Harness)

    if (pageState === 'hidden') window.dispatchEvent(new Event('input'))
    await vi.advanceTimersByTimeAsync(10 * 60_000)
    await flushMicrotasks()

    expect(activityMocks.heartbeat).not.toHaveBeenCalled()
    expect(eventCalls(dispatch, 'ft:upload-session-expired')).toHaveLength(0)
    expect(authState.authenticated).toBe(true)
    expect(activityMocks.replace).not.toHaveBeenCalled()

    release()
    wrapper.unmount()
  })
})
