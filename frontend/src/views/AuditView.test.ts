import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const auditLogPage = vi.hoisted(() => vi.fn())

vi.mock('@/api', () => {
  class ApiError extends Error {
    status: number
    aborted: boolean

    constructor(message: string, status: number, details?: unknown) {
      super(message)
      this.name = 'ApiError'
      this.status = status
      this.aborted = Boolean(details && typeof details === 'object' && 'aborted' in details && (details as { aborted?: boolean }).aborted)
    }
  }
  return { ApiError, api: { auditLogPage } }
})

import { ApiError } from '@/api'
import AuditView from '@/views/AuditView.vue'

function pageResult(action: string, page = 1, total = 1, totalPages = 1) {
  return {
    logs: [{ id: action, action, actionLabel: `标签 ${action}`, detail: `详情 ${action}`, status: 'ok' }],
    page,
    pageSize: 50,
    total,
    totalPages,
  }
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

async function setKeyword(wrapper: ReturnType<typeof mount>, value: string) {
  await wrapper.get('input').setValue(value)
}

describe('AuditView server-side filtering', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    auditLogPage.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('renders the initial logs and pagination after the first request', async () => {
    auditLogPage.mockResolvedValue(pageResult('login_success', 2, 120, 3))
    const wrapper = mount(AuditView)
    await flushPromises()

    expect(wrapper.text()).toContain('标签 login_success')
    expect(wrapper.text()).toContain('第 2 / 3 页，共 120 条')
    expect(auditLogPage).toHaveBeenCalledWith(expect.objectContaining({ page: 1, status: 'all', keyword: '' }), expect.any(AbortSignal))
    wrapper.unmount()
  })

  it('hides old results immediately and shows a first-error state for a new filter', async () => {
    auditLogPage.mockResolvedValueOnce(pageResult('old_action'))
    const wrapper = mount(AuditView)
    await flushPromises()
    expect(wrapper.text()).toContain('标签 old_action')

    auditLogPage.mockRejectedValueOnce(new ApiError('筛选失败', 500))
    await setKeyword(wrapper, 'new-filter')
    expect(wrapper.text()).not.toContain('标签 old_action')
    expect(wrapper.text()).not.toContain('获取的旧数据')

    await vi.advanceTimersByTimeAsync(300)
    await flushPromises()
    expect(wrapper.text()).toContain('筛选失败')
    expect(wrapper.text()).not.toContain('标签 old_action')
    expect(wrapper.text()).not.toContain('获取的旧数据')
    wrapper.unmount()
  })

  it('keeps same-filter data stale when a manual refresh fails', async () => {
    auditLogPage.mockResolvedValueOnce(pageResult('stable_action'))
    const wrapper = mount(AuditView)
    await flushPromises()

    auditLogPage.mockRejectedValueOnce(new ApiError('刷新失败', 503))
    const refresh = wrapper.findAll('button').find((button) => button.text() === '刷新')
    expect(refresh).toBeTruthy()
    await refresh!.trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('标签 stable_action')
    expect(wrapper.text()).toContain('获取的旧数据')
    wrapper.unmount()
  })

  it('uses latest-wins when an older filter resolves after a newer filter', async () => {
    auditLogPage.mockResolvedValueOnce(pageResult('initial_action'))
    const firstFilter = deferred<ReturnType<typeof pageResult>>()
    const secondFilter = deferred<ReturnType<typeof pageResult>>()
    auditLogPage.mockImplementationOnce(() => firstFilter.promise)
    auditLogPage.mockImplementationOnce(() => secondFilter.promise)
    const wrapper = mount(AuditView)
    await flushPromises()

    await setKeyword(wrapper, 'first')
    await vi.advanceTimersByTimeAsync(300)
    expect(auditLogPage).toHaveBeenCalledTimes(2)

    await setKeyword(wrapper, 'second')
    expect(wrapper.text()).not.toContain('标签 initial_action')
    await vi.advanceTimersByTimeAsync(300)
    expect(auditLogPage).toHaveBeenCalledTimes(3)

    secondFilter.resolve(pageResult('second_action'))
    await flushPromises()
    expect(wrapper.text()).toContain('标签 second_action')

    firstFilter.resolve(pageResult('first_action'))
    await flushPromises()
    expect(wrapper.text()).toContain('标签 second_action')
    expect(wrapper.text()).not.toContain('标签 first_action')
    wrapper.unmount()
  })
})
