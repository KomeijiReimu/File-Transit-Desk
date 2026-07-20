import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const apiMocks = vi.hoisted(() => {
  class MockApiError extends Error {
    status: number

    constructor(message: string, status: number) {
      super(message)
      this.name = 'ApiError'
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

const routerMocks = vi.hoisted(() => ({
  replace: vi.fn(),
  route: { query: {} as Record<string, unknown> },
}))

vi.mock('@/api', () => ({
  ApiError: apiMocks.ApiError,
  api: {
    me: apiMocks.me,
    login: apiMocks.login,
    adminLogin: apiMocks.adminLogin,
    logout: apiMocks.logout,
  },
}))

vi.mock('vue-router', () => ({
  useRouter: () => ({ replace: routerMocks.replace }),
  useRoute: () => routerMocks.route,
}))

import { ApiError } from '@/api'
import { authState } from '@/auth'
import LoginView from '@/views/LoginView.vue'

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void
  let reject!: (reason?: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

function setUnavailable() {
  authState.ready = true
  authState.status = 'unavailable'
  authState.authenticated = false
  authState.role = undefined
  authState.name = undefined
  authState.user = null
}

describe('LoginView unavailable session state', () => {
  beforeEach(() => {
    apiMocks.me.mockReset()
    apiMocks.login.mockReset()
    apiMocks.adminLogin.mockReset()
    apiMocks.logout.mockReset()
    routerMocks.replace.mockReset().mockResolvedValue(undefined)
    routerMocks.route.query = { unavailable: '1', redirect: '/config?section=storage' }
    setUnavailable()
  })

  afterEach(() => {
    document.body.innerHTML = ''
  })

  it('shows a reconnect state instead of asking for credentials and keeps its primary action keyboard-ready', async () => {
    const wrapper = mount(LoginView, { attachTo: document.body })
    await flushPromises()

    expect(wrapper.text()).toContain('服务暂时不可用')
    expect(wrapper.text()).toContain('登录凭据可能仍然有效')
    expect(wrapper.find('form').exists()).toBe(false)
    expect(wrapper.find('[role="tablist"]').exists()).toBe(false)
    expect(wrapper.find('input').exists()).toBe(false)
    const reconnect = wrapper.get('button')
    expect(reconnect.text()).toBe('重新连接')
    expect(reconnect.attributes('type')).toBe('button')
    expect(reconnect.attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('restores the existing session and safely returns to the requested page', async () => {
    apiMocks.me.mockResolvedValue({ authenticated: true, role: 'admin', name: 'restored-admin' })
    const wrapper = mount(LoginView, { attachTo: document.body })
    await flushPromises()

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(apiMocks.me).toHaveBeenCalledTimes(1)
    expect(authState.status).toBe('authenticated')
    expect(routerMocks.replace).toHaveBeenCalledWith('/config?section=storage')
    wrapper.unmount()
  })

  it('prevents duplicate retries, exposes loading state, and keeps focus on retry after a server failure', async () => {
    const request = deferred<{ authenticated: boolean }>()
    apiMocks.me.mockReturnValue(request.promise)
    const wrapper = mount(LoginView, { attachTo: document.body })
    await flushPromises()

    const reconnect = wrapper.get('button')
    await reconnect.trigger('click')
    await wrapper.vm.$nextTick()
    expect(reconnect.attributes('disabled')).toBeDefined()
    expect(reconnect.attributes('aria-busy')).toBe('true')
    expect(reconnect.text()).toContain('正在连接')
    expect(wrapper.get('[aria-live="polite"]').text()).toBe('正在重新连接服务。')

    await reconnect.trigger('click')
    expect(apiMocks.me).toHaveBeenCalledTimes(1)

    request.reject(new ApiError('maintenance', 503))
    await flushPromises()

    expect(authState.status).toBe('unavailable')
    expect(routerMocks.replace).not.toHaveBeenCalled()
    expect(wrapper.get('[role="alert"]').text()).toBe('服务仍未恢复，服务器暂时无法响应，请稍后重试。')
    expect(wrapper.get('[aria-live="polite"]').text()).toBe('重新连接未成功。')
    expect(wrapper.text()).not.toContain('登录已过期')
    expect(reconnect.attributes('disabled')).toBeUndefined()
    expect(reconnect.text()).toBe('重新连接')
    expect(document.activeElement).toBe(reconnect.element)
    wrapper.unmount()
  })

  it('switches naturally to the normal login form when the service explicitly returns 401', async () => {
    apiMocks.me.mockRejectedValue(new ApiError('unauthorized', 401))
    const wrapper = mount(LoginView, { attachTo: document.body })
    await flushPromises()

    await wrapper.get('button').trigger('click')
    await flushPromises()

    expect(authState.status).toBe('anonymous')
    expect(wrapper.find('form').exists()).toBe(true)
    expect(wrapper.find('[role="tablist"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('登录文件传输台')
    expect(wrapper.text()).not.toContain('服务暂时不可用')
    const codeInput = wrapper.get('input[autocomplete="one-time-code"]')
    expect(document.activeElement).toBe(codeInput.element)
    expect(routerMocks.replace).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
