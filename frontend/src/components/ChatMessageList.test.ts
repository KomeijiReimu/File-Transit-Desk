import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import { afterEach, describe, expect, it, vi } from 'vitest'
import ChatMessageList from '@/components/ChatMessageList.vue'
import type { ChatMessage } from '@/types'

type ListHandle = {
  scrollToBottom: (smooth?: boolean) => void
  loadOlderPreservingPosition: (load: () => Promise<boolean>) => Promise<boolean>
}

function message(id: number): ChatMessage {
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
    sourceIP: `192.0.2.${id}`,
  }
}

afterEach(() => {
  vi.restoreAllMocks()
  document.body.innerHTML = ''
})

describe('ChatMessageList', () => {
  it('enables polite live updates only after the initial history has rendered', async () => {
    const wrapper = mount(ChatMessageList, {
      props: { messages: [message(1), message(2)], admin: false },
    })
    const log = wrapper.get('[role="log"]')

    expect(log.attributes('aria-live')).toBe('off')
    await flushPromises()
    expect(log.attributes('aria-live')).toBe('polite')
    expect(log.attributes('aria-relevant')).toBe('additions text')
    expect(log.attributes('tabindex')).toBe('0')
  })

  it('preserves visual scroll position and keyboard focus when older messages prepend', async () => {
    const wrapper = mount(ChatMessageList, {
      props: { messages: [message(3), message(4)], admin: false, hasMore: true },
      attachTo: document.body,
    })
    await nextTick()
    const log = wrapper.get('.chat-message-log').element as HTMLElement
    let height = 1_000
    Object.defineProperty(log, 'scrollHeight', { configurable: true, get: () => height })
    log.scrollTop = 360
    const loadButton = wrapper.get('.chat-load-older').element as HTMLButtonElement
    loadButton.focus()
    expect(document.activeElement).toBe(loadButton)

    const loaded = await (wrapper.vm as unknown as ListHandle).loadOlderPreservingPosition(async () => {
      height = 1_420
      await wrapper.setProps({ messages: [message(1), message(2), message(3), message(4)] })
      return true
    })

    expect(loaded).toBe(true)
    expect(log.scrollTop).toBe(780)
    expect(document.activeElement).toBe(loadButton)
    wrapper.unmount()
  })

  it('uses smooth scrolling normally and an immediate scroll for reduced motion', async () => {
    const wrapper = mount(ChatMessageList, {
      props: { messages: [message(1)], admin: false },
    })
    await nextTick()
    const log = wrapper.get('.chat-message-log').element as HTMLElement
    Object.defineProperty(log, 'scrollHeight', { configurable: true, value: 900 })
    const scrollTo = vi.fn()
    Object.defineProperty(log, 'scrollTo', { configurable: true, value: scrollTo })

    ;(wrapper.vm as unknown as ListHandle).scrollToBottom(true)
    expect(scrollTo).toHaveBeenCalledWith({ top: 900, behavior: 'smooth' })

    vi.spyOn(window, 'matchMedia').mockReturnValue({ matches: true } as MediaQueryList)
    log.scrollTop = 0
    ;(wrapper.vm as unknown as ListHandle).scrollToBottom(true)
    expect(log.scrollTop).toBe(900)
    expect(scrollTo).toHaveBeenCalledTimes(1)
  })

  it('offers a keyboard-sized new-message control without forcing the log to move', async () => {
    const wrapper = mount(ChatMessageList, {
      props: { messages: [message(1)], admin: false, newMessageCount: 3 },
    })

    const button = wrapper.get('.chat-new-message-button')
    expect(button.text()).toContain('有 3 条新消息')
    await button.trigger('click')
    expect(wrapper.emitted('jump-latest')).toHaveLength(1)
  })

  it('disables unselected controls at the limit and never selects deleted messages', async () => {
    const wrapper = mount(ChatMessageList, {
      props: {
        messages: [
          message(1),
          message(2),
          { ...message(3), status: 'deleted', body: null },
        ],
        admin: true,
        selectedIds: [1],
        selectionAtLimit: true,
      },
    })

    const checkboxes = wrapper.findAll<HTMLInputElement>('input[type="checkbox"]')
    expect(checkboxes).toHaveLength(2)
    expect(checkboxes[0]?.element.disabled).toBe(false)
    expect(checkboxes[1]?.element.disabled).toBe(true)
    expect(wrapper.findAll('.chat-message')[2]?.find('input[type="checkbox"]').exists()).toBe(false)

    await checkboxes[0]?.setValue(false)
    expect(wrapper.emitted('selection-change')?.[0]).toEqual([expect.objectContaining({ id: 1 }), false])
  })
})
