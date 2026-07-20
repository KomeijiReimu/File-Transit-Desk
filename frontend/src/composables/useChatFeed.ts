import { computed, ref } from 'vue'
import { ApiError, api } from '@/api'
import { chatErrorMessage } from '@/chatErrors'
import type {
  ChatBatchDeleteResponse,
  ChatCapabilities,
  ChatChangesResponse,
  ChatClearResponse,
  ChatHistoryResponse,
  ChatMessage,
  ChatMutationResponse,
} from '@/types'

export interface ChatApiClient {
  chatCapabilities: (signal?: AbortSignal) => Promise<ChatCapabilities>
  chatMessages: (admin: boolean, params?: { beforeId?: number; limit?: number }, signal?: AbortSignal) => Promise<ChatHistoryResponse>
  chatChanges: (admin: boolean, params: { afterSeq: number; generation: number; limit?: number }, signal?: AbortSignal) => Promise<ChatChangesResponse>
  createChatMessage: (body: string, signal?: AbortSignal) => Promise<ChatMutationResponse>
  withdrawChatMessage: (id: number, signal?: AbortSignal) => Promise<ChatMutationResponse>
  deleteChatMessage: (id: number, signal?: AbortSignal) => Promise<ChatMutationResponse>
  batchDeleteChatMessages: (ids: number[], signal?: AbortSignal) => Promise<ChatBatchDeleteResponse>
  clearChatMessages: (expectedGeneration: number, expectedLatestChangeSeq: number, signal?: AbortSignal) => Promise<ChatClearResponse>
}

export interface UseChatFeedOptions {
  admin: boolean
  active?: boolean
  subjectKey?: string
  revalidateAuth?: () => Promise<unknown> | unknown
  onProjectionInvalidated?: () => void
  client?: ChatApiClient
  pollIntervalMs?: number
  maxBackoffMs?: number
  maxChangePagesPerSync?: number
  maxConsecutiveResets?: number
  random?: () => number
}

export interface ChatFeedBinding {
  subjectKey: string
  admin: boolean
  active: boolean
}

const DEFAULT_POLL_INTERVAL_MS = 3_000
const DEFAULT_MAX_BACKOFF_MS = 30_000
const DEFAULT_MAX_CHANGE_PAGES = 10
const DEFAULT_MAX_CONSECUTIVE_RESETS = 2

function isAbortError(error: unknown) {
  return (error instanceof ApiError && error.aborted)
    || (error instanceof DOMException && error.name === 'AbortError')
}

export function normalizeChatMessage(message: ChatMessage, admin: boolean): ChatMessage {
  const status = message.status
  const body = status === 'deleted' || (!admin && status === 'withdrawn')
    ? null
    : typeof message.body === 'string' ? message.body : null
  const normalized: ChatMessage = {
    id: message.id,
    authorTag: message.authorTag,
    role: message.role,
    body,
    status,
    isMine: Boolean(message.isMine),
    createdAt: message.createdAt,
    withdrawnAt: message.withdrawnAt || null,
    deletedAt: message.deletedAt || null,
    canWithdraw: status === 'active' && Boolean(message.canWithdraw),
    withdrawUntil: status === 'active' ? message.withdrawUntil || null : null,
    sourceIP: typeof message.sourceIP === 'string' ? message.sourceIP : '',
  }
  return normalized
}

export type ChatProjectionSource = 'mutation' | 'history' | 'changes'

export interface ChatMessageWatermark {
  generation: number
  seq: number
  source: ChatProjectionSource
  priority: number
}

const CHAT_PROJECTION_PRIORITY: Record<ChatProjectionSource, number> = {
  mutation: 0,
  history: 1,
  changes: 2,
}

interface ChatRequestContext {
  revision: number
  generation: number | null
  admin: boolean
  subjectKey: string
}

interface VersionedChatMessage {
  message: ChatMessage
  watermark: ChatMessageWatermark
}

function serviceSeqValid(value: number) {
  return Number.isSafeInteger(value) && value >= 0
}

