<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import AppIcon from '@/components/AppIcon.vue'
import { acquireModalIsolation } from '@/useModalIsolation'

const props = withDefaults(defineProps<{
  open: boolean
  title: string
  message: string
  detail?: string
  confirmLabel?: string
  cancelLabel?: string
  error?: string
  danger?: boolean
  loading?: boolean
}>(), {
  detail: '',
  confirmLabel: '确认',
  cancelLabel: '取消',
  error: '',
  danger: false,
  loading: false,
})

const emit = defineEmits<{
  cancel: []
  confirm: []
}>()

const dialogRef = ref<HTMLElement | null>(null)
const cancelRef = ref<HTMLButtonElement | null>(null)
let returnFocusEl: HTMLElement | null = null
let releaseModalIsolation: (() => void) | undefined
const instanceId = `confirm-${Math.random().toString(36).slice(2)}`
const titleId = `${instanceId}-title`
const messageId = `${instanceId}-message`
const detailId = `${instanceId}-detail`

function cancel() {
  if (!props.loading) emit('cancel')
}

function submitConfirm() {
  if (!props.loading) emit('confirm')
}

function onKeydown(event: KeyboardEvent) {
  if (!props.open) return
  if (event.key === 'Escape') {
    event.preventDefault()
    cancel()
    return
  }
  if (event.key !== 'Tab') return
  // 弹窗打开时把 Tab 焦点限制在内部，避免键盘用户误操作背景页面。
  const focusable = dialogRef.value?.querySelectorAll<HTMLElement>('button:not(:disabled), [href], input:not(:disabled), [tabindex]:not([tabindex="-1"])')
  if (!focusable?.length) {
    event.preventDefault()
    dialogRef.value?.focus()
    return
  }
  const first = focusable[0]
  const last = focusable[focusable.length - 1]
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

watch(() => props.open, async (open) => {
  if (!open) {
    releaseModalIsolation?.()
    releaseModalIsolation = undefined
    await nextTick()
    returnFocusEl?.focus()
    returnFocusEl = null
    return
  }
  returnFocusEl = document.activeElement instanceof HTMLElement ? document.activeElement : null
  releaseModalIsolation?.()
  releaseModalIsolation = acquireModalIsolation()
  await nextTick()
  // 危险确认默认聚焦取消按钮，降低误按 Enter 直接删除的风险。
  cancelRef.value?.focus()
})

watch(() => props.open, (open) => {
  if (open) document.addEventListener('keydown', onKeydown)
  else document.removeEventListener('keydown', onKeydown)
}, { immediate: true })

onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  releaseModalIsolation?.()
})
</script>

<template>
  <Teleport to="body">
    <Transition name="dialog-pop">
      <div v-if="open" class="dialog-backdrop" @click.self="cancel">
        <section
          ref="dialogRef"
          class="confirm-dialog"
          :class="{ danger }"
          role="dialog"
          aria-modal="true"
          :aria-labelledby="titleId"
          :aria-describedby="detail ? `${messageId} ${detailId}` : messageId"
          :aria-busy="loading"
          tabindex="-1"
        >
          <div class="confirm-icon" aria-hidden="true"><AppIcon name="alert-triangle" :size="26" /></div>
          <div class="confirm-copy">
            <h2 :id="titleId">{{ title }}</h2>
            <p :id="messageId">{{ message }}</p>
            <small v-if="detail" :id="detailId">{{ detail }}</small>
            <div v-if="error" class="dialog-error" role="alert">{{ error }}</div>
          </div>
          <div class="confirm-actions">
            <button ref="cancelRef" class="ghost-btn" type="button" :disabled="loading" @click="cancel">{{ cancelLabel }}</button>
            <button class="primary-btn" :class="{ danger }" type="button" :disabled="loading" @click="submitConfirm">
              {{ loading ? '处理中…' : confirmLabel }}
            </button>
          </div>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
