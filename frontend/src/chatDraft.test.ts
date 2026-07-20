import { describe, expect, it } from 'vitest'
import { containsUnsafeChatControl, measureChatDraft, prepareChatBody } from '@/chatDraft'
import type { ChatCapabilities } from '@/types'

const encoder = new TextEncoder()

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

describe('chat draft measurement', () => {
  it('normalizes line endings, trims once and counts Unicode code points', () => {
    const result = measureChatDraft(' \r\n😀a\r ', capabilities())

    expect(result.body).toBe('😀a')
    expect(result.characters).toBe(2)
    expect(result.bodyBytes).toBe(5)
    expect(result.requestBytes).toBe(encoder.encode(JSON.stringify({ body: '😀a' })).byteLength)
    expect(result.sendable).toBe(true)
  })

  it('enforces character, decoded UTF-8 and raw JSON request limits independently', () => {
    expect(measureChatDraft('ab', capabilities({ maxMessageChars: 1 })).characterExceeded).toBe(true)
    expect(measureChatDraft('😀', capabilities({ maxMessageBytes: 3 })).bodyBytesExceeded).toBe(true)

    const escaped = 'a"\nb'
    const rawBytes = encoder.encode(JSON.stringify({ body: escaped })).byteLength
    const result = measureChatDraft(escaped, capabilities({ maxRequestBytes: rawBytes - 1 }))
    expect(result.bodyBytesExceeded).toBe(false)
    expect(result.requestBytesExceeded).toBe(true)
    expect(result.sendable).toBe(false)
  })

  it('accepts exact boundaries and does not guess limits before capabilities load', () => {
    const body = '😀'
    const requestBytes = encoder.encode(JSON.stringify({ body })).byteLength
    expect(measureChatDraft(body, capabilities({
      maxMessageChars: 1,
      maxMessageBytes: 4,
      maxRequestBytes: requestBytes,
    })).sendable).toBe(true)

    const unknown = measureChatDraft(body, null)
    expect(unknown.characterExceeded).toBe(false)
    expect(unknown.bodyBytesExceeded).toBe(false)
    expect(unknown.requestBytesExceeded).toBe(false)
    expect(unknown.sendable).toBe(false)
  })

  it('rejects the same dangerous control families as the server while allowing tabs and newlines', () => {
    expect(containsUnsafeChatControl('line one\n\tline two')).toBe(false)
    expect(containsUnsafeChatControl('x\u0000y')).toBe(true)
    expect(containsUnsafeChatControl('x\u202ey')).toBe(true)
    expect(containsUnsafeChatControl('x\ufeffy')).toBe(true)
    expect(measureChatDraft('x\u202ey', capabilities()).errors).toContain('消息包含不安全的控制字符。')
  })

  it('treats whitespace-only drafts as empty', () => {
    expect(prepareChatBody(' \n\t ')).toBe('')
    expect(measureChatDraft(' \n\t ', capabilities())).toMatchObject({ empty: true, sendable: false })
  })
})
