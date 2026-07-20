import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import ChatMessageCard from '@/components/ChatMessage.vue'
import type { ChatMessage } from '@/types'

function message(overrides: Partial<ChatMessage> = {}): ChatMessage {
  return {
    id: 1,
    authorTag: '访客-ABC123',
    role: 'user',
    body: '普通消息',
    status: 'active',
    isMine: false,
    createdAt: '2026-07-20T08:00:00Z',
    withdrawnAt: null,
    deletedAt: null,
    canWithdraw: false,
    withdrawUntil: null,
    sourceIP: '192.0.2.10',
    ...overrides,
  }
}

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('ChatMessage', () => {
  it('shows an ordinary withdrawn tombstone with its IP but without the original or admin-only fields', () => {
    const entry = {
      ...message({
        status: 'withdrawn',
        body: '普通用户绝不能看到的原文',
        sourceIP: '198.51.100.8',
      }),
      authorKey: 'hidden-author-key',
      deletedBy: 'hidden-admin-key',
    } as ChatMessage
    const wrapper = mount(ChatMessageCard, {
      props: {
        admin: false,
        message: entry,
      },
    })

    expect(wrapper.text()).toContain('该消息已被撤回')
    expect(wrapper.text()).not.toContain('普通用户绝不能看到的原文')
    expect(wrapper.text()).toContain('198.51.100.8')
    expect(wrapper.text()).not.toContain('hidden-author-key')
    expect(wrapper.text()).not.toContain('hidden-admin-key')
  })

  it('shows the admin-only withdrawn original and IP as literal plain text', () => {
    const hostile = '<img src=x onerror="window.__xss = true">\n第二行'
    const wrapper = mount(ChatMessageCard, {
      props: {
        admin: true,
        message: message({
          status: 'withdrawn',
          body: hostile,
          sourceIP: '203.0.113.4',
          withdrawnAt: '2026-07-20T08:01:00Z',
        }),
      },
    })

    expect(wrapper.text()).toContain('用户已撤回 · 原文仅管理员可见')
    expect(wrapper.text()).toContain('<img src=x onerror="window.__xss = true">')
    expect(wrapper.text()).toContain('203.0.113.4')
    expect(wrapper.find('img').exists()).toBe(false)
    expect((window as unknown as { __xss?: boolean }).__xss).toBeUndefined()
  })

  it('shows a deleted message IP while never rendering its body or actions', () => {
    const wrapper = mount(ChatMessageCard, {
      props: {
        admin: true,
        message: message({
          status: 'deleted',
          body: '即使错误传入也不能显示',
          sourceIP: '192.0.2.55',
          deletedAt: '2026-07-20T08:02:00Z',
        }),
      },
    })

    expect(wrapper.text()).toContain('该消息已由管理员删除')
    expect(wrapper.text()).not.toContain('即使错误传入也不能显示')
    expect(wrapper.text()).toContain('192.0.2.55')
    expect(wrapper.find('button').exists()).toBe(false)
  })

  it('shows IP metadata on an ordinary active message without administrator controls', () => {
    const wrapper = mount(ChatMessageCard, {
      props: { admin: false, message: message({ sourceIP: '203.0.113.9' }) },
    })

    expect(wrapper.text()).toContain('203.0.113.9')
    expect(wrapper.text()).toContain('普通消息')
    expect(wrapper.find('.chat-action-button.danger').exists()).toBe(false)
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(false)
  })

  it('renders an administrator selection control only for non-deleted messages', async () => {
    const wrapper = mount(ChatMessageCard, {
      props: { admin: true, selectable: true, selected: false, message: message() },
    })
    const checkbox = wrapper.get('input[type="checkbox"]')
    await checkbox.setValue(true)
    expect(wrapper.emitted('selection-change')?.[0]).toEqual([expect.objectContaining({ id: 1 }), true])

    await wrapper.setProps({ message: message({ status: 'deleted', body: null }), selectable: false })
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(false)
  })

  it.each([
    ['active', '正常消息'],
    ['withdrawn', '撤回原文'],
  ] as const)('allows an administrator to delete a %s message', async (status, body) => {
    const wrapper = mount(ChatMessageCard, {
      props: { admin: true, message: message({ status, body }) },
    })

    await wrapper.get('.chat-action-button.danger').trigger('click')
    expect(wrapper.emitted('delete')?.[0]?.[0]).toMatchObject({ status })
  })

  it('uses a one-shot expiry without rendering a countdown or starting an interval', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-07-20T08:00:00Z'))
    const intervalSpy = vi.spyOn(window, 'setInterval')
    const timeoutSpy = vi.spyOn(window, 'setTimeout')
    const entry = message({
      isMine: true,
      canWithdraw: true,
      withdrawUntil: '2026-07-20T08:00:02Z',
    })
    const wrapper = mount(ChatMessageCard, { props: { admin: false, message: entry } })
    const button = wrapper.get('.chat-action-button')

    expect(button.text()).toBe('撤回')
    expect(button.attributes('aria-label')).toBe('撤回消息')
    expect(intervalSpy).not.toHaveBeenCalled()
    expect(timeoutSpy).toHaveBeenCalledWith(expect.any(Function), 2_000)
    await button.trigger('click')
    expect(wrapper.emitted('withdraw')).toHaveLength(1)

    const expiryCallback = timeoutSpy.mock.calls.find(([, delay]) => delay === 2_000)?.[0]
    expect(expiryCallback).toBeTypeOf('function')
    vi.setSystemTime(new Date('2026-07-20T08:00:02Z'))
    ;(expiryCallback as () => void)()
    await wrapper.vm.$nextTick()
    expect(wrapper.find('.chat-action-button').exists()).toBe(false)
    expect(intervalSpy).not.toHaveBeenCalled()
  })
})
