<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'

export interface GlassSelectOption {
  label: string
  value: string | number
  hint?: string
}

const props = defineProps<{
  modelValue: string | number
  options: GlassSelectOption[]
  placeholder?: string
  ariaLabel?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string | number]
}>()

const open = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLButtonElement | null>(null)
const activeIndex = ref(-1)
// 每个实例生成独立 listbox id，保证同页多个下拉框的 aria-controls 不冲突。
const listId = `glass-select-${Math.random().toString(36).slice(2)}`

const selected = computed(() => props.options.find((option) => String(option.value) === String(props.modelValue)))
const selectedIndex = computed(() => props.options.findIndex((option) => String(option.value) === String(props.modelValue)))

watch(open, async (value) => {
  if (!value) return
  activeIndex.value = selectedIndex.value >= 0 ? selectedIndex.value : 0
  await nextTick()
  // 打开后把焦点移到当前选项，键盘用户可直接上下移动或回车确认。
  root.value?.querySelector<HTMLButtonElement>('[data-active="true"]')?.focus()
})

function choose(value: string | number) {
  emit('update:modelValue', value)
  close(true)
}

function optionId(index: number) {
  return `${listId}-option-${index}`
}

function close(restoreFocus = false) {
  open.value = false
  if (restoreFocus) nextTick(() => trigger.value?.focus())
}

function toggle() {
  open.value = !open.value
}

function move(step: number) {
  if (!open.value) {
    open.value = true
    return
  }
  const total = props.options.length
  if (!total) return
  activeIndex.value = (activeIndex.value + step + total) % total
  nextTick(() => root.value?.querySelector<HTMLButtonElement>('[data-active="true"]')?.focus())
}

function chooseActive() {
  const option = props.options[activeIndex.value]
  if (option) choose(option.value)
}

function closeOnOutside(event: MouseEvent) {
  // 点击组件外部收起菜单，避免浮层残留挡住后续操作。
  const target = event.target as Node | null
  if (root.value && target && !root.value.contains(target)) close()
}

function handleKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape' && open.value) {
    event.preventDefault()
    close(true)
  }
}

function handleFocusout(event: FocusEvent) {
  // 键盘 Tab 离开整个组件时也收起，保持原生 select 的交互预期。
  const next = event.relatedTarget as Node | null
  if (root.value && (!next || !root.value.contains(next))) close()
}

onMounted(() => {
  document.addEventListener('click', closeOnOutside)
  document.addEventListener('keydown', handleKeydown)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', closeOnOutside)
  document.removeEventListener('keydown', handleKeydown)
})
</script>

<template>
  <div ref="root" class="glass-select" :class="{ open }" @focusout="handleFocusout">
    <button
      ref="trigger"
      class="glass-select-trigger"
      type="button"
      :aria-label="ariaLabel || placeholder || '选择'"
      :aria-expanded="open"
      :aria-controls="listId"
      :aria-activedescendant="open && activeIndex >= 0 ? optionId(activeIndex) : undefined"
      aria-haspopup="listbox"
      @click="toggle"
      @keydown.down.prevent="move(1)"
      @keydown.up.prevent="move(-1)"
      @keydown.enter.prevent="toggle"
      @keydown.space.prevent="toggle"
    >
      <span class="glass-select-text">
        <strong>{{ selected?.label || placeholder || '请选择' }}</strong>
        <small v-if="selected?.hint">{{ selected.hint }}</small>
      </span>
      <span class="glass-select-arrow" aria-hidden="true" />
    </button>

      <Transition name="select-pop">
      <div v-if="open" :id="listId" class="glass-select-menu" role="listbox" @click.stop>
        <button
          v-for="(option, index) in options"
          :key="String(option.value)"
          :id="optionId(index)"
          class="glass-select-option"
          :class="{ selected: String(option.value) === String(modelValue) }"
          type="button"
          role="option"
          :aria-selected="String(option.value) === String(modelValue)"
          :data-active="index === activeIndex"
          @keydown.down.prevent="move(1)"
          @keydown.up.prevent="move(-1)"
          @keydown.enter.prevent="chooseActive"
          @keydown.space.prevent="chooseActive"
          @click="choose(option.value)"
        >
          <span>
            <strong>{{ option.label }}</strong>
            <small v-if="option.hint">{{ option.hint }}</small>
          </span>
          <span v-if="String(option.value) === String(modelValue)" class="glass-select-check">✓</span>
        </button>
      </div>
    </Transition>
  </div>
</template>
