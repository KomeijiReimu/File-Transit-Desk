import type { ChatCapabilities } from '@/types'

export interface ChatDraftMetrics {
  body: string
  characters: number
  bodyBytes: number
  requestBytes: number
  empty: boolean
  unsafeControl: boolean
  characterExceeded: boolean
  bodyBytesExceeded: boolean
  requestBytesExceeded: boolean
  errors: string[]
  sendable: boolean
}

const encoder = new TextEncoder()

export function prepareChatBody(value: string) {
  return value.replace(/\r\n?/g, '\n').trim()
}

export function containsUnsafeChatControl(value: string) {
  for (const character of value) {
    const code = character.codePointAt(0) || 0
    if (code === 0x09 || code === 0x0a) continue
    if (code === 0 || code < 0x20 || (code >= 0x7f && code <= 0x9f)) return true
    if (code === 0xfeff || code === 0x061c || code === 0x200e || code === 0x200f) return true
    if ((code >= 0x202a && code <= 0x202e) || (code >= 0x2066 && code <= 0x2069)) return true
  }
  return false
}

export function measureChatDraft(value: string, capabilities: ChatCapabilities | null): ChatDraftMetrics {
  const body = prepareChatBody(value)
  const characters = [...body].length
  const bodyBytes = encoder.encode(body).byteLength
  const requestBytes = encoder.encode(JSON.stringify({ body })).byteLength
  const empty = body.length === 0
  const unsafeControl = containsUnsafeChatControl(body)
  const characterExceeded = Boolean(capabilities && characters > capabilities.maxMessageChars)
  const bodyBytesExceeded = Boolean(capabilities && bodyBytes > capabilities.maxMessageBytes)
  const requestBytesExceeded = Boolean(capabilities && requestBytes > capabilities.maxRequestBytes)
  const errors: string[] = []
  if (empty) errors.push('请输入消息内容。')
  if (unsafeControl) errors.push('消息包含不安全的控制字符。')
  if (characterExceeded) errors.push(`字符数超过上限 ${capabilities?.maxMessageChars}。`)
  if (bodyBytesExceeded) errors.push(`正文大小超过上限 ${capabilities?.maxMessageBytes} 字节。`)
  if (requestBytesExceeded) errors.push(`JSON 请求大小超过上限 ${capabilities?.maxRequestBytes} 字节。`)
  return {
    body,
    characters,
    bodyBytes,
    requestBytes,
    empty,
    unsafeControl,
    characterExceeded,
    bodyBytesExceeded,
    requestBytesExceeded,
    errors,
    sendable: Boolean(capabilities) && errors.length === 0,
  }
}
