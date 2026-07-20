import { flushPromises, mount } from '@vue/test-utils'
import { nextTick, ref } from 'vue'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api'
import type { ChatCapabilities, ChatMessage } from '@/types'

const chatViewMocks = vi.hoisted(() => ({
  authState: null as unknown as {
    ready: boolean
    status: 'unknown' | 'authenticated' | 'anonymous' | 'unavailable'
    authenticated: boolean
    role?: string
    name?: string
    user: { authenticated: boolean; role: string; name: string; sessionBinding: string; expiresAt?: string; idleExpiresAt?: string } | null
  },
  restoreSession: vi.fn(),
  useChatFeed: vi.fn(),
}))

vi.mock('@/auth', async () => {
  const { reactive } = await import('vue')
  const authState = reactive({
    ready: true,
    status: 'authenticated' as const,
    authenticated: true,
    role: 'user' as string | undefined,
    name: '访客' as string | undefined,
    user: {
      authenticated: true,
      role: 'user',
      name: '访客',
      sessionBinding: 'user-binding-a',
    } as { authenticated: boolean; role: string; name: string; sessionBinding: string; expiresAt?: string; idleExpiresAt?: string } | null,
  })
  chatViewMocks.authState = authState
  return { authState, restoreSession: chatViewMocks.restoreSession }
})
vi.mock('@/composables/useChatFeed', () => ({ useChatFeed: chatViewMocks.useChatFeed }))
vi.mock('@/useGsapEntrance', () => ({ useGsapEntrance: vi.fn() }))

import ChatView from '@/views/ChatView.vue'

function capabilities(): ChatCapabilities {
  return {
    maxMessageChars: 2000,
    maxMessageBytes: 8192,
    maxRequestBytes: 8192,
    withdrawWindowSeconds: 300,
    historyDefaultLimit: 50,
    historyMaxLimit: 100,
    changesDefaultLimit: 50,
    changesMaxLimit: 500,
  }
}

