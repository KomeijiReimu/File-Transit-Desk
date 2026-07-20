<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ApiError } from '@/api'
import { authState, restoreSession } from '@/auth'
import { chatErrorMessage } from '@/chatErrors'
import AppIcon from '@/components/AppIcon.vue'
import ChatComposer from '@/components/ChatComposer.vue'
import ChatMessageList from '@/components/ChatMessageList.vue'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import StateBlock from '@/components/StateBlock.vue'
import { useChatFeed, type ChatFeedBinding } from '@/composables/useChatFeed'
import type { ChatMessage } from '@/types'
import { useGsapEntrance } from '@/useGsapEntrance'

type ChatListHandle = {
  isNearBottom: () => boolean
  scrollToBottom: (smooth?: boolean) => void
  loadOlderPreservingPosition: (load: () => Promise<boolean>) => Promise<boolean>
  focusLog: () => void
}

type ChatComposerHandle = { focus: () => void }
type PendingAction = { kind: 'withdraw' | 'delete'; message: ChatMessage }
type PendingBulkAction = 'batch-delete' | 'clear'

const pageRef = ref<HTMLElement | null>(null)
const listRef = ref<ChatListHandle | null>(null)
const composerRef = ref<ChatComposerHandle | null>(null)
const draft = ref('')
const sending = ref(false)
const sendError = ref('')
const newMessageCount = ref(0)
const pendingAction = ref<PendingAction | null>(null)
const actionLoading = ref(false)
const actionError = ref('')
const pendingBulkAction = ref<PendingBulkAction | null>(null)
const bulkLoading = ref(false)
const bulkActionError = ref('')
const bulkError = ref('')
const retryingSync = ref(false)
let followingOwnSend = false
let viewSubjectRevision = 0
let subjectActivationToken = 0

const chatSubject = computed<ChatFeedBinding>(() => {
  const status = authState.status
  const role = authState.role
  const rawBinding = authState.user?.sessionBinding
  const sessionBinding = typeof rawBinding === 'string' && rawBinding.trim() ? rawBinding : undefined
  const roleKnown = role === 'admin' || role === 'user'
  const active = status === 'authenticated' && authState.authenticated && roleKnown && Boolean(sessionBinding)
  return {
    subjectKey: JSON.stringify([status, authState.authenticated, role || null, sessionBinding || null]),
    active,
    admin: active && role === 'admin',
  }
})

const initialSubject = chatSubject.value
const adminProjection = computed(() => chatSubject.value.active && chatSubject.value.admin)
let feed!: ReturnType<typeof useChatFeed>

async function revalidateChatSubject() {
  subjectActivationToken += 1
  viewSubjectRevision += 1
  clearSubjectInteractionState()
  const result = await restoreSession()
  const latestSubject = chatSubject.value
  if (latestSubject.active) await feed.rebind(latestSubject)
  return result
}

feed = useChatFeed({
  admin: initialSubject.admin,
  active: initialSubject.active,
  subjectKey: initialSubject.subjectKey,
  revalidateAuth: revalidateChatSubject,
  onProjectionInvalidated: clearSubjectInteractionState,
})

useGsapEntrance(pageRef)

const connectionLabel = computed(() => {
  if (feed.connectionState.value === 'loading') return '正在连接'
  if (feed.connectionState.value === 'interrupted') return '更新暂时中断'
  return feed.syncing.value ? '正在获取新消息' : '消息更新正常'
})
const connectionIcon = computed(() => feed.connectionState.value === 'interrupted'
  ? 'wifi-off'
  : feed.connectionState.value === 'loading' || feed.syncing.value ? 'refresh' : 'message-circle')
const dialogTitle = computed(() => pendingAction.value?.kind === 'delete' ? '删除这条消息？' : '撤回这条消息？')
const dialogMessage = computed(() => pendingAction.value?.kind === 'delete'
  ? '删除后，所有人只会看到管理员删除提示，正文将不再显示。'
  : '撤回后，其他用户只会看到一条撤回提示。')