function watermark(generation: number, seq: number, source: ChatProjectionSource): ChatMessageWatermark {
  return { generation, seq, source, priority: CHAT_PROJECTION_PRIORITY[source] }
}

export function chatWatermarkIsNewer(current: ChatMessageWatermark, incoming: ChatMessageWatermark) {
  if (current.generation !== incoming.generation) return false
  if (current.seq !== incoming.seq) return incoming.seq > current.seq
  return incoming.priority > current.priority
}

export function useChatFeed(options: UseChatFeedOptions) {
  const client = options.client || api
  const pollIntervalMs = options.pollIntervalMs || DEFAULT_POLL_INTERVAL_MS
  const maxBackoffMs = options.maxBackoffMs || DEFAULT_MAX_BACKOFF_MS
  const maxChangePages = options.maxChangePagesPerSync || DEFAULT_MAX_CHANGE_PAGES
  const maxConsecutiveResets = options.maxConsecutiveResets || DEFAULT_MAX_CONSECUTIVE_RESETS
  const random = options.random || Math.random
  let binding: ChatFeedBinding = {
    subjectKey: options.subjectKey || `legacy:${options.admin ? 'admin' : 'user'}`,
    admin: options.admin,
    active: options.active ?? true,
  }

  const messages = ref<ChatMessage[]>([])
  const capabilities = ref<ChatCapabilities | null>(null)
  const capabilitiesLoading = ref(false)
  const capabilitiesError = ref('')
  const initialLoading = ref(true)
  const initialError = ref('')
  const initialized = ref(false)
  const loadingOlder = ref(false)
  const olderError = ref('')
  const hasMore = ref(false)
  const nextBeforeId = ref<number | null>(null)
  const generation = ref<number | null>(null)
  const cursor = ref(0)
  const latestChangeSeq = ref(0)
  const selectedIds = ref<number[]>([])
  const selectionError = ref('')
  const selectedCount = computed(() => selectedIds.value.length)
  const selectionAtLimit = computed(() => selectedIds.value.length >= 100)
  const syncing = ref(false)
  const syncWarning = ref('')
  const refreshNotice = ref('')
  const lastSyncedAt = ref<Date | null>(null)
  const resetBlocked = ref(false)

  const connectionState = computed<'loading' | 'online' | 'interrupted'>(() => {
    if (!initialized.value || initialLoading.value) return 'loading'
    return syncWarning.value ? 'interrupted' : 'online'
  })

  let started = false
  let disposed = false
  let timer: number | undefined
  let noticeTimer: number | undefined
  let stateRevision = 0
  let failureCount = 0
  let resetStreak = 0
  let capabilityRequestId = 0
  let historyRequestId = 0
  let olderRequestId = 0
  let capabilityController: AbortController | undefined
  let historyController: AbortController | undefined
  let olderController: AbortController | undefined
  let syncController: AbortController | undefined
  let initializePromise: Promise<boolean> | null = null
  let syncPromise: Promise<void> | null = null
  let authRevalidationPromise: Promise<unknown> | null = null
  let authorizationBlocked = false
  const mutationControllers = new Set<AbortController>()
  const messageWatermarks = new Map<number, ChatMessageWatermark>()

  function visible() {
    return typeof document === 'undefined' || document.visibilityState !== 'hidden'
  }

  function clearTimer() {
    if (timer !== undefined) window.clearTimeout(timer)
    timer = undefined
  }

  function clearNoticeTimer() {
    if (noticeTimer !== undefined) window.clearTimeout(noticeTimer)
    noticeTimer = undefined
  }

  function jitter(delay: number) {
    return Math.max(50, Math.round(delay * (0.9 + random() * 0.2)))
  }

  function successDelay() {
    return jitter(pollIntervalMs)
  }

  function failureDelay() {
    const exponential = Math.min(maxBackoffMs, pollIntervalMs * (2 ** Math.min(failureCount, 6)))
    return Math.min(maxBackoffMs, jitter(exponential))
  }

  function schedule(delay = successDelay()) {
    clearTimer()
    if (!started || disposed || !binding.active || authorizationBlocked || !initialized.value || resetBlocked.value || !visible()) return
    timer = window.setTimeout(() => {
      timer = undefined
      void syncNow()
    }, delay)
  }

  function showRefreshNotice() {
    clearNoticeTimer()
    refreshNotice.value = '聊天记录已刷新'
    noticeTimer = window.setTimeout(() => {
      refreshNotice.value = ''
      noticeTimer = undefined
    }, 4_500)
  }

  function captureRequestContext(): ChatRequestContext {
    return {
      revision: stateRevision,
      generation: generation.value,
      admin: binding.admin,
      subjectKey: binding.subjectKey,
    }
  }

  function requestContextIsCurrent(context: ChatRequestContext) {
    return !disposed
      && context.revision === stateRevision
      && context.generation === generation.value
      && context.admin === binding.admin
      && context.subjectKey === binding.subjectKey
      && binding.active
      && !authorizationBlocked
  }

  function clearSelection() {
    selectedIds.value = []
    selectionError.value = ''
  }

  function reconcileSelection() {
    if (!binding.admin) {
      clearSelection()
      return
    }
    const byId = new Map(messages.value.map((message) => [message.id, message]))
    const next = selectedIds.value.filter((id) => {
      const message = byId.get(id)
      return !message || message.status !== 'deleted'
    })
    if (next.length !== selectedIds.value.length) selectedIds.value = next
    if (selectedIds.value.length < 100) selectionError.value = ''
  }

  function toggleSelection(id: number, selected: boolean) {
    selectionError.value = ''
    if (!binding.admin || !binding.active || authorizationBlocked) return false
    const message = messages.value.find((item) => item.id === id)
    if (!message || message.status === 'deleted') return false
    const current = new Set(selectedIds.value)
    if (!selected) {
      current.delete(id)
      selectedIds.value = [...current]
      return true
    }
    if (current.has(id)) return true
    if (current.size >= 100) {
      selectionError.value = '最多选择 100 条消息。'
      return false
    }
    current.add(id)
    selectedIds.value = [...current]
    return true
  }

  function applyVersionedMessages(incoming: VersionedChatMessage[], admin: boolean) {
    if (!incoming.length) return
    const byId = new Map(messages.value.map((message) => [message.id, message]))
    let changed = false
    incoming.forEach(({ message, watermark: incomingWatermark }) => {
      if (incomingWatermark.generation !== generation.value) return
      const currentWatermark = messageWatermarks.get(message.id)
      if (currentWatermark && !chatWatermarkIsNewer(currentWatermark, incomingWatermark)) return
      // 当前响应的规范化投影整体替换旧对象，绝不保留旧正文或投影专属字段。
      byId.set(message.id, normalizeChatMessage(message, admin))
      messageWatermarks.set(message.id, incomingWatermark)
      changed = true
    })
    if (changed) {
      messages.value = [...byId.values()].sort((left, right) => left.id - right.id)
      reconcileSelection()
    }
  }

  function initializeMessagesFromSnapshot(incoming: ChatMessage[], snapshotGeneration: number, snapshotSeq: number, admin: boolean) {
    messages.value = []
    messageWatermarks.clear()
    clearSelection()
    applyVersionedMessages(incoming.map((message) => ({
      message,
      watermark: watermark(snapshotGeneration, snapshotSeq, 'history'),
    })), admin)
  }

  async function loadCapabilities() {
    if (disposed || !binding.active || authorizationBlocked) return false
    const requestId = ++capabilityRequestId
    const context = captureRequestContext()
    capabilityController?.abort()
    const controller = new AbortController()
    capabilityController = controller
    capabilitiesLoading.value = true
    capabilitiesError.value = ''
    try {
      const result = await client.chatCapabilities(controller.signal)
      if (disposed || requestId !== capabilityRequestId || !binding.active || authorizationBlocked) return false
      capabilities.value = result
      return true
    } catch (error) {
      if (isAbortError(error)) return false
      if (handleSensitiveAuthError(error, context, false)) return false
      if (disposed || requestId !== capabilityRequestId || !binding.active || authorizationBlocked) return false
      capabilities.value = null
      capabilitiesError.value = chatErrorMessage(error, 'capabilities')
      return false
    } finally {
      if (requestId === capabilityRequestId) {
        capabilitiesLoading.value = false
        if (capabilityController === controller) capabilityController = undefined
      }
    }
  }

  async function loadLatestHistory(reason: 'initial' | 'reset' | 'manual' = 'initial') {
    if (disposed || !binding.active || authorizationBlocked) return false
    const requestId = ++historyRequestId
    const context = captureRequestContext()
    historyController?.abort()
    const controller = new AbortController()
    historyController = controller
    initialLoading.value = true
    initialError.value = ''
    try {
      const limit = capabilities.value?.historyDefaultLimit
      const result = await client.chatMessages(context.admin, { limit }, controller.signal)
      if (requestId !== historyRequestId || !requestContextIsCurrent(context)) return false
      if (!Number.isSafeInteger(result.generation) || result.generation < 1) {
        throw new ApiError('聊天同步 generation 无效。', 400, undefined, 'chat_generation_invalid')
      }
      if (!serviceSeqValid(result.latestChangeSeq)) {
        throw new ApiError('聊天历史水位无效。', 409, undefined, 'chat_cursor_reset_required')
      }
      if (context.generation !== null && result.generation !== context.generation) {
        await handleCursorReset()
        return false
      }

      if (context.generation === null) {
        generation.value = result.generation
        initializeMessagesFromSnapshot(result.messages || [], result.generation, result.latestChangeSeq, context.admin)
        // 只有初始或 reset 后的最新快照建立全局游标。
        cursor.value = result.latestChangeSeq
        latestChangeSeq.value = result.latestChangeSeq
      } else {
        applyVersionedMessages((result.messages || []).map((message) => ({
          message,
          watermark: watermark(result.generation, result.latestChangeSeq, 'history'),
        })), context.admin)
        latestChangeSeq.value = Math.max(latestChangeSeq.value, result.latestChangeSeq)
      }
      nextBeforeId.value = result.nextBeforeId ?? null
      hasMore.value = Boolean(result.hasMore)
      initialized.value = true
      lastSyncedAt.value = new Date()
      syncWarning.value = ''
      if (reason === 'reset' || reason === 'manual') showRefreshNotice()
      return true
    } catch (error) {
      if (isAbortError(error)) return false
      if (handleSensitiveAuthError(error, context, context.admin)) return false
      if (requestId !== historyRequestId || !requestContextIsCurrent(context)) return false
      initialized.value = false
      initialError.value = chatErrorMessage(error, 'history')
      return false
    } finally {
      if (requestId === historyRequestId) {
        initialLoading.value = false
        if (historyController === controller) historyController = undefined
      }
    }
  }

  function invalidateProjectionState(clearCapabilities: boolean, blocked: boolean) {
    stateRevision += 1
    clearTimer()
    clearNoticeTimer()
    if (clearCapabilities) capabilityController?.abort()
    syncController?.abort()
    olderController?.abort()
    historyController?.abort()
    mutationControllers.forEach((controller) => controller.abort())
    mutationControllers.clear()
    if (clearCapabilities) capabilityController = undefined
    syncController = undefined
    olderController = undefined
    historyController = undefined
    if (clearCapabilities) capabilityRequestId += 1
    olderRequestId += 1
    historyRequestId += 1
    initializePromise = null
    syncPromise = null
    messages.value = []
    messageWatermarks.clear()
    nextBeforeId.value = null
    hasMore.value = false
    generation.value = null
    cursor.value = 0
    latestChangeSeq.value = 0
    clearSelection()
    initialized.value = false
    initialLoading.value = false
    loadingOlder.value = false
    initialError.value = ''
    olderError.value = ''
    syncing.value = false
    syncWarning.value = ''
    refreshNotice.value = ''
    lastSyncedAt.value = null
    resetBlocked.value = false
    authorizationBlocked = blocked
    if (clearCapabilities) {
      capabilities.value = null
      capabilitiesLoading.value = false
      capabilitiesError.value = ''
    }
  }

  function clearFeedAtomically() {
    invalidateProjectionState(false, false)
  }

  function clearForSubjectChange(blocked = false) {
    invalidateProjectionState(true, blocked)
    failureCount = 0
    resetStreak = 0
  }

  function triggerAuthRevalidation() {
    if (!options.revalidateAuth || authRevalidationPromise) return
    let result: Promise<unknown> | unknown
    try {
      result = options.revalidateAuth()
    } catch {
      return
    }
    const pending = Promise.resolve(result)
    authRevalidationPromise = pending
    void pending.catch(() => undefined).finally(() => {
      if (authRevalidationPromise === pending) authRevalidationPromise = null
    })
  }

  function handleSensitiveAuthError(error: unknown, context: ChatRequestContext, adminEndpoint: boolean) {
    if (!requestContextIsCurrent(context) || !(error instanceof ApiError)) return false
    const subjectChanged = error.status === 409 && error.code === 'session_subject_changed'
    const adminForbidden = adminEndpoint && context.admin && error.status === 403
    if (!subjectChanged && !adminForbidden) return false
    // 先同步销毁当前投影，再让 auth singleflight 完成可信身份重验。
    clearForSubjectChange(true)
    options.onProjectionInvalidated?.()
    if (adminForbidden) triggerAuthRevalidation()
    return true
  }

  async function replaceFromLatestHistory(reason: 'reset' | 'manual') {
    clearFeedAtomically()
    return loadLatestHistory(reason)
  }

  async function handleCursorReset() {
    if (resetStreak >= maxConsecutiveResets) {
      clearFeedAtomically()
      resetBlocked.value = true
      syncWarning.value = '聊天同步连续失效，已暂停自动重试。请重新加载聊天记录。'
      initialLoading.value = false
      initialError.value = '聊天记录连续刷新失败，请手动重新加载。'
      return false
    }
    resetStreak += 1
    const recovered = await replaceFromLatestHistory('reset')
    if (recovered && started) schedule()
    return recovered
  }

  async function initialize() {
    if (disposed || !binding.active || authorizationBlocked) return false
    if (initializePromise) return initializePromise
    const context = captureRequestContext()
    const pending = (async () => {
      if (!capabilities.value) await loadCapabilities()
      if (!requestContextIsCurrent(context)) return false
      const loaded = await loadLatestHistory('initial')
      if (loaded && started) schedule()
      return loaded
    })()
    initializePromise = pending
    void pending.finally(() => {
      if (initializePromise === pending) initializePromise = null
    })
    return pending
  }

  async function retryInitial() {
    if (disposed || !binding.active || authorizationBlocked) return false
    const context = captureRequestContext()
    if (!capabilities.value) await loadCapabilities()
    if (!requestContextIsCurrent(context)) return false
    const loaded = await loadLatestHistory('initial')
    if (loaded && started) schedule()
    return loaded
  }

  async function retryCapabilities() {
    if (disposed || !binding.active || authorizationBlocked) return false
    return loadCapabilities()
  }

  async function loadOlder() {
    if (disposed || loadingOlder.value || !initialized.value || !hasMore.value || nextBeforeId.value === null) return false
    const requestId = ++olderRequestId
    const context = captureRequestContext()
    const beforeId = nextBeforeId.value
    olderController?.abort()
    const controller = new AbortController()
    olderController = controller
    loadingOlder.value = true
    olderError.value = ''
    try {
      const limit = capabilities.value?.historyDefaultLimit
      const result = await client.chatMessages(context.admin, { beforeId, limit }, controller.signal)
      if (requestId !== olderRequestId || !requestContextIsCurrent(context)) return false
      if (context.generation === null || result.generation !== context.generation) {
        await handleCursorReset()
        return false
      }
      if (!serviceSeqValid(result.latestChangeSeq)) {
        throw new ApiError('聊天历史水位无效。', 409, undefined, 'chat_cursor_reset_required')
      }
      applyVersionedMessages((result.messages || []).map((message) => ({
        message,
        watermark: watermark(context.generation as number, result.latestChangeSeq, 'history'),
      })), context.admin)
      latestChangeSeq.value = Math.max(latestChangeSeq.value, result.latestChangeSeq)
      nextBeforeId.value = result.nextBeforeId ?? null
      hasMore.value = Boolean(result.hasMore)
      return true
    } catch (error) {
      if (isAbortError(error)) return false
      if (handleSensitiveAuthError(error, context, context.admin)) return false
      if (requestId !== olderRequestId || !requestContextIsCurrent(context)) return false
      if (error instanceof ApiError && error.code === 'chat_cursor_reset_required') {
        await handleCursorReset()
        return false
      }
      olderError.value = chatErrorMessage(error, 'history')
      return false
    } finally {
      if (requestId === olderRequestId) {
        loadingOlder.value = false
        if (olderController === controller) olderController = undefined
      }
    }
  }

  function validatedChangePage(result: ChatChangesResponse, context: ChatRequestContext, pageCursor: number) {
    if (context.generation === null
      || result.generation !== context.generation
      || !serviceSeqValid(result.nextAfterSeq)
      || !serviceSeqValid(result.latestChangeSeq)
      || result.nextAfterSeq < pageCursor
      || result.nextAfterSeq > result.latestChangeSeq
      || (result.hasMore && result.nextAfterSeq === pageCursor)) {
      throw new ApiError('聊天同步游标已失效。', 409, undefined, 'chat_cursor_reset_required')
    }
    const ordered = [...(result.changes || [])].sort((left, right) => left.seq - right.seq)
    if (ordered.some((change, index) => !serviceSeqValid(change.seq)
      || change.seq <= pageCursor
      || change.seq > result.nextAfterSeq
      || (index > 0 && change.seq <= (ordered[index - 1]?.seq || 0)))) {
      throw new ApiError('聊天同步变更页无效。', 409, undefined, 'chat_cursor_reset_required')
    }
    return ordered
  }

  async function readChangePages(controller: AbortController, context: ChatRequestContext) {
    let pageHasMore = false
    for (let page = 0; page < maxChangePages; page += 1) {
      if (!requestContextIsCurrent(context) || context.generation === null) return false
      const pageCursor = cursor.value
      const result = await client.chatChanges(context.admin, {
        afterSeq: pageCursor,
        generation: context.generation,
        limit: capabilities.value?.changesMaxLimit,
      }, controller.signal)
      if (!requestContextIsCurrent(context)) return false
      const ordered = validatedChangePage(result, context, pageCursor)
      applyVersionedMessages(ordered.map((change) => ({
        message: change.message,
        watermark: watermark(context.generation as number, change.seq, 'changes'),
      })), context.admin)
      // mutation.eventSeq 与 change.seq 都不能单独推进游标，只接受服务端分页返回的 nextAfterSeq。
      cursor.value = result.nextAfterSeq
      latestChangeSeq.value = Math.max(latestChangeSeq.value, result.latestChangeSeq)
      lastSyncedAt.value = new Date()
      pageHasMore = Boolean(result.hasMore)
      if (!pageHasMore) return false
    }
    return pageHasMore
  }

  function syncNow() {
    clearTimer()
    if (disposed || !started || !binding.active || authorizationBlocked || !visible() || !initialized.value || generation.value === null || resetBlocked.value) {
      return Promise.resolve()
    }
    if (syncPromise) return syncPromise

    const context = captureRequestContext()
    const controller = new AbortController()
    syncController = controller
    syncing.value = true
    let nextDelay: number | null = null
    let pending!: Promise<void>
    pending = (async () => {
      try {
        const continueImmediately = await readChangePages(controller, context)
        if (!requestContextIsCurrent(context)) return
        failureCount = 0
        resetStreak = 0
        syncWarning.value = ''
        nextDelay = continueImmediately ? 50 : successDelay()
      } catch (error) {
        if (handleSensitiveAuthError(error, context, context.admin)) return
        if (!requestContextIsCurrent(context)) return
        if (isAbortError(error)) {
          if (visible()) nextDelay = 0
          return
        }
        if (error instanceof ApiError && error.code === 'chat_cursor_reset_required') {
          const recovered = await handleCursorReset()
          if (recovered) {
            failureCount = 0
            nextDelay = successDelay()
          }
          return
        }
        failureCount += 1
        syncWarning.value = chatErrorMessage(error, 'sync')
        nextDelay = failureDelay()
      } finally {
        if (syncController === controller) syncController = undefined
        if (syncPromise === pending) {
          syncing.value = false
          syncPromise = null
          if (nextDelay !== null) schedule(nextDelay)
        }
      }
    })()
    syncPromise = pending
    return pending
  }

  async function reloadAfterSyncFailure() {
    if (disposed || !binding.active || authorizationBlocked) return false
    resetBlocked.value = false
    resetStreak = 0
    failureCount = 0
    syncWarning.value = ''
    const loaded = await replaceFromLatestHistory('manual')
    if (loaded && started) schedule()
    return loaded
  }

  async function withMutation<T extends ChatMutationResponse>(request: (signal: AbortSignal) => Promise<T>, adminEndpoint = false) {
    if (disposed || !binding.active || authorizationBlocked) {
      throw new ApiError('聊天会话尚未就绪。', 409, undefined, 'session_subject_changed')
    }
    const context = captureRequestContext()
    const controller = new AbortController()
    mutationControllers.add(controller)
    try {
      const result = await request(controller.signal)
      if (requestContextIsCurrent(context) && context.generation !== null && serviceSeqValid(result.eventSeq)) {
        applyVersionedMessages([{
          message: result.message,
          watermark: watermark(context.generation, result.eventSeq, 'mutation'),
        }], context.admin)
        latestChangeSeq.value = Math.max(latestChangeSeq.value, result.eventSeq)
      }
      // eventSeq 故意不写入 cursor；后续 changes 会按 id 覆盖同一消息。
      return normalizeChatMessage(result.message, context.admin)
    } catch (error) {
      if (handleSensitiveAuthError(error, context, adminEndpoint)) throw error
      if (requestContextIsCurrent(context) && error instanceof ApiError && error.code === 'chat_cursor_reset_required') {
        await handleCursorReset()
      }
      throw error
    } finally {
      mutationControllers.delete(controller)
    }
  }

  function send(body: string) {
    return withMutation((signal) => client.createChatMessage(body, signal))
  }

  function withdraw(id: number) {
    return withMutation((signal) => client.withdrawChatMessage(id, signal))
  }

  function remove(id: number) {
    return withMutation((signal) => client.deleteChatMessage(id, signal), true)
  }

  function validatedBatchIDs(ids: number[]) {
    if (ids.length < 1 || ids.length > 100
      || ids.some((id) => !Number.isSafeInteger(id) || id < 1)
      || new Set(ids).size !== ids.length) {
      throw new ApiError('批量删除请求无效。', 400, undefined, 'chat_batch_delete_request_invalid')
    }
    return [...ids]
  }

  async function batchDelete(ids: number[]) {
    if (disposed || !binding.active || authorizationBlocked || !binding.admin || generation.value === null) {
      throw new ApiError('聊天会话尚未就绪。', 409, undefined, 'session_subject_changed')
    }
    const requestedIDs = validatedBatchIDs(ids)
    const context = captureRequestContext()
    const controller = new AbortController()
    mutationControllers.add(controller)
    try {
      const result = await client.batchDeleteChatMessages(requestedIDs, controller.signal)
      if (!requestContextIsCurrent(context) || context.generation === null) return result
      if (!Array.isArray(result.mutations)
        || result.mutations.some((mutation) => !serviceSeqValid(mutation.eventSeq))) {
        throw new ApiError('批量删除响应无效。', 409, undefined, 'chat_state_conflict')
      }
      applyVersionedMessages(result.mutations.map((mutation) => ({
        message: mutation.message,
        watermark: watermark(context.generation as number, mutation.eventSeq, 'mutation'),
      })), context.admin)
      result.mutations.forEach((mutation) => {
        latestChangeSeq.value = Math.max(latestChangeSeq.value, mutation.eventSeq)
      })
      reconcileSelection()
      return result
    } catch (error) {
      if (handleSensitiveAuthError(error, context, true)) throw error
      throw error
    } finally {
      mutationControllers.delete(controller)
    }
  }

  async function clearAll() {
    if (disposed || !binding.active || authorizationBlocked || !binding.admin || generation.value === null) {
      throw new ApiError('聊天会话尚未就绪。', 409, undefined, 'session_subject_changed')
    }
    const context = captureRequestContext()
    const expectedGeneration = context.generation as number
    const expectedLatestChangeSeq = latestChangeSeq.value
    const controller = new AbortController()
    mutationControllers.add(controller)
    try {
      const result = await client.clearChatMessages(expectedGeneration, expectedLatestChangeSeq, controller.signal)
      if (!requestContextIsCurrent(context)) return result
      const loaded = await replaceFromLatestHistory('manual')
      if (loaded && started) schedule()
      return result
    } catch (error) {
      if (handleSensitiveAuthError(error, context, true)) throw error
      const reloadConflict = requestContextIsCurrent(context)
        && error instanceof ApiError
        && error.code === 'chat_clear_conflict'
      if (reloadConflict) {
        const loaded = await replaceFromLatestHistory('manual')
        if (loaded && started) schedule()
        throw new ApiError('消息已更新，请重新确认清空。', 409, error.details, 'chat_clear_conflict')
      }
      throw error
    } finally {
      mutationControllers.delete(controller)
    }
  }

  async function rebind(nextBinding: ChatFeedBinding) {
    if (disposed) return false
    if (binding.subjectKey === nextBinding.subjectKey
      && binding.admin === nextBinding.admin
      && binding.active === nextBinding.active
      && !authorizationBlocked) {
      return initialized.value
    }

    // revision 提升和敏感投影清理必须发生在任何新主体请求之前。
    clearForSubjectChange(false)
    binding = { ...nextBinding }
    if (!binding.active) return false
    initialLoading.value = true
    if (!started) return false
    return initialize()
  }

  function onVisibilityChange() {
    if (!binding.active || authorizationBlocked) return
    if (!visible()) {
      clearTimer()
      syncController?.abort()
      return
    }
    if (initialized.value) void syncNow()
    else if (!initialLoading.value) void initialize()
  }

  function onWindowFocus() {
    if (binding.active && !authorizationBlocked && visible() && initialized.value) void syncNow()
  }

  async function start() {
    if (started || disposed) return initialized.value
    started = true
    if (typeof document !== 'undefined') document.addEventListener('visibilitychange', onVisibilityChange)
    if (typeof window !== 'undefined') window.addEventListener('focus', onWindowFocus)
    if (!binding.active || authorizationBlocked) {
      initialLoading.value = false
      return false
    }
    return initialize()
  }

  function dispose() {
    if (disposed) return
    disposed = true
    started = false
    stateRevision += 1
    clearTimer()
    clearNoticeTimer()
    if (typeof document !== 'undefined') document.removeEventListener('visibilitychange', onVisibilityChange)
    if (typeof window !== 'undefined') window.removeEventListener('focus', onWindowFocus)
    capabilityController?.abort()
    historyController?.abort()
    olderController?.abort()
    syncController?.abort()
    mutationControllers.forEach((controller) => controller.abort())
    mutationControllers.clear()
    initializePromise = null
    syncPromise = null
    messages.value = []
    messageWatermarks.clear()
    latestChangeSeq.value = 0
    clearSelection()
  }

  return {
    messages,
    capabilities,
    capabilitiesLoading,
    capabilitiesError,
    initialLoading,
    initialError,
    initialized,
    loadingOlder,
    olderError,
    hasMore,
    nextBeforeId,
    generation,
    cursor,
    latestChangeSeq,
    selectedIds,
    selectedCount,
    selectionAtLimit,
    selectionError,
    syncing,
    syncWarning,
    refreshNotice,
    lastSyncedAt,
    resetBlocked,
    connectionState,
    start,
    dispose,
    retryInitial,
    retryCapabilities,
    loadOlder,
    syncNow,
    reloadAfterSyncFailure,
    rebind,
    send,
    withdraw,
    remove,
    toggleSelection,
    clearSelection,
    batchDelete,
    clearAll,
  }
}
