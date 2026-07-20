import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const apiMocks = vi.hoisted(() => {
  class MockApiError extends Error {
    status: number
    details?: unknown
    code?: string
    aborted: boolean

    constructor(message: string, status: number, details?: unknown, code?: string) {
      super(message)
      this.name = 'ApiError'
      this.status = status
      this.details = details
      this.code = code
      this.aborted = Boolean(details && typeof details === 'object' && 'aborted' in details && (details as { aborted?: boolean }).aborted)
    }
  }

  return {
    ApiError: MockApiError,
    dirs: vi.fn(),
    uploadLimits: vi.fn(),
    createUploadLease: vi.fn(),
    uploadByLease: vi.fn(),
  }
})

const bindingMocks = vi.hoisted(() => {
  const state = { value: undefined as string | undefined }
  return {
    state,
    currentSessionBinding: vi.fn(() => state.value),
  }
})

const sessionMocks = vi.hoisted(() => ({
  acquireUploadSessionHold: vi.fn(() => vi.fn()),
}))

vi.mock('@/api', () => ({
  ApiError: apiMocks.ApiError,
  api: {
    dirs: apiMocks.dirs,
    uploadLimits: apiMocks.uploadLimits,
    createUploadLease: apiMocks.createUploadLease,
    uploadByLease: apiMocks.uploadByLease,
  },
}))

vi.mock('@/authEpoch', () => ({
  currentSessionBinding: bindingMocks.currentSessionBinding,
}))

vi.mock('@/useSessionActivity', () => ({
  acquireUploadSessionHold: sessionMocks.acquireUploadSessionHold,
}))

vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {} }),
}))

import { ApiError } from '@/api'
import UploadView from '@/views/UploadView.vue'

function deferred<T = void>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

async function mountView() {
  const wrapper = mount(UploadView, {
    global: {
      stubs: {
        RouterLink: { template: '<a><slot /></a>' },
      },
    },
  })
  await flushPromises()
  return wrapper
}

async function addFiles(wrapper: VueWrapper, names: string[]) {
  const input = wrapper.get('input[type="file"]')
  Object.defineProperty(input.element, 'files', {
    configurable: true,
    value: names.map((name) => new File(['data'], name)),
  })
  await input.trigger('change')
}

function button(wrapper: VueWrapper, label: string) {
  const match = wrapper.findAll('button').find((entry) => entry.text() === label)
  if (!match) throw new Error(`找不到按钮：${label}`)
  return match
}

function authExpiredCalls(dispatch: ReturnType<typeof vi.spyOn>) {
  return dispatch.mock.calls.filter(([event]: [Event]) => event.type === 'ft:auth-expired')
}