const dialogDetail = computed(() => pendingAction.value?.kind === 'delete'
  ? '管理员可以删除正常消息或已经撤回的消息，此操作不可恢复。'
  : '撤回仅在服务器给出的时间窗口内有效。')
const destructiveBusy = computed(() => actionLoading.value || bulkLoading.value)
const bulkDialogTitle = computed(() => pendingBulkAction.value === 'clear' ? '清空全部消息？' : '删除所选消息？')
const bulkDialogMessage = computed(() => pendingBulkAction.value === 'clear'
  ? '全部聊天消息将被清空，此操作不可恢复。'
  : `将删除已选择的 ${feed.selectedCount.value} 条消息，此操作不可恢复。`)
const bulkConfirmLabel = computed(() => pendingBulkAction.value === 'clear' ? '确认清空' : '确认删除')

watch(draft, () => { sendError.value = '' })

function clearSubjectInteractionState() {
  pendingAction.value = null
  actionError.value = ''
  actionLoading.value = false
  pendingBulkAction.value = null
  bulkLoading.value = false
  bulkActionError.value = ''
  bulkError.value = ''
  newMessageCount.value = 0
  draft.value = ''
  sendError.value = ''
  sending.value = false
  retryingSync.value = false
  followingOwnSend = false
}

watch(
  () => chatSubject.value.subjectKey,
  () => {
    const nextSubject = chatSubject.value
    const activationToken = ++subjectActivationToken
    viewSubjectRevision += 1
    // 先以 inactive 绑定同步销毁旧投影；同一认证写入批次结束后才允许新请求。
    void feed.rebind({ ...nextSubject, active: false })
    clearSubjectInteractionState()
    if (!nextSubject.active) return
    queueMicrotask(() => {
      const latestSubject = chatSubject.value
      if (activationToken !== subjectActivationToken
        || latestSubject.subjectKey !== nextSubject.subjectKey
        || !latestSubject.active) return
      void feed.rebind(latestSubject)
    })
  },
  { flush: 'sync' },
)

watch(
  () => feed.messages.value.map((message) => message.id),
  async (ids, previousIds = []) => {
    if (!feed.initialized.value || !ids.length) return
    if (!previousIds.length) {
      newMessageCount.value = 0
      await nextTick()
      listRef.value?.scrollToBottom(false)
      return
    }
    const previous = new Set(previousIds)
    const previousMax = Math.max(...previousIds)
    const additions = ids.filter((id) => !previous.has(id) && id > previousMax)
    if (!additions.length) return
    const shouldFollow = followingOwnSend || (listRef.value?.isNearBottom() ?? true)
    await nextTick()
    if (shouldFollow) {
      newMessageCount.value = 0
      listRef.value?.scrollToBottom(true)
    } else {
      newMessageCount.value += additions.length
    }
  },
  { flush: 'pre' },
)

async function handleSend(body: string) {
  if (sending.value) return
  const operationSubjectRevision = viewSubjectRevision
  followingOwnSend = true
  sending.value = true
  sendError.value = ''
  try {
    await feed.send(body)
    if (operationSubjectRevision !== viewSubjectRevision) return
    draft.value = ''
    newMessageCount.value = 0
    await nextTick()
    listRef.value?.scrollToBottom(true)
    await nextTick()
    newMessageCount.value = 0
  } catch (error) {
    if (operationSubjectRevision !== viewSubjectRevision) return
    sendError.value = chatErrorMessage(error, 'send')
  } finally {
    if (operationSubjectRevision === viewSubjectRevision) {
      sending.value = false
      await nextTick()
      if (operationSubjectRevision === viewSubjectRevision) {
        followingOwnSend = false
        composerRef.value?.focus()
      }
    }
  }
}

async function handleLoadOlder() {
  if (listRef.value) await listRef.value.loadOlderPreservingPosition(feed.loadOlder)
  else await feed.loadOlder()
}

function jumpToLatest() {
  newMessageCount.value = 0
  listRef.value?.scrollToBottom(true)
  void nextTick(() => listRef.value?.focusLog())
}

function onBottomChange(nearBottom: boolean) {
  if (nearBottom) newMessageCount.value = 0
}

