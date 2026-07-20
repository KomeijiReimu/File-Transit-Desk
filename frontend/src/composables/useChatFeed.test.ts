import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '@/api'
import {
  chatWatermarkIsNewer,
  useChatFeed,
  type ChatApiClient,
  type ChatMessageWatermark,
} from '@/composables/useChatFeed'
import type {
  ChatBatchDeleteResponse,
  ChatCapabilities,
  ChatChange,
  ChatChangesResponse,
  ChatClearResponse,
  ChatHistoryResponse,
  ChatMessage,
  ChatMutationResponse,
} from '@/types'

const timestamp = '2026-07-20T08:00:00Z'

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
    createdAt: timestamp,
    withdrawnAt: null,
    deletedAt: null,
    canWithdraw: false,
    withdrawUntil: null,
    sourceIP: `192.0.2.${id}`,
    ...overrides,
  }
}

function history(messages: ChatMessage[] = [], overrides: Partial<ChatHistoryResponse> = {}): ChatHistoryResponse {
  return {
    messages,
    nextBeforeId: null,
    hasMore: false,
    latestChangeSeq: 4,
    generation: 1,
    ...overrides,
  }
}

function change(seq: number, entry: ChatMessage, kind: ChatChange['kind'] = 'create'): ChatChange {
  return { seq, kind, createdAt: timestamp, message: entry }
}

function changes(items: ChatChange[] = [], overrides: Partial<ChatChangesResponse> = {}): ChatChangesResponse {
  const lastSeq = items.length ? items[items.length - 1]?.seq || 4 : 4
  return {
    changes: items,
    nextAfterSeq: lastSeq,
    hasMore: false,
    latestChangeSeq: lastSeq,
    generation: 1,
    ...overrides,
  }
}

function mutation(entry: ChatMessage, eventSeq = 999): ChatMutationResponse {
  return { message: entry, eventSeq }
}

