import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api'
import { chatErrorMessage } from '@/chatErrors'

describe('chat error copy', () => {
  it('maps rate limits and Retry-After to a concrete wait', () => {
    expect(chatErrorMessage(
      new ApiError('limited', 429, undefined, 'chat_send_rate_limited', 12),
      'send',
    )).toBe('发送消息过于频繁，请在 12 秒后重试。')
  })

  it.each([
    ['chat_withdraw_expired', '这条消息已超过可撤回时间。'],
    ['chat_withdraw_forbidden', '你无权撤回这条消息。'],
    ['chat_state_conflict', '消息状态已经变化，请同步后再试。'],
    ['chat_request_too_large', '消息请求超过服务器允许的大小，请缩短内容。'],
    ['chat_cursor_reset_required', '聊天记录状态已变化，正在重新加载。'],
  ])('maps %s to specific Chinese feedback', (code, expected) => {
    expect(chatErrorMessage(new ApiError('server message', 409, undefined, code), 'withdraw')).toBe(expected)
  })

  it('keeps sync network failures non-blocking and explicit', () => {
    expect(chatErrorMessage(new ApiError('offline', 0), 'sync')).toBe('消息更新暂时中断，正在自动重试。')
  })
})
