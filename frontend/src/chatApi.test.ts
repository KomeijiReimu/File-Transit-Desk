import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('@/router', () => ({
  default: {
    currentRoute: { value: { meta: {}, name: 'chat', fullPath: '/chat' } },
    replace: vi.fn(),
  },
}))

import { api } from '@/api'
import { setCurrentSessionBinding } from '@/authEpoch'

function response(body: unknown, status = 200) {
  return Promise.resolve(new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  }))
}

afterEach(() => {
  vi.unstubAllGlobals()
  setCurrentSessionBinding(undefined)
})

describe('chat API wiring', () => {
  it('selects ordinary and administrator projections and wires every mutation exactly', async () => {
    const binding = 'abababababababababababababababab'
    setCurrentSessionBinding(binding)
    const fetchMock = vi.fn((url: string, _options?: RequestInit) => {
      if (url === '/api/chat/capabilities') return response({ maxMessageChars: 10 })
      if (url.includes('/changes')) return response({ changes: [], generation: 2, nextAfterSeq: 7, hasMore: false, latestChangeSeq: 7 })
      if (url.includes('/messages') && !url.endsWith('/withdraw')) {
        if (url === '/api/chat/messages') return response({ message: { id: 9 }, eventSeq: 11 }, 201)
        if (url === '/api/admin/chat/messages/9') return response({ message: { id: 9 }, eventSeq: 13 })
        return response({ messages: [], generation: 2, latestChangeSeq: 7, hasMore: false, nextBeforeId: null })
      }
      if (url.endsWith('/withdraw')) return response({ message: { id: 9 }, eventSeq: 12 })
      throw new Error(`unexpected URL: ${url}`)
    })
    vi.stubGlobal('fetch', fetchMock)

    await api.chatCapabilities()
    await api.chatMessages(false, { beforeId: 20, limit: 50 })
    await api.chatMessages(true, { limit: 50 })
    await api.chatChanges(false, { afterSeq: 7, generation: 2, limit: 500 })
    await api.chatChanges(true, { afterSeq: 7, generation: 2, limit: 500 })
    await api.createChatMessage('纯文本')
    await api.withdrawChatMessage(9)
    await api.deleteChatMessage(9)

    expect(fetchMock.mock.calls.map(([url]) => url)).toEqual([
      '/api/chat/capabilities',
      '/api/chat/messages?beforeId=20&limit=50',
      '/api/admin/chat/messages?limit=50',
      '/api/chat/changes?afterSeq=7&generation=2&limit=500',
      '/api/admin/chat/changes?afterSeq=7&generation=2&limit=500',
      '/api/chat/messages',
      '/api/chat/messages/9/withdraw',
      '/api/admin/chat/messages/9',
    ])
    expect(fetchMock.mock.calls.some(([url]) => url === '/api/auth/heartbeat')).toBe(false)

    const createOptions = fetchMock.mock.calls[5]?.[1] as unknown as RequestInit
    expect(createOptions).toMatchObject({ method: 'POST', credentials: 'include', body: JSON.stringify({ body: '纯文本' }) })
    expect(new Headers(createOptions.headers).get('Content-Type')).toBe('application/json')
    expect(new Headers(createOptions.headers).get('X-Session-Binding')).toBe(binding)
    expect(fetchMock.mock.calls[6]?.[1]).toMatchObject({ method: 'POST', credentials: 'include' })
    expect(fetchMock.mock.calls[7]?.[1]).toMatchObject({ method: 'DELETE', credentials: 'include' })
  })
})
