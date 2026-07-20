<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import type { ChatMessage } from '@/types'

const props = defineProps<{
  message: ChatMessage
  admin: boolean
}>()

const emit = defineEmits<{
  withdraw: [message: ChatMessage]
  delete: [message: ChatMessage]
}>()

const now = ref(Date.now())
let countdownTimer: number | undefined

const withdrawSeconds = computed(() => {
  if (!props.message.withdrawUntil) return 0
  const until = Date.parse(props.message.withdrawUntil)
  if (!Number.isFinite(until)) return 0
  return Math.max(0, Math.ceil((until - now.value) / 1000))
})
const showWithdraw = computed(() => !props.admin
  && props.message.isMine
  && props.message.status === 'active'
  && Boolean(props.message.withdrawUntil))
const withdrawEnabled = computed(() => showWithdraw.value && props.message.canWithdraw && withdrawSeconds.value > 0)
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

function countdownText(seconds: number) {
  if (seconds <= 0) return '撤回已过期'
  const minutes = Math.floor(seconds / 60)
  const rest = seconds % 60
  return `撤回 · ${String(minutes).padStart(2, '0')}:${String(rest).padStart(2, '0')}`
}

function syncCountdownTimer() {
  if (countdownTimer !== undefined) window.clearInterval(countdownTimer)
  countdownTimer = undefined
  now.value = Date.now()
  if (!showWithdraw.value || withdrawSeconds.value <= 0) return
  countdownTimer = window.setInterval(() => {
    now.value = Date.now()
    if (withdrawSeconds.value <= 0 && countdownTimer !== undefined) {
      window.clearInterval(countdownTimer)
      countdownTimer = undefined
    }
  }, 1_000)
}

watch(() => [props.message.status, props.message.canWithdraw, props.message.withdrawUntil], syncCountdownTimer)
onMounted(syncCountdownTimer)
onBeforeUnmount(() => {
  if (countdownTimer !== undefined) window.clearInterval(countdownTimer)
})
</script>

<template>
  <article
    class="chat-message"
    :class="{ mine: message.isMine, admin: message.role === 'admin', tombstone: message.status !== 'active' }"
    :data-status="message.status"
    :aria-label="`${message.authorTag} 的消息`"
  >
    <div class="chat-avatar" :data-role="message.role" aria-hidden="true">
      <AppIcon v-if="message.role === 'admin'" name="shield-check" :size="18" />
      <span v-else>{{ authorInitial }}</span>
    </div>

    <div class="chat-bubble">
      <header class="chat-message-meta">
        <span class="chat-author">{{ message.isMine ? '你' : message.authorTag }}</span>
        <span v-if="message.role === 'admin'" class="chat-role-badge">管理员</span>
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

      <footer v-if="message.status !== 'deleted' && (admin && message.sourceIP || showWithdraw || canDelete)" class="chat-message-footer">
        <span v-if="admin && message.sourceIP" class="chat-source-ip">来源 {{ message.sourceIP }}</span>
        <span v-else />
        <div class="chat-message-actions">
          <button
            v-if="showWithdraw"
            class="chat-action-button"
            type="button"
            :disabled="!withdrawEnabled"
            :aria-label="withdrawEnabled ? `撤回消息，剩余 ${withdrawSeconds} 秒` : '消息已超过可撤回时间'"
            @click="emit('withdraw', message)"
          >
            <AppIcon name="undo" :size="16" />
            {{ countdownText(withdrawSeconds) }}
          </button>
          <button v-if="canDelete" class="chat-action-button danger" type="button" @click="emit('delete', message)">
            <AppIcon name="trash" :size="16" />
            删除
          </button>
        </div>
      </footer>
    </div>
  </article>
</template>
