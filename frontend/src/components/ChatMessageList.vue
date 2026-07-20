<script setup lang="ts">
import { nextTick, onMounted, ref } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import ChatMessageCard from '@/components/ChatMessage.vue'
import EmptyState from '@/components/EmptyState.vue'
import type { ChatMessage } from '@/types'

const props = withDefaults(defineProps<{
  messages: ChatMessage[]
  admin: boolean
  hasMore?: boolean
  loadingOlder?: boolean
  olderError?: string
  newMessageCount?: number
}>(), {
  hasMore: false,
  loadingOlder: false,
  olderError: '',
  newMessageCount: 0,
})

const emit = defineEmits<{
  'load-older': []
  'jump-latest': []
  withdraw: [message: ChatMessage]
  delete: [message: ChatMessage]
  'bottom-change': [nearBottom: boolean]
}>()

const scrollRef = ref<HTMLElement | null>(null)
const liveReady = ref(false)

function reducedMotion() {
  return typeof window !== 'undefined' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
}

function isNearBottom(threshold = 112) {
  const element = scrollRef.value
  if (!element) return true
  return element.scrollHeight - element.scrollTop - element.clientHeight <= threshold
}

function scrollToBottom(smooth = true) {
  const element = scrollRef.value
  if (!element) return
  const top = element.scrollHeight
  if (smooth && !reducedMotion() && typeof element.scrollTo === 'function') {
    element.scrollTo({ top, behavior: 'smooth' })
  } else {
    element.scrollTop = top
  }
  emit('bottom-change', true)
}

function focusLog() {
  scrollRef.value?.focus({ preventScroll: true })
}

async function loadOlderPreservingPosition(load: () => Promise<boolean>) {
  const element = scrollRef.value
  if (!element) return load()
  const previousHeight = element.scrollHeight
  const previousTop = element.scrollTop
  const focused = document.activeElement instanceof HTMLElement ? document.activeElement : null
  const loaded = await load()
  if (!loaded) return false
  await nextTick()
  element.scrollTop = previousTop + (element.scrollHeight - previousHeight)
  if (focused?.isConnected) focused.focus({ preventScroll: true })
  else element.focus({ preventScroll: true })
  return true
}

function onScroll() {
  emit('bottom-change', isNearBottom())
}

onMounted(async () => {
  await nextTick()
  scrollToBottom(false)
  await nextTick()
  // 初始历史已经就位后再开启 live，避免读屏器逐条播报旧消息。
  liveReady.value = true
})

defineExpose({ isNearBottom, scrollToBottom, loadOlderPreservingPosition, focusLog, scrollElement: scrollRef })
</script>

<template>
  <div class="chat-log-frame">
    <div
      ref="scrollRef"
      class="chat-message-log"
      role="log"
      :aria-live="liveReady ? 'polite' : 'off'"
      aria-relevant="additions text"
      aria-atomic="false"
      aria-label="聊天消息"
      tabindex="0"
      @scroll.passive="onScroll"
    >
      <div class="chat-history-control">
        <button v-if="hasMore" class="chat-load-older" type="button" :disabled="loadingOlder" @click="emit('load-older')">
          <span v-if="loadingOlder" class="loader" aria-hidden="true" />
          <AppIcon v-else name="arrow-up" :size="17" />
          {{ loadingOlder ? '正在加载更早消息…' : '加载更早消息' }}
        </button>
        <span v-else-if="messages.length" class="chat-history-start">已到达聊天记录起点</span>
        <div v-if="olderError" class="chat-inline-error" role="alert">{{ olderError }}</div>
      </div>

      <div v-if="messages.length" class="chat-message-stack">
        <ChatMessageCard
          v-for="message in messages"
          :key="message.id"
          :message="message"
          :admin="admin"
          @withdraw="emit('withdraw', $event)"
          @delete="emit('delete', $event)"
        />
      </div>
      <EmptyState v-else title="还没有消息" description="发送第一条纯文本消息，开始这段交流。" icon="message-circle" />
    </div>

    <Transition name="chat-new-message">
      <button v-if="newMessageCount > 0" class="chat-new-message-button" type="button" @click="emit('jump-latest')">
        <AppIcon name="chevron-down" :size="18" />
        {{ newMessageCount === 1 ? '有新消息' : `有 ${newMessageCount} 条新消息` }}
      </button>
    </Transition>
  </div>
</template>
