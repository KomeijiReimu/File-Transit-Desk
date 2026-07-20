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
    ...overrides,
  }
}

afterEach(() => vi.useRealTimers())

describe('ChatMessage', () => {
  it('shows an ordinary withdrawn tombstone without the original or source IP', () => {
    const wrapper = mount(ChatMessageCard, {
      props: {
        admin: false,
        message: message({
          status: 'withdrawn',
          body: '普通用户绝不能看到的原文',
          sourceIP: '198.51.100.8',
        }),
      },
    })

    expect(wrapper.text()).toContain('该消息已被撤回')
    expect(wrapper.text()).not.toContain('普通用户绝不能看到的原文')
    expect(wrapper.text()).not.toContain('198.51.100.8')
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

  it('never renders a deleted body or management metadata and removes all actions', () => {
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
    expect(wrapper.text()).not.toContain('192.0.2.55')
    expect(wrapper.find('button').exists()).toBe(false)
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

  it('expires the withdrawal button from the server deadline and emits only while enabled', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-07-20T08:00:00Z'))
    const entry = message({
      isMine: true,
      canWithdraw: true,
      withdrawUntil: '2026-07-20T08:00:02Z',
    })
    const wrapper = mount(ChatMessageCard, { props: { admin: false, message: entry } })
    const button = wrapper.get('.chat-action-button')

    expect(button.text()).toContain('00:02')
    await button.trigger('click')
    expect(wrapper.emitted('withdraw')).toHaveLength(1)

    await vi.advanceTimersByTimeAsync(2_000)
    expect(button.text()).toContain('撤回已过期')
    expect(button.attributes('disabled')).toBeDefined()
  })
})
