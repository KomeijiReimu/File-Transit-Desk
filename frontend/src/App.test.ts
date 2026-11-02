import { enableAutoUnmount, mount } from '@vue/test-utils'
import { defineComponent, h, nextTick, onUnmounted } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const appMocks = vi.hoisted(() => ({
  route: { name: 'files', fullPath: '/files' },
  authState: undefined as any,
  logout: vi.fn(async () => {}),
  authenticationEpoch: vi.fn(() => 7),
  invalidateSessionSubject: vi.fn(() => true),
  useSessionActivity: vi.fn(),
  probeMounts: [] as string[],
  probeUnmounts: [] as string[],
  probeAPICalls: vi.fn(),
}))

vi.mock('vue-router', async () => {
  const { reactive } = await vi.importActual<typeof import('vue')>('vue')
  appMocks.route = reactive(appMocks.route)
  return { useRoute: () => appMocks.route }
})

vi.mock('@/auth', async () => {
  const { computed, reactive } = await vi.importActual<typeof import('vue')>('vue')
  const authState = reactive({
    ready: true,
    status: 'authenticated',
    authenticated: true,
    role: 'user',
    name: '访客',
    user: { authenticated: true, role: 'user', sessionBinding: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' },
  })
  appMocks.authState = authState
  return {
    authState,
    isAdmin: computed(() => authState.authenticated && authState.role === 'admin'),
    logout: appMocks.logout,
  }
})

vi.mock('@/useSessionActivity', () => ({ useSessionActivity: appMocks.useSessionActivity }))
vi.mock('@/authEpoch', () => ({
  authenticationEpoch: appMocks.authenticationEpoch,
  invalidateSessionSubject: appMocks.invalidateSessionSubject,
}))

import App from '@/App.vue'

enableAutoUnmount(afterEach)

const ProtectedProbe = defineComponent({
  setup() {
    const binding = appMocks.authState.user?.sessionBinding || ''
    appMocks.probeMounts.push(binding)
    let active = true
    const timer = window.setTimeout(() => {
      if (active) appMocks.probeAPICalls(binding)
    }, 50)
    onUnmounted(() => {
      active = false
      window.clearTimeout(timer)
      appMocks.probeUnmounts.push(binding)
    })
    return () => h('div', { 'data-router-view': '', 'data-binding': binding })
  },
})

const global = {
  stubs: {
    RouterLink: { props: ['to'], template: '<a><slot /></a>' },
    RouterView: ProtectedProbe,
  },
}

function setAuthenticated(binding = 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', role: 'user' | 'admin' = 'user') {
  appMocks.authState.ready = true
  appMocks.authState.status = 'authenticated'
  appMocks.authState.authenticated = true
  appMocks.authState.role = role
  appMocks.authState.name = role === 'admin' ? '管理员' : '访客'
  appMocks.authState.user = { authenticated: true, role, sessionBinding: binding }
}

function clearProtectedSubject(status: 'unknown' | 'anonymous' | 'unavailable' = 'unknown') {
  appMocks.authState.ready = status !== 'unknown'
  appMocks.authState.status = status
  appMocks.authState.authenticated = false
  appMocks.authState.role = undefined
  appMocks.authState.name = undefined
  appMocks.authState.user = null
}

describe('application chat navigation', () => {
  beforeEach(() => {
    appMocks.route.name = 'files'
    appMocks.route.fullPath = '/files'
    setAuthenticated()
    appMocks.logout.mockClear()
    appMocks.authenticationEpoch.mockClear()
    appMocks.invalidateSessionSubject.mockClear()
    appMocks.probeMounts.length = 0
    appMocks.probeUnmounts.length = 0
    appMocks.probeAPICalls.mockClear()
  })

  afterEach(() => vi.useRealTimers())

  it('limits administrator navigation to the administrator role', async () => {
    const wrapper = mount(App, { global })
    expect(wrapper.findAllComponents(global.stubs.RouterLink).map((link) => link.props('to'))).toContain('/chat')
    expect(wrapper.findAllComponents(global.stubs.RouterLink).map((link) => link.props('to'))).not.toContain('/tokens')
    setAuthenticated('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'admin')
    await nextTick()
    expect(wrapper.findAllComponents(global.stubs.RouterLink).map((link) => link.props('to'))).toContain('/tokens')
  })

  it('closes an expanded navigation menu after routing', async () => {
    const wrapper = mount(App, { global })
    await wrapper.get('.mobile-nav-toggle').trigger('click')
    expect(wrapper.get('.mobile-nav-toggle').attributes('aria-expanded')).toBe('true')
    appMocks.route.fullPath = '/upload'
    appMocks.route.name = 'upload'
    await nextTick()

    expect(wrapper.get('.mobile-nav-toggle').attributes('aria-expanded')).toBe('false')
  })

  it.each(['share', 'login'])('keeps navigation off the public %s page', (name) => {
    appMocks.route.name = name
    appMocks.route.fullPath = name === 'share' ? '/share/token' : '/login'
    clearProtectedSubject()
    const wrapper = mount(App, { global })
    expect(wrapper.findAllComponents(global.stubs.RouterLink)).toHaveLength(0)
    expect(wrapper.find('[data-router-view]').exists()).toBe(true)
  })

  it('lets the centralized auth-route synchronizer own logout navigation', async () => {
    const wrapper = mount(App, { global })
    await wrapper.get('button.full').trigger('click')

    expect(appMocks.logout).toHaveBeenCalledTimes(1)
  })

  it.each(['unknown', 'anonymous', 'unavailable'] as const)('unmounts the protected route synchronously when auth becomes %s', async (status) => {
    vi.useFakeTimers()
    const wrapper = mount(App, { global })
    expect(appMocks.probeMounts).toEqual(['aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'])

    clearProtectedSubject(status)
    await nextTick()

    expect(wrapper.find('[data-router-view]').exists()).toBe(false)
    expect(appMocks.probeUnmounts).toEqual(['aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'])
    expect(wrapper.get('button.full').attributes('disabled')).toBeDefined()
    await vi.advanceTimersByTimeAsync(100)
    expect(appMocks.probeAPICalls).not.toHaveBeenCalled()
  })

  it('remounts a protected route for a new binding even when the role is unchanged', async () => {
    vi.useFakeTimers()
    const wrapper = mount(App, { global })
    const replacement = 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'

    setAuthenticated(replacement, 'user')
    await nextTick()

    expect(appMocks.probeMounts).toEqual(['aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', replacement])
    expect(appMocks.probeUnmounts).toEqual(['aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'])
    expect(wrapper.get('[data-router-view]').attributes('data-binding')).toBe(replacement)
    await vi.advanceTimersByTimeAsync(100)
    expect(appMocks.probeAPICalls).toHaveBeenCalledTimes(1)
    expect(appMocks.probeAPICalls).toHaveBeenCalledWith(replacement)
  })

  it('requests subject revalidation once when authenticated state has no binding, without looping while unknown', async () => {
    const wrapper = mount(App, { global })

    appMocks.authState.user = { authenticated: true, role: 'user', sessionBinding: '' }
    await nextTick()
    await nextTick()

    expect(wrapper.find('[data-router-view]').exists()).toBe(false)
    expect(appMocks.invalidateSessionSubject).toHaveBeenCalledTimes(1)
    expect(appMocks.invalidateSessionSubject).toHaveBeenCalledWith(7, undefined, 'session_subject_changed')

    appMocks.authState.status = 'unknown'
    await nextTick()
    expect(appMocks.invalidateSessionSubject).toHaveBeenCalledTimes(1)
  })
})
