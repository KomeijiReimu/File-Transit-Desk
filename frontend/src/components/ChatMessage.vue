<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import type { ChatMessage } from '@/types'

const props = withDefaults(defineProps<{
  message: ChatMessage
  admin: boolean
  selectable?: boolean
  selected?: boolean
  selectionDisabled?: boolean
  actionsDisabled?: boolean
}>(), {
  selectable: false,
  selected: false,
  selectionDisabled: false,
  actionsDisabled: false,
})

const emit = defineEmits<{
  withdraw: [message: ChatMessage]
  delete: [message: ChatMessage]
  'selection-change': [message: ChatMessage, selected: boolean]
}>()

const withdrawAvailable = ref(false)
let expiryTimer: number | undefined

const showWithdraw = computed(() => !props.admin
  && props.message.isMine
  && props.message.status === 'active'
  && props.message.canWithdraw
  && withdrawAvailable.value)
const canDelete = computed(() => props.admin && props.message.status !== 'deleted')
const authorInitial = computed(() => props.message.role === 'admin' ? '管' : props.message.authorTag.slice(-1).toUpperCase())

const shortTime = new Intl.DateTimeFormat('zh-CN', {
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
})
const fullTime = new Intl.DateTimeFormat('zh-CN', {
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

function validDate(value: string | null) {
  if (!value) return null
  const date = new Date(value)
  return Number.isFinite(date.getTime()) ? date : null
}

function formatShort(value: string | null) {
  const date = validDate(value)
  return date ? shortTime.format(date) : '时间未知'
}

function formatFull(value: string | null) {
  const date = validDate(value)
  return date ? fullTime.format(date) : '时间未知'
}

function clearExpiryTimer() {
  if (expiryTimer !== undefined) window.clearTimeout(expiryTimer)
  expiryTimer = undefined
}

function scheduleWithdrawExpiry() {
  clearExpiryTimer()
  withdrawAvailable.value = false
  if (props.admin || !props.message.isMine || props.message.status !== 'active' || !props.message.canWithdraw) return
  const until = Date.parse(props.message.withdrawUntil || '')
  const remaining = until - Date.now()
  if (!Number.isFinite(until) || remaining <= 0) return
  withdrawAvailable.value = true
  const delay = Math.min(remaining, 2_147_483_647)
  expiryTimer = window.setTimeout(() => {
    expiryTimer = undefined
    if (remaining > 2_147_483_647) scheduleWithdrawExpiry()
    else withdrawAvailable.value = false
  }, delay)
}

watch(
  () => [props.admin, props.message.isMine, props.message.status, props.message.canWithdraw, props.message.withdrawUntil],
  scheduleWithdrawExpiry,
  { immediate: true },
)
onBeforeUnmount(clearExpiryTimer)
</script>

<template>
  <article
    class="chat-message"
    :class="{ mine: message.isMine, admin: message.role === 'admin', tombstone: message.status !== 'active', selected }"
    :data-status="message.status"
    :aria-label="`${message.authorTag} 的消息`"
  >
    <label v-if="selectable" class="chat-message-select" :class="{ selected }">
      <input
        type="checkbox"
        :checked="selected"
        :disabled="selectionDisabled"
        :aria-label="`选择 ${message.authorTag} 的消息`"
        @change="emit('selection-change', message, ($event.target as HTMLInputElement).checked)"
      />
    </label>
    <div class="chat-avatar" :data-role="message.role" aria-hidden="true">
      <AppIcon v-if="message.role === 'admin'" name="shield-check" :size="18" />
      <span v-else>{{ authorInitial }}</span>
    </div>

    <div class="chat-bubble">
      <header class="chat-message-meta">
        <span class="chat-author">{{ message.isMine ? '你' : message.authorTag }}</span>
        <span v-if="message.role === 'admin'" class="chat-role-badge">管理员</span>
        <span class="chat-source-ip">IP {{ message.sourceIP }}</span>
        <time :datetime="message.createdAt" :title="formatFull(message.createdAt)">{{ formatShort(message.createdAt) }}</time>
      </header>

      <div v-if="message.status === 'deleted'" class="chat-tombstone-copy">
        <AppIcon name="trash" :size="17" />
        <span>该消息已由管理员删除</span>
      </div>

      <template v-else-if="message.status === 'withdrawn'">
        <div v-if="admin" class="chat-admin-withdrawn">
          <div class="chat-tombstone-copy">
            <AppIcon name="undo" :size="17" />
            <span>用户已撤回 · 原文仅管理员可见</span>
          </div>
          <p v-if="message.body !== null" class="chat-message-body">{{ message.body }}</p>
          <small v-if="message.withdrawnAt">撤回于 {{ formatFull(message.withdrawnAt) }}</small>
        </div>
        <div v-else class="chat-tombstone-copy">
          <AppIcon name="undo" :size="17" />
          <span>{{ message.isMine ? '你撤回了一条消息' : '该消息已被撤回' }}</span>
        </div>
      </template>

      <p v-else class="chat-message-body">{{ message.body }}</p>

      <footer v-if="showWithdraw || canDelete" class="chat-message-footer">
        <div class="chat-message-actions">
          <button
            v-if="showWithdraw"
            class="chat-action-button"
            type="button"
            :disabled="actionsDisabled"
            aria-label="撤回消息"
            @click="emit('withdraw', message)"
          >
            <AppIcon name="undo" :size="16" />
            撤回
          </button>
          <button v-if="canDelete" class="chat-action-button danger" type="button" :disabled="actionsDisabled" @click="emit('delete', message)">
            <AppIcon name="trash" :size="16" />
            删除
          </button>
        </div>
      </footer>
    </div>
  </article>
</template>