function makeClient(overrides: Partial<ChatApiClient> = {}) {
  return {
    chatCapabilities: vi.fn(async () => capabilities()),
    chatMessages: vi.fn(async () => history()),
    chatChanges: vi.fn(async () => changes()),
    createChatMessage: vi.fn(async (body: string) => mutation(message(90, { body, isMine: true }))),
    withdrawChatMessage: vi.fn(async (id: number) => mutation(message(id, { body: null, status: 'withdrawn', isMine: true }))),
    deleteChatMessage: vi.fn(async (id: number) => mutation(message(id, { body: null, status: 'deleted' }))),
    batchDeleteChatMessages: vi.fn(async (ids: number[]): Promise<ChatBatchDeleteResponse> => ({
      deletedCount: ids.length,
      mutations: ids.map((id, index) => mutation(message(id, { body: null, status: 'deleted' }), 1_000 + index)),
    })),
    clearChatMessages: vi.fn(async (): Promise<ChatClearResponse> => ({
      clearedCount: 0,
      generation: 2,
      latestChangeSeq: 4,
    })),
    ...overrides,
  } as ChatApiClient & Record<keyof ChatApiClient, ReturnType<typeof vi.fn>>
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

async function flushMicrotasks() {
  for (let index = 0; index < 8; index += 1) await Promise.resolve()
}

describe('useChatFeed', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('orders projection watermarks by seq and then changes > history > mutation', () => {
    const mark = (seq: number, source: ChatMessageWatermark['source'], priority: number): ChatMessageWatermark => ({
      generation: 1,
      seq,
      source,
      priority,
    })
    expect(chatWatermarkIsNewer(mark(9, 'changes', 2), mark(10, 'mutation', 0))).toBe(true)
    expect(chatWatermarkIsNewer(mark(10, 'mutation', 0), mark(10, 'history', 1))).toBe(true)
    expect(chatWatermarkIsNewer(mark(10, 'history', 1), mark(10, 'changes', 2))).toBe(true)
    expect(chatWatermarkIsNewer(mark(10, 'changes', 2), mark(10, 'mutation', 0))).toBe(false)
    expect(chatWatermarkIsNewer(mark(10, 'changes', 2), mark(10, 'changes', 2))).toBe(false)
    expect(chatWatermarkIsNewer(mark(10, 'changes', 2), { ...mark(11, 'changes', 2), generation: 2 })).toBe(false)
  })

  it('loads the capability-sized latest history, keeps ordinary IPs and strips withdrawn originals', async () => {
    const leaked = {
      ...message(2, { status: 'withdrawn', body: '仅管理员原文', sourceIP: '198.51.100.2' }),
      authorKey: 'hidden-author-key',
      deletedBy: 'hidden-admin-key',
    } as ChatMessage
    const client = makeClient({
      chatMessages: vi.fn(async () => history([
        leaked,
        message(1),
        message(2, { status: 'withdrawn', body: '最后版本', sourceIP: '198.51.100.3' }),
      ], { nextBeforeId: 1, hasMore: true, latestChangeSeq: 12 })),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })

    await feed.start()

    expect(client.chatMessages).toHaveBeenCalledWith(false, { limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value.map((entry) => entry.id)).toEqual([1, 2])
    expect(feed.messages.value[1]).toMatchObject({ body: null, status: 'withdrawn' })
    expect(feed.messages.value[1]).toMatchObject({ sourceIP: '198.51.100.2' })
    expect(feed.messages.value[1]).not.toHaveProperty('authorKey')
    expect(feed.messages.value[1]).not.toHaveProperty('deletedBy')
    expect(feed.cursor.value).toBe(12)
    expect(feed.hasMore.value).toBe(true)
    feed.dispose()
  })

  it('uses the same state machine for the admin projection while retaining withdrawn originals and IPs', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([
        message(1, { status: 'withdrawn', body: '撤回原文', sourceIP: '203.0.113.9' }),
        message(2, { status: 'deleted', body: '绝不能保留', sourceIP: '203.0.113.10' }),
      ])),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })

    await feed.start()

    expect(client.chatMessages).toHaveBeenCalledWith(true, { limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value[0]).toMatchObject({ body: '撤回原文', sourceIP: '203.0.113.9' })
    expect(feed.messages.value[1]).toMatchObject({ body: null, status: 'deleted', sourceIP: '203.0.113.10' })
    feed.dispose()
  })

  it('stays empty while inactive, then rebinds from admin to the ordinary endpoints with fresh capabilities', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([
          message(1, { status: 'withdrawn', body: '管理员原文', sourceIP: '203.0.113.1' }),
        ], { generation: 1, latestChangeSeq: 5 }))
        .mockResolvedValueOnce(history([
          message(2, { status: 'withdrawn', body: '普通投影不应保留', sourceIP: '203.0.113.2' }),
        ], { generation: 2, latestChangeSeq: 20 })),
      chatChanges: vi.fn(async () => changes([], {
        generation: 2,
        nextAfterSeq: 20,
        latestChangeSeq: 20,
      })),
    })
    const feed = useChatFeed({
      admin: true,
      active: true,
      subjectKey: 'authenticated|admin|binding-a',
      client,
      random: () => 0.5,
    })
    await feed.start()
    expect(feed.messages.value[0]).toMatchObject({ body: '管理员原文', sourceIP: '203.0.113.1' })

    await feed.rebind({ subjectKey: 'unknown|-|-', admin: false, active: false })
    expect(feed.messages.value).toEqual([])
    expect(feed.capabilities.value).toBeNull()
    expect(feed.generation.value).toBeNull()
    await vi.advanceTimersByTimeAsync(20_000)
    expect(client.chatMessages).toHaveBeenCalledTimes(1)
    expect(client.chatChanges).not.toHaveBeenCalled()

    await feed.rebind({ subjectKey: 'authenticated|user|binding-b', admin: false, active: true })
    expect(client.chatCapabilities).toHaveBeenCalledTimes(2)
    expect(client.chatMessages).toHaveBeenLastCalledWith(false, { limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value[0]).toMatchObject({ status: 'withdrawn', body: null })
    expect(feed.messages.value[0]).toMatchObject({ sourceIP: '203.0.113.2' })

    await feed.syncNow()
    expect(client.chatChanges).toHaveBeenLastCalledWith(false, { afterSeq: 20, generation: 2, limit: 500 }, expect.any(AbortSignal))
    feed.dispose()
  })

  it('does not issue any chat request before an inactive subject becomes authenticated', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1)], { generation: 2, latestChangeSeq: 10 })),
    })
    const feed = useChatFeed({
      admin: false,
      active: false,
      subjectKey: 'unknown|-|-',
      client,
      random: () => 0.5,
    })

    await expect(feed.start()).resolves.toBe(false)
    await vi.advanceTimersByTimeAsync(20_000)
    window.dispatchEvent(new Event('focus'))
    expect(client.chatCapabilities).not.toHaveBeenCalled()
    expect(client.chatMessages).not.toHaveBeenCalled()
    expect(client.chatChanges).not.toHaveBeenCalled()
    await expect(feed.send('不应发送')).rejects.toMatchObject({ code: 'session_subject_changed' })
    expect(client.createChatMessage).not.toHaveBeenCalled()

    await feed.rebind({ subjectKey: 'authenticated|user|binding-b', admin: false, active: true })
    expect(client.chatMessages).toHaveBeenCalledWith(false, { limit: 50 }, expect.any(AbortSignal))
    feed.dispose()
  })

  it('does not let a stale capability completion continue the previous subject initialization', async () => {
    const oldCapabilities = deferred<ChatCapabilities>()
    const client = makeClient({
      chatCapabilities: vi.fn()
        .mockImplementationOnce(() => oldCapabilities.promise)
        .mockResolvedValueOnce(capabilities()),
      chatMessages: vi.fn(async () => history([message(2)], { generation: 2, latestChangeSeq: 10 })),
    })
    const feed = useChatFeed({
      admin: true,
      active: true,
      subjectKey: 'authenticated|admin|binding-a',
      client,
      random: () => 0.5,
    })
    const oldStart = feed.start()
    await flushMicrotasks()

    await feed.rebind({ subjectKey: 'authenticated|user|binding-b', admin: false, active: true })
    expect(client.chatMessages).toHaveBeenCalledTimes(1)
    expect(client.chatMessages).toHaveBeenCalledWith(false, { limit: 50 }, expect.any(AbortSignal))

    oldCapabilities.resolve(capabilities())
    await oldStart
    expect(client.chatMessages).toHaveBeenCalledTimes(1)
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 2 })])
    feed.dispose()
  })

  it('switches an ordinary subject to the admin history and changes endpoints', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { generation: 1, latestChangeSeq: 5 }))
        .mockResolvedValueOnce(history([
          message(2, { status: 'withdrawn', body: '新管理员原文', sourceIP: '203.0.113.22' }),
        ], { generation: 2, latestChangeSeq: 20 })),
      chatChanges: vi.fn(async () => changes([], { generation: 2, nextAfterSeq: 20, latestChangeSeq: 20 })),
    })
    const feed = useChatFeed({
      admin: false,
      active: true,
      subjectKey: 'authenticated|user|binding-a',
      client,
      random: () => 0.5,
    })
    await feed.start()

    await feed.rebind({ subjectKey: 'authenticated|admin|binding-b', admin: true, active: true })
    expect(client.chatMessages).toHaveBeenLastCalledWith(true, { limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value[0]).toMatchObject({ body: '新管理员原文', sourceIP: '203.0.113.22' })
    await feed.syncNow()
    expect(client.chatChanges).toHaveBeenLastCalledWith(true, { afterSeq: 20, generation: 2, limit: 500 }, expect.any(AbortSignal))
    feed.dispose()
  })

  it('does not restart for the same subject binding and role heartbeat update', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1)], { latestChangeSeq: 5 })),
    })
    const sameBinding = { subjectKey: 'authenticated|admin|same-binding', admin: true, active: true }
    const feed = useChatFeed({ ...sameBinding, client, random: () => 0.5 })
    await feed.start()

    await expect(feed.rebind(sameBinding)).resolves.toBe(true)
    expect(client.chatCapabilities).toHaveBeenCalledTimes(1)
    expect(client.chatMessages).toHaveBeenCalledTimes(1)
    expect(feed.messages.value).toHaveLength(1)
    feed.dispose()
  })

  it('rejects in-flight admin history, changes and mutation projections after rebind', async () => {
    const oldHistory = deferred<ChatHistoryResponse>()
    const oldChanges = deferred<ChatChangesResponse>()
    const oldMutation = deferred<ChatMutationResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([
          message(1, { status: 'withdrawn', body: '旧管理员原文', sourceIP: '198.51.100.1' }),
        ], { generation: 1, latestChangeSeq: 5 }))
        .mockImplementationOnce(() => oldHistory.promise)
        .mockResolvedValueOnce(history([
          message(20, { body: '管理员 B 投影', sourceIP: '198.51.100.20' }),
        ], { generation: 2, latestChangeSeq: 20 })),
      chatChanges: vi.fn(() => oldChanges.promise),
      deleteChatMessage: vi.fn(() => oldMutation.promise),
    })
    const feed = useChatFeed({
      admin: true,
      active: true,
      subjectKey: 'authenticated|admin|binding-a',
      client,
      random: () => 0.5,
    })
    await feed.start()

    const historyRequest = feed.retryInitial()
    const changesRequest = feed.syncNow()
    const mutationRequest = feed.remove(1)
    await flushMicrotasks()

    await feed.rebind({ subjectKey: 'authenticated|admin|binding-b', admin: true, active: true })
    expect(feed.messages.value).toEqual([expect.objectContaining({
      id: 20,
      body: '管理员 B 投影',
      sourceIP: '198.51.100.20',
    })])

    oldHistory.resolve(history([
      message(2, { status: 'withdrawn', body: '迟到 history 原文', sourceIP: '198.51.100.2' }),
    ], { generation: 1, latestChangeSeq: 6 }))
    oldChanges.resolve(changes([
      change(6, message(3, { body: '迟到 changes 原文', sourceIP: '198.51.100.3' })),
    ], { generation: 1, nextAfterSeq: 6, latestChangeSeq: 6 }))
    oldMutation.resolve(mutation(message(4, { body: '迟到 mutation 原文', sourceIP: '198.51.100.4' }), 7))
    await Promise.all([historyRequest, changesRequest, mutationRequest])

    expect(feed.messages.value).toEqual([expect.objectContaining({
      id: 20,
      body: '管理员 B 投影',
      sourceIP: '198.51.100.20',
    })])
    expect(feed.messages.value.some((entry) => entry.body?.includes('迟到'))).toBe(false)
    feed.dispose()
  })

  it('clears an admin projection on endpoint 403, revalidates once, then accepts an ordinary rebind', async () => {
    const revalidation = deferred<void>()
    const revalidateAuth = vi.fn(() => revalidation.promise)
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([
          message(1, { status: 'withdrawn', body: '敏感原文', sourceIP: '192.0.2.1' }),
        ], { generation: 1, latestChangeSeq: 5 }))
        .mockResolvedValueOnce(history([
          message(2, { status: 'withdrawn', body: '不得显示', sourceIP: '192.0.2.2' }),
        ], { generation: 2, latestChangeSeq: 20 })),
      chatChanges: vi.fn()
        .mockRejectedValueOnce(new ApiError('forbidden', 403))
        .mockResolvedValueOnce(changes([], { generation: 2, nextAfterSeq: 20, latestChangeSeq: 20 })),
      deleteChatMessage: vi.fn().mockRejectedValueOnce(new ApiError('forbidden', 403)),
    })
    const feed = useChatFeed({
      admin: true,
      active: true,
      subjectKey: 'authenticated|admin|binding-a',
      revalidateAuth,
      client,
      random: () => 0.5,
    })
    await feed.start()
    feed.toggleSelection(1, true)

    const forbiddenSync = feed.syncNow()
    const forbiddenDelete = feed.remove(1)
    await forbiddenSync
    await expect(forbiddenDelete).rejects.toMatchObject({ status: 403 })
    expect(feed.messages.value).toEqual([])
    expect(feed.capabilities.value).toBeNull()
    expect(feed.generation.value).toBeNull()
    expect(feed.selectedIds.value).toEqual([])
    expect(revalidateAuth).toHaveBeenCalledTimes(1)

    await feed.rebind({ subjectKey: 'authenticated|user|binding-b', admin: false, active: true })
    expect(client.chatMessages).toHaveBeenLastCalledWith(false, { limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value[0]).toMatchObject({ status: 'withdrawn', body: null })
    expect(feed.messages.value[0]).toMatchObject({ sourceIP: '192.0.2.2' })
    await feed.syncNow()
    expect(client.chatChanges).toHaveBeenLastCalledWith(false, { afterSeq: 20, generation: 2, limit: 500 }, expect.any(AbortSignal))

    revalidation.resolve()
    await flushMicrotasks()
    expect(revalidateAuth).toHaveBeenCalledTimes(1)
    feed.dispose()
  })

  it('clears session_subject_changed without starting a second auth revalidation', async () => {
    const revalidateAuth = vi.fn()
    const onProjectionInvalidated = vi.fn()
    const client = makeClient({
      chatMessages: vi.fn(async () => history([
        message(1, { status: 'withdrawn', body: '敏感原文', sourceIP: '192.0.2.10' }),
      ], { latestChangeSeq: 5 })),
      chatChanges: vi.fn().mockRejectedValueOnce(new ApiError('subject changed', 409, undefined, 'session_subject_changed')),
    })
    const feed = useChatFeed({
      admin: true,
      active: true,
      subjectKey: 'authenticated|admin|binding-a',
      revalidateAuth,
      onProjectionInvalidated,
      client,
      random: () => 0.5,
    })
    await feed.start()
    feed.toggleSelection(1, true)

    await feed.syncNow()
    expect(feed.messages.value).toEqual([])
    expect(feed.generation.value).toBeNull()
    expect(feed.selectedIds.value).toEqual([])
    expect(onProjectionInvalidated).toHaveBeenCalledTimes(1)
    expect(revalidateAuth).not.toHaveBeenCalled()
    feed.dispose()
  })

  it('prepends older keyset pages by id without changing the global cursor', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(3), message(4)], { nextBeforeId: 3, hasMore: true, latestChangeSeq: 20 }))
        .mockResolvedValueOnce(history([message(1), message(2), message(3)], { generation: 1, latestChangeSeq: 99 })),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    await expect(feed.loadOlder()).resolves.toBe(true)

    expect(client.chatMessages).toHaveBeenLastCalledWith(false, { beforeId: 3, limit: 50 }, expect.any(AbortSignal))
    expect(feed.messages.value.map((entry) => entry.id)).toEqual([1, 2, 3, 4])
    expect(feed.cursor.value).toBe(20)
    feed.dispose()
  })

  it('does not let an old active history response overwrite a newer delete change', async () => {
    const oldHistory = deferred<ChatHistoryResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1, { body: '最初正文' })], { latestChangeSeq: 5 }))
        .mockImplementationOnce(() => oldHistory.promise),
      chatChanges: vi.fn()
        .mockResolvedValueOnce(changes([
          change(6, message(1, { body: null, status: 'deleted' }), 'delete'),
        ], { nextAfterSeq: 6, latestChangeSeq: 6 }))
        .mockResolvedValueOnce(changes([], { nextAfterSeq: 6, latestChangeSeq: 6 })),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    const historyRequest = feed.retryInitial()
    await flushMicrotasks()
    await feed.syncNow()
    expect(feed.messages.value[0]).toMatchObject({ status: 'deleted', body: null })

    oldHistory.resolve(history([message(1, { body: '迟到的 active 正文' })], { latestChangeSeq: 6 }))
    await historyRequest
    expect(feed.messages.value[0]).toMatchObject({ status: 'deleted', body: null })
    expect(feed.cursor.value).toBe(6)

    await feed.syncNow()
    expect(feed.messages.value[0]).toMatchObject({ status: 'deleted', body: null })
    feed.dispose()
  })

  it('follows paged changes in seq order, upserts by id and advances only from nextAfterSeq', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1)], { latestChangeSeq: 4 })),
      chatChanges: vi.fn()
        .mockResolvedValueOnce(changes([
          change(6, message(2, { body: '第二版' })),
          change(5, message(2, { body: '第一版' })),
        ], { nextAfterSeq: 6, hasMore: true, latestChangeSeq: 900 }))
        .mockResolvedValueOnce(changes([
          change(7, message(1, { body: null, status: 'deleted' }), 'delete'),
        ], { nextAfterSeq: 7, latestChangeSeq: 901 })),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    await feed.syncNow()

    expect(client.chatChanges).toHaveBeenNthCalledWith(1, false, { afterSeq: 4, generation: 1, limit: 500 }, expect.any(AbortSignal))
    expect(client.chatChanges).toHaveBeenNthCalledWith(2, false, { afterSeq: 6, generation: 1, limit: 500 }, expect.any(AbortSignal))
    expect(feed.cursor.value).toBe(7)
    expect(feed.messages.value).toHaveLength(2)
    expect(feed.messages.value[0]).toMatchObject({ id: 1, status: 'deleted', body: null })
    expect(feed.messages.value[1]).toMatchObject({ id: 2, body: '第二版' })
    feed.dispose()
  })

  it('bounds each catch-up pass and continues a still-paged stream on a short follow-up timer', async () => {
    const client = makeClient({
      chatChanges: vi.fn()
        .mockResolvedValueOnce(changes([change(5, message(5))], { nextAfterSeq: 5, hasMore: true }))
        .mockResolvedValueOnce(changes([change(6, message(6))], { nextAfterSeq: 6, hasMore: true }))
        .mockResolvedValueOnce(changes([change(7, message(7))], { nextAfterSeq: 7, hasMore: false })),
    })
    const feed = useChatFeed({
      admin: false,
      client,
      random: () => 0.5,
      maxChangePagesPerSync: 2,
    })
    await feed.start()

    await feed.syncNow()
    expect(client.chatChanges).toHaveBeenCalledTimes(2)
    expect(feed.cursor.value).toBe(6)

    await vi.advanceTimersByTimeAsync(50)
    expect(client.chatChanges).toHaveBeenCalledTimes(3)
    expect(feed.cursor.value).toBe(7)
    feed.dispose()
  })

  it('upserts confirmed mutations immediately but never treats eventSeq as the changes cursor', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1, { isMine: true })], { latestChangeSeq: 10 })),
      createChatMessage: vi.fn(async () => mutation(message(2, { body: '已确认发送', isMine: true }), 800)),
      withdrawChatMessage: vi.fn(async () => mutation(message(1, { body: null, status: 'withdrawn', isMine: true }), 801)),
      deleteChatMessage: vi.fn(async () => mutation(message(2, { body: '服务端也不应返回', status: 'deleted' }), 802)),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()

    await feed.send('已确认发送')
    await feed.withdraw(1)
    await feed.remove(2)

    expect(feed.cursor.value).toBe(10)
    expect(feed.messages.value.find((entry) => entry.id === 1)?.status).toBe('withdrawn')
    expect(feed.messages.value.find((entry) => entry.id === 2)).toMatchObject({ status: 'deleted', body: null })
    feed.dispose()
  })

  it('applies batch-delete mutation watermarks without advancing the changes cursor', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1), message(2)], { latestChangeSeq: 10 })),
      batchDeleteChatMessages: vi.fn(async () => ({
        deletedCount: 2,
        mutations: [
          mutation(message(1, { status: 'deleted', body: null }), 11),
          mutation(message(2, { status: 'deleted', body: null }), 12),
        ],
      })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    feed.toggleSelection(2, true)

    await feed.batchDelete([1, 2])

    expect(client.batchDeleteChatMessages).toHaveBeenCalledWith([1, 2], expect.any(AbortSignal))
    expect(feed.messages.value).toEqual([
      expect.objectContaining({ id: 1, status: 'deleted', body: null }),
      expect.objectContaining({ id: 2, status: 'deleted', body: null }),
    ])
    expect(feed.selectedIds.value).toEqual([])
    expect(feed.cursor.value).toBe(10)
    expect(feed.latestChangeSeq.value).toBe(12)
    feed.dispose()
  })

  it('keeps batch selection after a failed request', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1), message(2)], { latestChangeSeq: 10 })),
      batchDeleteChatMessages: vi.fn(async () => {
        throw new ApiError('conflict', 409, undefined, 'chat_batch_delete_conflict')
      }),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    feed.toggleSelection(2, true)

    await expect(feed.batchDelete([1, 2])).rejects.toMatchObject({ code: 'chat_batch_delete_conflict' })
    expect(feed.selectedIds.value).toEqual([1, 2])
    feed.dispose()
  })

  it('ignores a late batch tombstone older than an applied change watermark', async () => {
    const batchResponse = deferred<ChatBatchDeleteResponse>()
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1)], { latestChangeSeq: 10 })),
      batchDeleteChatMessages: vi.fn(() => batchResponse.promise),
      chatChanges: vi.fn(async () => changes([
        change(12, message(1, { status: 'deleted', body: null, sourceIP: '203.0.113.12' }), 'delete'),
      ], { nextAfterSeq: 12, latestChangeSeq: 12 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    const deleting = feed.batchDelete([1])
    await flushMicrotasks()
    await feed.syncNow()

    batchResponse.resolve({
      deletedCount: 1,
      mutations: [mutation(message(1, { status: 'deleted', body: null, sourceIP: '198.51.100.11' }), 11)],
    })
    await deleting
    expect(feed.messages.value[0]).toMatchObject({ status: 'deleted', sourceIP: '203.0.113.12' })
    expect(feed.cursor.value).toBe(12)
    feed.dispose()
  })

  it('enforces the 100-message selection cap and removes messages deleted by changes', async () => {
    const entries = Array.from({ length: 102 }, (_, index) => message(index + 1))
    entries[101] = message(102, { status: 'deleted', body: null })
    const client = makeClient({
      chatMessages: vi.fn(async () => history(entries, { latestChangeSeq: 200 })),
      chatChanges: vi.fn(async () => changes([
        change(201, message(1, { status: 'deleted', body: null }), 'delete'),
      ], { nextAfterSeq: 201, latestChangeSeq: 201 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    for (let id = 1; id <= 100; id += 1) expect(feed.toggleSelection(id, true)).toBe(true)

    expect(feed.toggleSelection(101, true)).toBe(false)
    expect(feed.selectionError.value).toBe('最多选择 100 条消息。')
    expect(feed.toggleSelection(102, true)).toBe(false)
    expect(feed.selectedCount.value).toBe(100)
    expect(feed.selectionAtLimit.value).toBe(true)

    await feed.syncNow()
    expect(feed.selectedIds.value).not.toContain(1)
    expect(feed.selectedCount.value).toBe(99)
    expect(feed.selectionError.value).toBe('')
    feed.dispose()
  })

  it('clears atomically, reloads the new generation and ignores old changes and batch responses', async () => {
    const oldChanges = deferred<ChatChangesResponse>()
    const oldBatch = deferred<ChatBatchDeleteResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { generation: 1, latestChangeSeq: 10 }))
        .mockResolvedValueOnce(history([], { generation: 2, latestChangeSeq: 10 })),
      chatChanges: vi.fn(() => oldChanges.promise),
      batchDeleteChatMessages: vi.fn(() => oldBatch.promise),
      clearChatMessages: vi.fn(async () => ({ clearedCount: 1, generation: 2, latestChangeSeq: 10 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    const syncing = feed.syncNow()
    const deleting = feed.batchDelete([1])
    await flushMicrotasks()

    await feed.clearAll()
    expect(client.clearChatMessages).toHaveBeenCalledWith(1, 10, expect.any(AbortSignal))
    expect(feed.messages.value).toEqual([])
    expect(feed.generation.value).toBe(2)
    expect(feed.cursor.value).toBe(10)
    expect(feed.selectedIds.value).toEqual([])

    oldChanges.resolve(changes([
      change(11, message(9, { body: '迟到 changes', sourceIP: '198.51.100.9' })),
    ], { generation: 1, nextAfterSeq: 11, latestChangeSeq: 11 }))
    oldBatch.resolve({
      deletedCount: 1,
      mutations: [mutation(message(1, { status: 'deleted', body: null }), 11)],
    })
    await Promise.all([syncing, deleting])
    expect(feed.messages.value).toEqual([])
    expect(feed.generation.value).toBe(2)
    feed.dispose()
  })

  it('reloads on a clear CAS conflict and requires a new confirmation without retrying clear', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { generation: 3, latestChangeSeq: 20 }))
        .mockResolvedValueOnce(history([message(2, { body: '最新快照' })], { generation: 3, latestChangeSeq: 22 })),
      clearChatMessages: vi.fn(async () => {
        throw new ApiError('conflict', 409, undefined, 'chat_clear_conflict')
      }),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)

    await expect(feed.clearAll()).rejects.toMatchObject({
      code: 'chat_clear_conflict',
      message: '消息已更新，请重新确认清空。',
    })
    expect(client.clearChatMessages).toHaveBeenCalledTimes(1)
    expect(client.clearChatMessages).toHaveBeenCalledWith(3, 20, expect.any(AbortSignal))
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 2, body: '最新快照' })])
    expect(feed.generation.value).toBe(3)
    expect(feed.cursor.value).toBe(22)
    expect(feed.selectedIds.value).toEqual([])
    feed.dispose()
  })

  it('uses the latest mutation watermark for clear CAS without advancing the cursor', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([], { generation: 1, latestChangeSeq: 10 }))
        .mockResolvedValueOnce(history([], { generation: 2, latestChangeSeq: 15 })),
      createChatMessage: vi.fn(async () => mutation(message(5, { isMine: true }), 15)),
      clearChatMessages: vi.fn(async () => ({ clearedCount: 1, generation: 2, latestChangeSeq: 15 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    await feed.send('新消息')
    expect(feed.cursor.value).toBe(10)
    expect(feed.latestChangeSeq.value).toBe(15)

    await feed.clearAll()
    expect(client.clearChatMessages).toHaveBeenCalledWith(1, 15, expect.any(AbortSignal))
    expect(feed.generation.value).toBe(2)
    expect(feed.cursor.value).toBe(15)
    feed.dispose()
  })

  it('clears selection and ignores old batch and clear responses after a subject rebind', async () => {
    const oldBatch = deferred<ChatBatchDeleteResponse>()
    const oldClear = deferred<ChatClearResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { generation: 1, latestChangeSeq: 10 }))
        .mockResolvedValueOnce(history([message(20, { body: '普通新主体' })], { generation: 2, latestChangeSeq: 30 })),
      batchDeleteChatMessages: vi.fn(() => oldBatch.promise),
      clearChatMessages: vi.fn(() => oldClear.promise),
    })
    const feed = useChatFeed({ admin: true, subjectKey: 'admin-a', client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    const deleting = feed.batchDelete([1])
    const clearing = feed.clearAll()
    await flushMicrotasks()

    await feed.rebind({ subjectKey: 'user-b', admin: false, active: true })
    expect(feed.selectedIds.value).toEqual([])
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 20, body: '普通新主体' })])

    oldBatch.resolve({ deletedCount: 1, mutations: [mutation(message(1, { status: 'deleted', body: null }), 11)] })
    oldClear.resolve({ clearedCount: 1, generation: 2, latestChangeSeq: 10 })
    await Promise.all([deleting, clearing])
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 20, body: '普通新主体' })])
    expect(feed.generation.value).toBe(2)
    expect(feed.selectedIds.value).toEqual([])
    feed.dispose()
  })

  it('keeps an admin change sourceIP when the same-seq create mutation completes later', async () => {
    const createResponse = deferred<ChatMutationResponse>()
    const client = makeClient({
      chatMessages: vi.fn(async () => history([], { latestChangeSeq: 9 })),
      createChatMessage: vi.fn(() => createResponse.promise),
      chatChanges: vi.fn(async () => changes([
        change(10, message(4, { body: 'changes 权威正文', sourceIP: '203.0.113.40', isMine: true })),
      ], { nextAfterSeq: 10, latestChangeSeq: 10 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()

    const sending = feed.send('mutation 正文')
    await flushMicrotasks()
    await feed.syncNow()
    expect(feed.messages.value[0]).toMatchObject({ body: 'changes 权威正文', sourceIP: '203.0.113.40' })

    createResponse.resolve(mutation(message(4, { body: '迟到 mutation 正文', isMine: true }), 10))
    await sending
    expect(feed.messages.value[0]).toMatchObject({ body: 'changes 权威正文', sourceIP: '203.0.113.40' })
    feed.dispose()
  })

  it('does not let older mutation responses restore body or state after withdraw and delete changes', async () => {
    const withdrawResponse = deferred<ChatMutationResponse>()
    const deleteResponse = deferred<ChatMutationResponse>()
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1), message(2)], { latestChangeSeq: 9 })),
      withdrawChatMessage: vi.fn(() => withdrawResponse.promise),
      deleteChatMessage: vi.fn(() => deleteResponse.promise),
      chatChanges: vi.fn(async () => changes([
        change(12, message(1, { body: null, status: 'withdrawn', isMine: true }), 'withdraw'),
        change(13, message(2, { body: null, status: 'deleted' }), 'delete'),
      ], { nextAfterSeq: 13, latestChangeSeq: 13 })),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    const withdrawing = feed.withdraw(1)
    const deleting = feed.remove(2)
    await flushMicrotasks()
    await feed.syncNow()

    withdrawResponse.resolve(mutation(message(1, { body: '旧撤回请求正文', status: 'active', isMine: true }), 10))
    deleteResponse.resolve(mutation(message(2, { body: '旧删除请求正文', status: 'active' }), 11))
    await Promise.all([withdrawing, deleting])

    expect(feed.messages.value.find((entry) => entry.id === 1)).toMatchObject({ status: 'withdrawn', body: null })
    expect(feed.messages.value.find((entry) => entry.id === 2)).toMatchObject({ status: 'deleted', body: null })
    feed.dispose()
  })

  it('ignores old mutation and history responses after a generation reset', async () => {
    const oldHistory = deferred<ChatHistoryResponse>()
    const oldMutation = deferred<ChatMutationResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { latestChangeSeq: 5, generation: 1 }))
        .mockImplementationOnce(() => oldHistory.promise)
        .mockResolvedValueOnce(history([message(20, { body: 'generation 2 快照' })], { latestChangeSeq: 20, generation: 2 })),
      createChatMessage: vi.fn(() => oldMutation.promise),
      chatChanges: vi.fn().mockRejectedValueOnce(new ApiError('reset', 409, undefined, 'chat_cursor_reset_required')),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    const historyRequest = feed.retryInitial()
    const mutationRequest = feed.send('旧 generation mutation')
    await flushMicrotasks()
    await feed.syncNow()
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 20, body: 'generation 2 快照' })])

    oldHistory.resolve(history([message(2, { body: '迟到旧 history' })], { latestChangeSeq: 6, generation: 1 }))
    oldMutation.resolve(mutation(message(3, { body: '迟到旧 mutation', isMine: true }), 7))
    await Promise.all([historyRequest, mutationRequest])

    expect(feed.generation.value).toBe(2)
    expect(feed.cursor.value).toBe(20)
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 20, body: 'generation 2 快照' })])
    feed.dispose()
  })

  it('shows a mutation immediately and upgrades it with a same-seq authoritative change', async () => {
    const client = makeClient({
      chatMessages: vi.fn(async () => history([], { latestChangeSeq: 9 })),
      createChatMessage: vi.fn(async () => mutation(message(8, { body: '即时 mutation', isMine: true }), 10)),
      chatChanges: vi.fn(async () => changes([
        change(10, message(8, { body: '权威 changes 投影', sourceIP: '198.51.100.80', isMine: true })),
      ], { nextAfterSeq: 10, latestChangeSeq: 10 })),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()

    await feed.send('即时 mutation')
    expect(feed.messages.value[0]).toMatchObject({ body: '即时 mutation' })
    expect(feed.messages.value[0]).toMatchObject({ sourceIP: '192.0.2.8' })
    expect(feed.cursor.value).toBe(9)

    await feed.syncNow()
    expect(feed.messages.value[0]).toMatchObject({ body: '权威 changes 投影', sourceIP: '198.51.100.80' })
    expect(feed.cursor.value).toBe(10)
    feed.dispose()
  })

  it('atomically clears stale state on a reset response before replacing it with fresh history', async () => {
    const replacement = deferred<ChatHistoryResponse>()
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1, { body: '旧记录' })], { latestChangeSeq: 5, generation: 1 }))
        .mockImplementationOnce(() => replacement.promise),
      chatChanges: vi.fn().mockRejectedValueOnce(new ApiError('reset', 409, undefined, 'chat_cursor_reset_required')),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    const syncing = feed.syncNow()
    await flushMicrotasks()
    expect(feed.messages.value).toEqual([])
    expect(feed.generation.value).toBeNull()
    expect(feed.cursor.value).toBe(0)
    expect(feed.initialLoading.value).toBe(true)

    replacement.resolve(history([message(8, { body: '新记录' })], { latestChangeSeq: 12, generation: 2 }))
    await syncing
    expect(feed.messages.value).toEqual([expect.objectContaining({ id: 8, body: '新记录' })])
    expect(feed.generation.value).toBe(2)
    expect(feed.cursor.value).toBe(12)
    expect(feed.refreshNotice.value).toBe('聊天记录已刷新')
    feed.dispose()
  })

  it('stops a continuous reset loop, leaves no stale messages and waits for manual reload', async () => {
    const client = makeClient({
      chatMessages: vi.fn()
        .mockResolvedValueOnce(history([message(1)], { latestChangeSeq: 1, generation: 1 }))
        .mockResolvedValueOnce(history([message(2)], { latestChangeSeq: 2, generation: 2 })),
      chatChanges: vi.fn().mockRejectedValue(new ApiError('reset', 409, undefined, 'chat_cursor_reset_required')),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5, maxConsecutiveResets: 1 })
    await feed.start()
    await feed.syncNow()
    await feed.syncNow()

    expect(feed.resetBlocked.value).toBe(true)
    expect(feed.messages.value).toEqual([])
    expect(feed.generation.value).toBeNull()
    expect(feed.initialError.value).toContain('连续刷新失败')
    expect(client.chatMessages).toHaveBeenCalledTimes(2)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(client.chatChanges).toHaveBeenCalledTimes(2)
    feed.dispose()
  })

  it('polls only while visible and syncs immediately on visibility and focus without heartbeat helpers', async () => {
    let visibility: DocumentVisibilityState = 'visible'
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility)
    const heartbeat = vi.fn()
    const recordSessionActivity = vi.fn()
    const client = Object.assign(makeClient(), { heartbeat, recordSessionActivity })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    await vi.advanceTimersByTimeAsync(3_000)
    expect(client.chatChanges).toHaveBeenCalledTimes(1)

    visibility = 'hidden'
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(12_000)
    expect(client.chatChanges).toHaveBeenCalledTimes(1)

    visibility = 'visible'
    document.dispatchEvent(new Event('visibilitychange'))
    await flushMicrotasks()
    expect(client.chatChanges).toHaveBeenCalledTimes(2)

    window.dispatchEvent(new Event('focus'))
    await flushMicrotasks()
    expect(client.chatChanges).toHaveBeenCalledTimes(3)
    expect(heartbeat).not.toHaveBeenCalled()
    expect(recordSessionActivity).not.toHaveBeenCalled()
    feed.dispose()
  })

  it('backs off after a network failure and clears the non-blocking warning after recovery', async () => {
    const client = makeClient({
      chatChanges: vi.fn()
        .mockRejectedValueOnce(new TypeError('offline'))
        .mockResolvedValueOnce(changes()),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()

    await vi.advanceTimersByTimeAsync(3_000)
    expect(feed.syncWarning.value).toContain('消息更新暂时中断')
    await vi.advanceTimersByTimeAsync(5_999)
    expect(client.chatChanges).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(client.chatChanges).toHaveBeenCalledTimes(2)
    expect(feed.syncWarning.value).toBe('')
    feed.dispose()
  })

  it('never overlaps change polls and aborts the active request on dispose', async () => {
    const pending = deferred<ChatChangesResponse>()
    let activeSignal: AbortSignal | undefined
    const client = makeClient({
      chatChanges: vi.fn((_admin, _params, signal) => {
        activeSignal = signal
        return pending.promise
      }),
    })
    const feed = useChatFeed({ admin: false, client, random: () => 0.5 })
    await feed.start()
    await vi.advanceTimersByTimeAsync(3_000)

    window.dispatchEvent(new Event('focus'))
    document.dispatchEvent(new Event('visibilitychange'))
    await flushMicrotasks()
    expect(client.chatChanges).toHaveBeenCalledTimes(1)

    feed.dispose()
    expect(activeSignal?.aborted).toBe(true)
    pending.resolve(changes())
    await flushMicrotasks()
    expect(feed.syncWarning.value).toBe('')
  })

  it('clears messages and ignores a mutation response that finishes after dispose', async () => {
    const pendingMutation = deferred<ChatMutationResponse>()
    const client = makeClient({
      chatMessages: vi.fn(async () => history([message(1)], { latestChangeSeq: 5 })),
      createChatMessage: vi.fn(() => pendingMutation.promise),
    })
    const feed = useChatFeed({ admin: true, client, random: () => 0.5 })
    await feed.start()
    feed.toggleSelection(1, true)
    const sending = feed.send('dispose 后不得插入')
    await flushMicrotasks()

    feed.dispose()
    expect(feed.messages.value).toEqual([])
    expect(feed.selectedIds.value).toEqual([])
    pendingMutation.resolve(mutation(message(2, { body: '迟到 mutation', isMine: true }), 6))
    await sending
    expect(feed.messages.value).toEqual([])
  })
})