function requestAction(kind: PendingAction['kind'], message: ChatMessage) {
  if (bulkLoading.value) return
  actionError.value = ''
  pendingAction.value = { kind, message }
}

function closeAction() {
  if (actionLoading.value) return
  pendingAction.value = null
  actionError.value = ''
}

async function confirmAction() {
  const action = pendingAction.value
  if (!action || actionLoading.value) return
  const operationSubjectRevision = viewSubjectRevision
  actionLoading.value = true
  actionError.value = ''
  try {
    if (action.kind === 'withdraw') await feed.withdraw(action.message.id)
    else await feed.remove(action.message.id)
    if (operationSubjectRevision !== viewSubjectRevision) return
    pendingAction.value = null
    await nextTick()
    await nextTick()
    listRef.value?.focusLog()
  } catch (error) {
    if (operationSubjectRevision !== viewSubjectRevision) return
    actionError.value = chatErrorMessage(error, action.kind)
    if (error instanceof ApiError && ['chat_withdraw_expired', 'chat_state_conflict', 'chat_message_not_found'].includes(error.code || '')) {
      void feed.syncNow()
    }
  } finally {
    if (operationSubjectRevision === viewSubjectRevision) actionLoading.value = false
  }
}

function handleSelection(message: ChatMessage, selected: boolean) {
  if (destructiveBusy.value) return
  bulkError.value = ''
  feed.toggleSelection(message.id, selected)
}

function requestBulkAction(action: PendingBulkAction) {
  if (destructiveBusy.value) return
  if (action === 'batch-delete' && feed.selectedCount.value < 1) return
  bulkActionError.value = ''
  bulkError.value = ''
  pendingBulkAction.value = action
}

function closeBulkAction() {
  if (bulkLoading.value) return
  pendingBulkAction.value = null
  bulkActionError.value = ''
}

async function confirmBulkAction() {
  const action = pendingBulkAction.value
  if (!action || destructiveBusy.value) return
  const operationSubjectRevision = viewSubjectRevision
  const selected = [...feed.selectedIds.value]
  bulkLoading.value = true
  bulkActionError.value = ''
  bulkError.value = ''
  try {
    if (action === 'batch-delete') await feed.batchDelete(selected)
    else await feed.clearAll()
    if (operationSubjectRevision !== viewSubjectRevision) return
    pendingBulkAction.value = null
    await nextTick()
    listRef.value?.focusLog()
  } catch (error) {
    if (operationSubjectRevision !== viewSubjectRevision) return
    const context = action === 'batch-delete' ? 'batch-delete' : 'clear'
    const message = chatErrorMessage(error, context)
    if (action === 'clear' && error instanceof ApiError && error.code === 'chat_clear_conflict') {
      pendingBulkAction.value = null
      bulkError.value = message
    } else {
      bulkActionError.value = message
    }
  } finally {
    if (operationSubjectRevision === viewSubjectRevision) bulkLoading.value = false
  }
}

async function retrySync() {
  if (retryingSync.value) return
  const operationSubjectRevision = viewSubjectRevision
  retryingSync.value = true
  try {
    if (feed.resetBlocked.value) await feed.reloadAfterSyncFailure()
    else await feed.syncNow()
  } finally {
    if (operationSubjectRevision === viewSubjectRevision) retryingSync.value = false
  }
}

onMounted(() => void feed.start())
onBeforeUnmount(feed.dispose)
</script>

