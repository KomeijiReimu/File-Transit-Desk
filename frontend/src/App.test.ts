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

  it('shows 在线交流 to an ordinary authenticated user', () => {
    const wrapper = mount(App, { global })
    expect(wrapper.text()).toContain('在线交流')
    expect(wrapper.text()).not.toContain('令牌管理')
  })

  it('shows the same chat destination to an administrator', () => {
    setAuthenticated('aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 'admin')
    const wrapper = mount(App, { global })
    expect(wrapper.text()).toContain('在线交流')
    expect(wrapper.text()).toContain('令牌管理')
  })

  it('uses an explicit menu control and toggles the compact navigation with a class', async () => {
    const wrapper = mount(App, { global })
    const toggle = wrapper.get('.mobile-nav-toggle')
    const navigation = wrapper.get('#main-navigation')

    expect(toggle.text()).toBe('展开菜单')
    expect(toggle.attributes('aria-label')).toBe('展开菜单')
    expect(toggle.attributes('aria-controls')).toBe('main-navigation')
    expect(toggle.attributes('aria-expanded')).toBe('false')
    expect(navigation.classes()).not.toContain('is-open')

    await toggle.trigger('click')
    expect(toggle.text()).toBe('收起菜单')
    expect(toggle.attributes('aria-label')).toBe('收起菜单')
    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(navigation.classes()).toContain('is-open')
  })

  it('keeps the desktop navigation in the document and closes an expanded menu after routing', async () => {
    const wrapper = mount(App, { global })
    const navigation = wrapper.get('nav[aria-label="主导航"]')
    expect(navigation.text()).toContain('文件浏览')
    expect(navigation.text()).toContain('文件上传')
    expect(wrapper.find('.brand small').exists()).toBe(false)
    expect(wrapper.find('.sidebar-card .meta small').exists()).toBe(false)
    expect(wrapper.get('.sidebar-role-label.visually-hidden').text()).toBe('受信用户会话')
    expect(wrapper.get('.mobile-nav-toggle').attributes('aria-controls')).toBe(navigation.attributes('id'))

    await wrapper.get('.mobile-nav-toggle').trigger('click')
    expect(navigation.classes()).toContain('is-open')
    appMocks.route.fullPath = '/upload'
    appMocks.route.name = 'upload'
    await nextTick()

    expect(navigation.classes()).not.toContain('is-open')
    expect(wrapper.get('.mobile-nav-toggle').text()).toBe('展开菜单')
    expect(wrapper.get('.mobile-nav-toggle').attributes('aria-expanded')).toBe('false')
  })

  it.each(['share', 'login'])('keeps navigation off the public %s page', (name) => {
    appMocks.route.name = name
    appMocks.route.fullPath = name === 'share' ? '/share/token' : '/login'
    clearProtectedSubject()
    const wrapper = mount(App, { global })
    expect(wrapper.text()).not.toContain('在线交流')
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
