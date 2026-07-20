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
const projectionLabel = computed(() => adminProjection.value ? '管理员视图' : '用户视图')
const projectionDescription = computed(() => adminProjection.value
  ? '可查看用户撤回前的内容与来源 IP，也可以删除消息。'
  : '消息撤回或删除后，内容会按权限隐藏。')
const dialogTitle = computed(() => pendingAction.value?.kind === 'delete' ? '删除这条消息？' : '撤回这条消息？')
const dialogMessage = computed(() => pendingAction.value?.kind === 'delete'
  ? '删除后，所有人只会看到管理员删除提示，正文将不再显示。'
  : '撤回后，其他用户只会看到一条撤回提示。')
const dialogDetail = computed(() => pendingAction.value?.kind === 'delete'
  ? '管理员可以删除正常消息或已经撤回的消息，此操作不可恢复。'
  : '撤回仅在服务器给出的时间窗口内有效。')

watch(draft, () => { sendError.value = '' })

function clearSubjectInteractionState() {
  pendingAction.value = null
  actionError.value = ''
  actionLoading.value = false
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
      <div>
        <p class="eyebrow">仅限登录用户</p>
        <h1 id="chat-page-title">在线交流</h1>
        <p>在这里交流文件传输相关事项。撤回和删除后的内容会按权限显示。</p>
      </div>
      <div class="chat-header-status" :data-state="feed.connectionState.value" role="status" aria-live="polite">
        <span class="chat-status-icon" aria-hidden="true"><AppIcon :name="connectionIcon" :size="21" /></span>
        <span><strong>{{ connectionLabel }}</strong><small>约每 3 秒自动检查新消息</small></span>
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
          <span><strong>聊天室</strong><small>仅当前已登录用户可进入</small></span>
        </div>
        <div class="chat-projection-note">
          <strong>{{ projectionLabel }}</strong>
          <span>{{ projectionDescription }}</span>
        </div>
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
          @load-older="handleLoadOlder"
          @jump-latest="jumpToLatest"
          @bottom-change="onBottomChange"
          @withdraw="requestAction('withdraw', $event)"
          @delete="requestAction('delete', $event)"
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
  </section>
</template>
