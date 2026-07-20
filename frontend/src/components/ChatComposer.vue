<script setup lang="ts">
import { computed, nextTick, onMounted, ref, useId, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import { measureChatDraft } from '@/chatDraft'
import type { ChatCapabilities } from '@/types'

const props = withDefaults(defineProps<{
  modelValue: string
  capabilities: ChatCapabilities | null
  capabilitiesLoading?: boolean
  capabilitiesError?: string
  sending?: boolean
  error?: string
  disabled?: boolean
}>(), {
  capabilitiesLoading: false,
  capabilitiesError: '',
  sending: false,
  error: '',
  disabled: false,
})

const emit = defineEmits<{
  'update:modelValue': [value: string]
  send: [body: string]
  'retry-capabilities': []
}>()

const textareaRef = ref<HTMLTextAreaElement | null>(null)
const composing = ref(false)
const id = useId()
const inputId = `chat-composer-${id}`
const hintId = `${inputId}-hint`
const validationId = `${inputId}-validation`
const errorId = `${inputId}-error`
const metrics = computed(() => measureChatDraft(props.modelValue, props.capabilities))
const disabled = computed(() => props.disabled || props.sending || !metrics.value.sendable || props.capabilitiesLoading || Boolean(props.capabilitiesError))
const describedBy = computed(() => [hintId, metrics.value.errors.length && !metrics.value.empty ? validationId : '', props.error ? errorId : ''].filter(Boolean).join(' '))

function resizeTextarea() {
  const textarea = textareaRef.value
  if (!textarea) return
  textarea.style.height = 'auto'
  textarea.style.height = `${Math.min(textarea.scrollHeight, 164)}px`
}

function updateValue(event: Event) {
  emit('update:modelValue', (event.target as HTMLTextAreaElement).value)
  void nextTick(resizeTextarea)
}

function submit() {
  if (disabled.value) return
  emit('send', metrics.value.body)
}

function handleKeydown(event: KeyboardEvent) {
  if (event.key !== 'Enter' || event.shiftKey || event.isComposing || composing.value) return
  event.preventDefault()
  submit()
}

function focus() {
  textareaRef.value?.focus({ preventScroll: true })
}

watch(() => props.modelValue, () => void nextTick(resizeTextarea))
onMounted(resizeTextarea)

defineExpose({ focus })
</script>

<template>
  <form class="chat-composer" aria-label="发送聊天消息" @submit.prevent="submit">
    <div v-if="capabilitiesError" class="chat-capability-error" role="alert">
      <span>{{ capabilitiesError }}</span>
      <button class="ghost-btn" type="button" :disabled="capabilitiesLoading" @click="$emit('retry-capabilities')">
        <AppIcon name="refresh" :size="17" />
        {{ capabilitiesLoading ? '读取中…' : '重试' }}
      </button>
    </div>

    <label class="chat-compose-field" :for="inputId">
      <span class="visually-hidden">消息内容</span>
      <textarea
        :id="inputId"
        ref="textareaRef"
        class="chat-textarea"
        :value="modelValue"
        rows="1"
        placeholder="输入纯文本消息…"
        :disabled="sending || props.disabled"
        :aria-invalid="metrics.errors.length > 0 && !metrics.empty"
        :aria-describedby="describedBy"
        @input="updateValue"
        @keydown="handleKeydown"
        @compositionstart="composing = true"
        @compositionend="composing = false"
      />
    </label>

    <div class="chat-compose-footer">
      <div class="chat-compose-guidance">
        <p :id="hintId">Enter 发送 · Shift + Enter 换行</p>
        <div v-if="capabilities" class="chat-metrics" aria-label="消息大小">
          <span :data-over="metrics.characterExceeded">{{ metrics.characters }} / {{ capabilities.maxMessageChars }} 字符</span>
          <span :data-over="metrics.bodyBytesExceeded">{{ metrics.bodyBytes }} / {{ capabilities.maxMessageBytes }} 正文字节</span>
          <span :data-over="metrics.requestBytesExceeded">{{ metrics.requestBytes }} / {{ capabilities.maxRequestBytes }} 请求字节</span>
        </div>
        <p v-else-if="capabilitiesLoading" class="chat-limit-state" role="status">正在读取服务器发送限制…</p>
        <p v-if="metrics.errors.length && !metrics.empty" :id="validationId" class="field-error" role="alert">{{ metrics.errors.join(' ') }}</p>
        <p v-if="error" :id="errorId" class="field-error" role="alert">{{ error }}</p>
      </div>
      <button class="primary-btn chat-send-button" type="submit" :disabled="disabled" aria-label="发送消息">
        <span>{{ sending ? '发送中…' : '发送' }}</span>
        <AppIcon name="send" :size="19" />
      </button>
    </div>
  </form>
</template>