<template>
  <section ref="pageRef" class="page-stack chat-page">
    <header class="page-header chat-page-header" data-motion>
      <h1 id="chat-page-title">在线交流</h1>
      <div class="chat-header-status" :data-state="feed.connectionState.value" role="status" aria-live="polite">
        <span class="chat-status-icon" aria-hidden="true"><AppIcon :name="connectionIcon" :size="21" /></span>
        <strong>{{ connectionLabel }}</strong>
      </div>
    </header>

    <section
      class="panel chat-stage"
      data-motion
      role="region"
      aria-labelledby="chat-page-title"
      :aria-busy="feed.initialLoading.value"
    >
      <div class="chat-stage-bar">
        <div class="chat-channel-mark">
          <span class="chat-live-dot" :data-state="feed.connectionState.value" aria-hidden="true" />
          <strong>聊天室</strong>
        </div>
        <div v-if="adminProjection" class="chat-bulk-toolbar" aria-label="聊天批量操作">
          <span class="chat-selection-count">已选 {{ feed.selectedCount.value }}</span>
          <button class="ghost-btn" type="button" :disabled="destructiveBusy || feed.selectedCount.value < 1" @click="requestBulkAction('batch-delete')">删除所选</button>
          <button class="ghost-btn danger" type="button" :disabled="destructiveBusy || !feed.initialized.value || feed.messages.value.length < 1" @click="requestBulkAction('clear')">清空全部</button>
        </div>
      </div>

      <div v-if="bulkError || feed.selectionError.value" class="chat-bulk-error" role="alert">
        {{ bulkError || feed.selectionError.value }}
      </div>

      <Transition name="chat-notice">
        <div v-if="feed.syncWarning.value" class="chat-sync-warning" role="alert">
          <AppIcon name="wifi-off" :size="19" />
          <span>{{ feed.syncWarning.value }}</span>
          <button class="ghost-btn" type="button" :disabled="retryingSync" @click="retrySync">
            {{ retryingSync ? '正在重试…' : feed.resetBlocked.value ? '重新加载' : '立即重试' }}
          </button>
        </div>
      </Transition>
      <Transition name="chat-notice">
        <div v-if="feed.refreshNotice.value" class="chat-refresh-notice" role="status">
          <AppIcon name="refresh" :size="17" />
          {{ feed.refreshNotice.value }}
        </div>
      </Transition>

      <div class="chat-feed-area">
        <StateBlock v-if="feed.initialLoading.value" loading title="正在读取聊天记录" />
        <StateBlock
          v-else-if="feed.initialError.value"
          :error="feed.initialError.value"
          retry-label="重新加载"
          @retry="feed.retryInitial"
        />
        <ChatMessageList
          v-else
          ref="listRef"
          :messages="feed.messages.value"
          :admin="adminProjection"
          :has-more="feed.hasMore.value"
          :loading-older="feed.loadingOlder.value"
          :older-error="feed.olderError.value"
          :new-message-count="newMessageCount"
          :selected-ids="feed.selectedIds.value"
          :selection-disabled="destructiveBusy"
          :selection-at-limit="feed.selectionAtLimit.value"
          :actions-disabled="destructiveBusy"
          @load-older="handleLoadOlder"
          @jump-latest="jumpToLatest"
          @bottom-change="onBottomChange"
          @withdraw="requestAction('withdraw', $event)"
          @delete="requestAction('delete', $event)"
          @selection-change="handleSelection"
        />
      </div>

      <ChatComposer
        ref="composerRef"
        v-model="draft"
        :capabilities="feed.capabilities.value"
        :capabilities-loading="feed.capabilitiesLoading.value"
        :capabilities-error="feed.capabilitiesError.value"
        :sending="sending"
        :error="sendError"
        :disabled="!feed.initialized.value"
        @send="handleSend"
        @retry-capabilities="feed.retryCapabilities"
      />
    </section>

    <ConfirmDialog
      :open="Boolean(pendingAction)"
      :title="dialogTitle"
      :message="dialogMessage"
      :detail="dialogDetail"
      :confirm-label="pendingAction?.kind === 'delete' ? '确认删除' : '确认撤回'"
      :error="actionError"
      :loading="actionLoading"
      danger
      @cancel="closeAction"
      @confirm="confirmAction"
    />

    <ConfirmDialog
      :open="Boolean(pendingBulkAction)"
      :title="bulkDialogTitle"
      :message="bulkDialogMessage"
      :confirm-label="bulkConfirmLabel"
      :error="bulkActionError"
      :loading="bulkLoading"
      danger
      @cancel="closeBulkAction"
      @confirm="confirmBulkAction"
    />
  </section>
</template>