function message(id: number, overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id,
    authorTag: `访客-${id}`,
    role: 'user',
    body: `消息 ${id}`,
    status: 'active',
    isMine: false,
    createdAt: '2026-07-20T08:00:00Z',
    withdrawnAt: null,
    deletedAt: null,
    canWithdraw: false,
    withdrawUntil: null,
    ...overrides,
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

function createFeed(initialMessages: ChatMessage[] = []) {
  const messages = ref(initialMessages)
  const feedCapabilities = ref<ChatCapabilities | null>(capabilities())
  const initialized = ref(true)
  const initialLoading = ref(false)
  const generation = ref<number | null>(1)
  const cursor = ref(1)
  const rebind = vi.fn(async () => {
    messages.value = []
    feedCapabilities.value = null
    initialized.value = false
    initialLoading.value = false
    generation.value = null
    cursor.value = 0
    return false
  })
  return {
    messages,
    capabilities: feedCapabilities,
    capabilitiesLoading: ref(false),
    capabilitiesError: ref(''),
    initialLoading,
    initialError: ref(''),
    initialized,
    loadingOlder: ref(false),
    olderError: ref(''),
    hasMore: ref(false),
    nextBeforeId: ref<number | null>(null),
    generation,
    cursor,
    syncing: ref(false),
    syncWarning: ref(''),
    refreshNotice: ref(''),
    lastSyncedAt: ref<Date | null>(new Date()),
    resetBlocked: ref(false),
    connectionState: ref<'loading' | 'online' | 'interrupted'>('online'),
    start: vi.fn(async () => true),
    dispose: vi.fn(),
    retryInitial: vi.fn(async () => true),
    retryCapabilities: vi.fn(async () => true),
    loadOlder: vi.fn(async () => true),
    syncNow: vi.fn(async () => {}),
    reloadAfterSyncFailure: vi.fn(async () => true),
    rebind,
    send: vi.fn(async (_body: string) => message(99, { isMine: true })),
    withdraw: vi.fn(async (_id: number) => message(1, { status: 'withdrawn', body: null, isMine: true })),
    remove: vi.fn(async (_id: number) => message(1, { status: 'deleted', body: null })),
  }
}

function setAuthenticated(role: 'admin' | 'user', sessionBinding: string) {
  chatViewMocks.authState.ready = true
  chatViewMocks.authState.status = 'authenticated'
  chatViewMocks.authState.authenticated = true
  chatViewMocks.authState.role = role
  chatViewMocks.authState.name = role === 'admin' ? '管理员' : '访客'
  chatViewMocks.authState.user = {
    authenticated: true,
    role,
    name: chatViewMocks.authState.name,
    sessionBinding,
  }
}

function setInactive(status: 'unknown' | 'anonymous' | 'unavailable') {
  chatViewMocks.authState.user = null
  chatViewMocks.authState.role = undefined
  chatViewMocks.authState.authenticated = false
  chatViewMocks.authState.status = status
  chatViewMocks.authState.ready = status !== 'unknown'
  chatViewMocks.authState.name = undefined
}

function setUnknown() {
  setInactive('unknown')
}

beforeEach(() => {
  document.body.innerHTML = ''
  setAuthenticated('user', 'user-binding-a')
  chatViewMocks.restoreSession.mockReset()
  chatViewMocks.useChatFeed.mockReset()
})

afterEach(() => {
  document.body.innerHTML = ''
})

describe('ChatView interaction', () => {
  it('retains the exact draft and shows Retry-After feedback when sending fails', async () => {
    const feed = createFeed([message(1)])
    feed.send.mockRejectedValueOnce(new ApiError('rate limited', 429, undefined, 'chat_send_rate_limited', 3))
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await nextTick()
    const textarea = wrapper.get('textarea')
    await textarea.setValue('  草稿不能丢  ')
    await textarea.trigger('keydown', { key: 'Enter' })
    await flushPromises()

    expect(feed.send).toHaveBeenCalledWith('草稿不能丢')
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('  草稿不能丢  ')
    expect(wrapper.text()).toContain('请在 3 秒后重试')
    wrapper.unmount()
  })

  it('clears a confirmed draft and follows the sender message even when the reader was above the bottom', async () => {
    const feed = createFeed([message(1)])
    feed.send.mockImplementationOnce(async (body: string) => {
      const sent = message(2, { body, isMine: true })
      feed.messages.value = [...feed.messages.value, sent]
      return sent
    })
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()
    const log = wrapper.get('.chat-message-log').element as HTMLElement
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 1_000 })
    Object.defineProperty(log, 'clientHeight', { configurable: true, value: 300 })
    Object.defineProperty(log, 'scrollTo', {
      configurable: true,
      value: ({ top }: ScrollToOptions) => { log.scrollTop = Number(top || 0) },
    })
    log.scrollTop = 100
    const textarea = wrapper.get('textarea')

    await textarea.setValue('自己的新消息')
    await textarea.trigger('keydown', { key: 'Enter' })
    await flushPromises()

    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(log.scrollTop).toBe(1_000)
    expect(wrapper.find('.chat-new-message-button').exists()).toBe(false)
    wrapper.unmount()
  })

  it('uses ConfirmDialog for withdrawal and defaults focus to cancel', async () => {
    const own = message(1, {
      isMine: true,
      canWithdraw: true,
      withdrawUntil: '2099-07-20T08:05:00Z',
    })
    const feed = createFeed([own])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await nextTick()

    await wrapper.get('.chat-action-button:not(.danger)').trigger('click')
    await nextTick()
    const dialog = document.querySelector<HTMLElement>('[role="dialog"]')
    const cancel = dialog?.querySelector<HTMLButtonElement>('.confirm-actions .ghost-btn')
    expect(dialog?.textContent).toContain('撤回这条消息？')
    expect(document.activeElement).toBe(cancel)

    dialog?.querySelector<HTMLButtonElement>('.confirm-actions .primary-btn')?.click()
    await flushPromises()
    expect(feed.withdraw).toHaveBeenCalledWith(1)
    expect(document.querySelector('[role="dialog"]')).toBeNull()
    expect(document.activeElement?.classList.contains('chat-message-log')).toBe(true)
    wrapper.unmount()
  })

  it('keeps the confirmation open with a concrete expired-withdraw error and refreshes state', async () => {
    const own = message(3, {
      isMine: true,
      canWithdraw: true,
      withdrawUntil: '2099-07-20T08:05:00Z',
    })
    const feed = createFeed([own])
    feed.withdraw.mockRejectedValueOnce(new ApiError('expired', 409, undefined, 'chat_withdraw_expired'))
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await nextTick()

    await wrapper.get('.chat-action-button:not(.danger)').trigger('click')
    await nextTick()
    document.querySelector<HTMLButtonElement>('[role="dialog"] .confirm-actions .primary-btn')?.click()
    await flushPromises()

    expect(document.querySelector('[role="dialog"]')?.textContent).toContain('这条消息已超过可撤回时间')
    expect(feed.syncNow).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it('lets administrators delete withdrawn messages through the same confirmation flow', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(7, {
      status: 'withdrawn',
      body: '管理员可见原文',
      sourceIP: '198.51.100.7',
    })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await nextTick()

    await wrapper.get('.chat-action-button.danger').trigger('click')
    await nextTick()
    const dialog = document.querySelector<HTMLElement>('[role="dialog"]')
    expect(dialog?.textContent).toContain('删除这条消息？')
    dialog?.querySelector<HTMLButtonElement>('.confirm-actions .primary-btn')?.click()
    await flushPromises()
    expect(feed.remove).toHaveBeenCalledWith(7)
    wrapper.unmount()
  })

  it('removes admin originals, source IP and delete capability before an external restore completes', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const restore = deferred<void>()
    chatViewMocks.restoreSession.mockReturnValueOnce(restore.promise)
    const feed = createFeed([message(7, {
      status: 'withdrawn',
      body: '旧管理员撤回原文',
      sourceIP: '203.0.113.77',
    })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    expect(wrapper.text()).toContain('旧管理员撤回原文')
    expect(wrapper.text()).toContain('203.0.113.77')
    expect(wrapper.find('.chat-action-button.danger').exists()).toBe(true)
    expect(chatViewMocks.useChatFeed).toHaveBeenCalledWith(expect.objectContaining({
      admin: true,
      active: true,
      subjectKey: expect.stringContaining('admin-binding-a'),
    }))

    const restoring = chatViewMocks.restoreSession()
    setUnknown()
    expect(feed.messages.value).toEqual([])
    await nextTick()

    expect(wrapper.text()).not.toContain('旧管理员撤回原文')
    expect(wrapper.text()).not.toContain('203.0.113.77')
    expect(wrapper.find('.chat-action-button.danger').exists()).toBe(false)
    expect(feed.rebind).toHaveBeenCalledWith(expect.objectContaining({ active: false, admin: false }))

    restore.resolve()
    await restoring
    wrapper.unmount()
  })

  it('rebinds an externally restored ordinary user without exposing admin fields', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(1, {
      status: 'withdrawn',
      body: '管理员原文',
      sourceIP: '198.51.100.1',
    })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    setUnknown()
    await nextTick()
    setAuthenticated('user', 'user-binding-b')
    await flushPromises()

    expect(feed.rebind).toHaveBeenLastCalledWith(expect.objectContaining({
      active: true,
      admin: false,
      subjectKey: expect.stringContaining('user-binding-b'),
    }))
    feed.messages.value = [message(2, {
      status: 'withdrawn',
      body: '普通用户不可见原文',
      sourceIP: '198.51.100.2',
    })]
    feed.initialized.value = true
    await nextTick()
    expect(wrapper.text()).not.toContain('普通用户不可见原文')
    expect(wrapper.text()).not.toContain('198.51.100.2')
    expect(wrapper.find('.chat-action-button.danger').exists()).toBe(false)
    wrapper.unmount()
  })

  it('clears first and then activates a newly bound administrator', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(1, { body: '管理员 A 内容', sourceIP: '192.0.2.1' })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    setUnknown()
    expect(feed.messages.value).toEqual([])
    setAuthenticated('admin', 'admin-binding-b')
    expect(feed.messages.value).toEqual([])
    await flushPromises()

    expect(feed.rebind).toHaveBeenLastCalledWith(expect.objectContaining({
      active: true,
      admin: true,
      subjectKey: expect.stringContaining('admin-binding-b'),
    }))
    wrapper.unmount()
  })

  it('closes a pending delete dialog and clears the draft on subject change', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(7, {
      status: 'withdrawn',
      body: '管理员原文',
      sourceIP: '198.51.100.7',
    })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()
    await wrapper.get('textarea').setValue('不能跨主体保留的草稿')
    await wrapper.get('.chat-action-button.danger').trigger('click')
    await nextTick()
    expect(document.querySelector('[role="dialog"]')).not.toBeNull()

    setUnknown()
    await nextTick()

    expect(document.querySelector('[role="dialog"]')).toBeNull()
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(feed.messages.value).toEqual([])
    wrapper.unmount()
  })

  it('does not rebind when a heartbeat only refreshes fields for the same binding and role', async () => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(1)])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    chatViewMocks.authState.user = {
      authenticated: true,
      role: 'admin',
      name: '管理员',
      sessionBinding: 'admin-binding-a',
      expiresAt: '2026-07-21T08:00:00Z',
      idleExpiresAt: '2026-07-20T09:00:00Z',
    }
    await flushPromises()

    expect(feed.rebind).not.toHaveBeenCalled()
    expect(feed.messages.value).toHaveLength(1)
    wrapper.unmount()
  })

  it.each(['unknown', 'anonymous', 'unavailable'] as const)('clears the chat when authenticated status becomes %s', async (status) => {
    setAuthenticated('admin', 'admin-binding-a')
    const feed = createFeed([message(1, { body: '敏感投影', sourceIP: '192.0.2.90' })])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await flushPromises()

    setInactive(status)
    expect(feed.messages.value).toEqual([])
    await nextTick()
    expect(feed.rebind).toHaveBeenLastCalledWith(expect.objectContaining({ active: false }))
    expect(wrapper.text()).not.toContain('敏感投影')
    wrapper.unmount()
  })

  it('does not steal scroll for remote messages while the reader is away from the bottom', async () => {
    const feed = createFeed([message(1)])
    chatViewMocks.useChatFeed.mockReturnValue(feed)
    const wrapper = mount(ChatView, { attachTo: document.body })
    await nextTick()
    await nextTick()
    const log = wrapper.get('.chat-message-log').element as HTMLElement
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 1_000 })
    Object.defineProperty(log, 'clientHeight', { configurable: true, value: 300 })
    Object.defineProperty(log, 'scrollTo', {
      configurable: true,
      value: ({ top }: ScrollToOptions) => { log.scrollTop = Number(top || 0) },
    })
    log.scrollTop = 100

    feed.messages.value = [...feed.messages.value, message(2)]
    await flushPromises()

    expect(log.scrollTop).toBe(100)
    expect(wrapper.get('.chat-new-message-button').text()).toContain('有新消息')
    await wrapper.get('.chat-new-message-button').trigger('click')
    await flushPromises()
    expect(log.scrollTop).toBe(1_000)
    expect(wrapper.find('.chat-new-message-button').exists()).toBe(false)
    wrapper.unmount()
  })
})