describe('UploadView upload identity lifecycle', () => {
  beforeEach(() => {
    bindingMocks.state.value = 'binding-a'
    bindingMocks.currentSessionBinding.mockClear()
    sessionMocks.acquireUploadSessionHold.mockReset().mockImplementation(() => vi.fn())
    apiMocks.dirs.mockReset().mockResolvedValue([{
      id: 'uploads',
      name: 'Uploads',
      label: '上传目录',
      type: 'directory',
      canUpload: true,
      allowUpload: true,
    }])
    apiMocks.uploadLimits.mockReset().mockResolvedValue({
      blockedExtensions: [],
      allowedExtensions: [],
      uploadMaxFileBytes: 1024,
      uploadMaxBytes: 2048,
    })
    apiMocks.createUploadLease.mockReset().mockResolvedValue({
      lease: 'lease-a',
      uploadUrl: '/api/files/upload-by-lease',
      rawUploadUrl: '/api/files/upload-raw-by-lease',
    })
    apiMocks.uploadByLease.mockReset().mockResolvedValue({ ok: true })
  })

  it('stops after a subject_changed lease failure, leaves later files pending, and does not expire the new session', async () => {
    apiMocks.createUploadLease.mockImplementationOnce(async () => {
      bindingMocks.state.value = undefined
      throw new ApiError('主体已变化', 409, undefined, 'session_subject_changed')
    })
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    const wrapper = await mountView()
    await addFiles(wrapper, ['first.txt', 'second.txt'])

    await button(wrapper, '开始上传').trigger('click')
    await flushPromises()

    const items = wrapper.findAll('.upload-queue li')
    expect(apiMocks.createUploadLease).toHaveBeenCalledTimes(1)
    expect(apiMocks.uploadByLease).not.toHaveBeenCalled()
    expect(items.map((item) => item.attributes('data-status'))).toEqual(['error', 'queued'])
    expect(items[1].text()).toContain('待上传')
    expect(wrapper.text()).toContain('登录身份已变化，上传队列已停止')
    expect(authExpiredCalls(dispatch)).toHaveLength(0)

    apiMocks.createUploadLease.mockClear()
    await button(wrapper, '重试').trigger('click')
    await flushPromises()
    expect(apiMocks.createUploadLease).not.toHaveBeenCalled()
    expect(apiMocks.uploadByLease).not.toHaveBeenCalled()
    expect(authExpiredCalls(dispatch)).toHaveLength(0)
    wrapper.unmount()
  })

  it('does not request a lease when Start upload is clicked without a binding', async () => {
    bindingMocks.state.value = undefined
    const wrapper = await mountView()
    await addFiles(wrapper, ['missing-binding.txt'])

    await button(wrapper, '开始上传').trigger('click')
    await flushPromises()

    expect(apiMocks.createUploadLease).not.toHaveBeenCalled()
    expect(apiMocks.uploadByLease).not.toHaveBeenCalled()
    expect(wrapper.get('.upload-queue li').attributes('data-status')).toBe('queued')
    expect(wrapper.text()).toContain('当前登录身份不可用，上传未开始')
    wrapper.unmount()
  })

  it('keeps an authorized raw upload alive across binding change and unmount without leasing the next file', async () => {
    const uploadStarted = deferred()
    const finishUpload = deferred<{ ok: boolean }>()
    let signal: AbortSignal | undefined
    apiMocks.uploadByLease.mockImplementationOnce(async (_lease, _file, options) => {
      signal = options.signal
      uploadStarted.resolve()
      return finishUpload.promise
    })
    const wrapper = await mountView()
    await addFiles(wrapper, ['authorized.txt', 'pending.txt'])

    await button(wrapper, '开始上传').trigger('click')
    await uploadStarted.promise
    bindingMocks.state.value = 'binding-b'
    wrapper.unmount()

    expect(signal?.aborted).toBe(false)
    finishUpload.resolve({ ok: true })
    await flushPromises()

    expect(apiMocks.createUploadLease).toHaveBeenCalledTimes(1)
    expect(apiMocks.uploadByLease).toHaveBeenCalledTimes(1)
    expect(signal?.aborted).toBe(false)
  })

  it('dispatches auth expiry only for a real upload-session-expired event after the current lease completes', async () => {
    const uploadStarted = deferred()
    const finishUpload = deferred<{ ok: boolean }>()
    apiMocks.uploadByLease.mockImplementationOnce(async () => {
      uploadStarted.resolve()
      return finishUpload.promise
    })
    const dispatch = vi.spyOn(window, 'dispatchEvent')
    const wrapper = await mountView()
    await addFiles(wrapper, ['idle-expired.txt'])

    await button(wrapper, '开始上传').trigger('click')
    await uploadStarted.promise
    window.dispatchEvent(new CustomEvent('ft:upload-session-expired'))
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).toContain('已授权的当前上传会继续')

    finishUpload.resolve({ ok: true })
    await flushPromises()

    expect(authExpiredCalls(dispatch)).toHaveLength(1)
    expect(wrapper.text()).toContain('登录状态已过期，请重新登录后查看文件')
    wrapper.unmount()
  })
})
