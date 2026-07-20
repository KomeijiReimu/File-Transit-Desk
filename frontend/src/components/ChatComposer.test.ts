import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import ChatComposer from '@/components/ChatComposer.vue'
import type { ChatCapabilities } from '@/types'

function capabilities(overrides: Partial<ChatCapabilities> = {}): ChatCapabilities {
  return {
    maxMessageChars: 20,
    maxMessageBytes: 80,
    maxRequestBytes: 120,
    withdrawWindowSeconds: 300,
    historyDefaultLimit: 50,
    historyMaxLimit: 100,
    changesDefaultLimit: 50,
    changesMaxLimit: 500,
    ...overrides,
  }
}

describe('ChatComposer', () => {
  it('sends a trimmed draft with Enter and leaves Shift+Enter for a newline', async () => {
    const wrapper = mount(ChatComposer, {
      props: { modelValue: '  hello  ', capabilities: capabilities() },
    })
    const textarea = wrapper.get('textarea')

    await textarea.trigger('keydown', { key: 'Enter', shiftKey: true })
    expect(wrapper.emitted('send')).toBeUndefined()

    await textarea.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('send')).toEqual([['hello']])
  })

  it('does not send while an IME composition is active', async () => {
    const wrapper = mount(ChatComposer, {
      props: { modelValue: '输入中', capabilities: capabilities() },
    })
    const textarea = wrapper.get('textarea')

    await textarea.trigger('compositionstart')
    await textarea.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('send')).toBeUndefined()

    await textarea.trigger('compositionend')
    await textarea.trigger('keydown', { key: 'Enter' })
    expect(wrapper.emitted('send')).toEqual([['输入中']])
  })

  it('disables sending and explains each server-provided limit without disabling draft editing', async () => {
    const wrapper = mount(ChatComposer, {
      props: {
        modelValue: '😀😀',
        capabilities: capabilities({ maxMessageChars: 1, maxMessageBytes: 7, maxRequestBytes: 18 }),
      },
    })

    expect(wrapper.get('textarea').attributes('disabled')).toBeUndefined()
    expect(wrapper.get('.chat-send-button').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('字符数超过上限 1')
    expect(wrapper.text()).toContain('正文大小超过上限 7 字节')
    expect(wrapper.text()).toContain('JSON 请求大小超过上限 18 字节')
  })

  it('keeps the draft editable but blocks send when capabilities fail, and exposes retry', async () => {
    const wrapper = mount(ChatComposer, {
      props: {
        modelValue: '草稿不能丢',
        capabilities: null,
        capabilitiesError: '无法读取聊天发送限制，请重试。',
      },
    })

    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('草稿不能丢')
    expect(wrapper.get('textarea').attributes('disabled')).toBeUndefined()
    expect(wrapper.get('.chat-send-button').attributes('disabled')).toBeDefined()
    await wrapper.get('.chat-capability-error button').trigger('click')
    expect(wrapper.emitted('retry-capabilities')).toHaveLength(1)
  })

  it('keeps server errors next to an unchanged draft and prevents duplicate sends while busy', async () => {
    const wrapper = mount(ChatComposer, {
      props: {
        modelValue: '仍然保留',
        capabilities: capabilities(),
        sending: true,
        error: '发送消息过于频繁，请在 3 秒后重试。',
      },
    })

    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).toBe('仍然保留')
    expect(wrapper.text()).toContain('发送消息过于频繁')
    await wrapper.get('form').trigger('submit')
    expect(wrapper.emitted('send')).toBeUndefined()
  })
})
