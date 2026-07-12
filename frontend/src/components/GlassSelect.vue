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
  invalid?: boolean
  describedBy?: string
}>()

const emit = defineEmits<{
  'update:modelValue': [value: string | number]
}>()

const open = ref(false)
const root = ref<HTMLElement | null>(null)
const trigger = ref<HTMLButtonElement | null>(null)
const activeIndex = ref(-1)
const listId = `glass-select-${Math.random().toString(36).slice(2)}`
let typeahead = ''
let typeaheadTimer: number | undefined
let suppressNextTriggerClick = false
let suppressTriggerTimer: number | undefined

const selected = computed(() => props.options.find((option) => String(option.value) === String(props.modelValue)))
const selectedIndex = computed(() => props.options.findIndex((option) => String(option.value) === String(props.modelValue)))

watch(open, async (value) => {
  if (!value) return
  activeIndex.value = props.options.length ? (selectedIndex.value >= 0 ? selectedIndex.value : 0) : -1
  await nextTick()
  scrollActiveIntoView()
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

function handleTriggerClick(event: MouseEvent) {
  if (suppressNextTriggerClick) {
    event.preventDefault()
    event.stopPropagation()
    suppressNextTriggerClick = false
    if (suppressTriggerTimer) window.clearTimeout(suppressTriggerTimer)
    suppressTriggerTimer = undefined
    open.value = false
    return
  }
  toggle()
}

function handleOptionClick(event: MouseEvent, value: string | number) {
  event.preventDefault()
  event.stopPropagation()
  // 菜单在 click 期间卸载时，部分浏览器会把同一指针序列落到下方触发器；仅忽略这一次穿透 click，不阻止用户随后主动重开。
  suppressNextTriggerClick = true
  if (suppressTriggerTimer) window.clearTimeout(suppressTriggerTimer)
  suppressTriggerTimer = window.setTimeout(() => {
    suppressNextTriggerClick = false
    suppressTriggerTimer = undefined
  }, 0)
  choose(value)
}

function move(step: number) {
  if (!open.value) {
    open.value = true
    return
  }
  const total = props.options.length
  if (!total) return
  activeIndex.value = (activeIndex.value + step + total) % total
  void nextTick(scrollActiveIntoView)
}

function moveTo(index: number) {
  if (!props.options.length) return
  if (!open.value) open.value = true
  activeIndex.value = Math.max(0, Math.min(index, props.options.length - 1))
  void nextTick(scrollActiveIntoView)
}

function scrollActiveIntoView() {
  root.value?.querySelector<HTMLElement>('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
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

function handleTriggerKeydown(event: KeyboardEvent) {
  if (event.key === 'ArrowDown') {
    event.preventDefault()
    move(1)
  } else if (event.key === 'ArrowUp') {
    event.preventDefault()
    move(-1)
  } else if (event.key === 'Home') {
    event.preventDefault()
    moveTo(0)
  } else if (event.key === 'End') {
    event.preventDefault()
    moveTo(props.options.length - 1)
  } else if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault()
    if (open.value) chooseActive()
    else open.value = true
  } else if (event.key === 'Escape' && open.value) {
    event.preventDefault()
    close(true)
  } else if (event.key.length === 1 && !event.ctrlKey && !event.metaKey && !event.altKey) {
    typeahead += event.key.toLocaleLowerCase()
    if (typeaheadTimer) window.clearTimeout(typeaheadTimer)
    typeaheadTimer = window.setTimeout(() => { typeahead = '' }, 650)
    const index = props.options.findIndex((option) => option.label.toLocaleLowerCase().startsWith(typeahead))
    if (index >= 0) moveTo(index)
  }
}

function handleFocusout(event: FocusEvent) {
  // 键盘 Tab 离开整个组件时也收起，保持原生 select 的交互预期。
  const next = event.relatedTarget as Node | null
  if (root.value && (!next || !root.value.contains(next))) close()
}

onMounted(() => {
  document.addEventListener('click', closeOnOutside)
})

onBeforeUnmount(() => {
  document.removeEventListener('click', closeOnOutside)
  if (typeaheadTimer) window.clearTimeout(typeaheadTimer)
  if (suppressTriggerTimer) window.clearTimeout(suppressTriggerTimer)
})

defineExpose({ focus: () => trigger.value?.focus() })
</script>

<template>
  <div ref="root" class="glass-select" :class="{ open }" @focusout="handleFocusout">
    <button
      ref="trigger"
      class="glass-select-trigger"
      type="button"
      role="combobox"
      :aria-label="ariaLabel || placeholder || '选择'"
      :aria-expanded="open"
      :aria-controls="listId"
      :aria-activedescendant="open && activeIndex >= 0 ? optionId(activeIndex) : undefined"
      :aria-invalid="invalid || undefined"
      :aria-describedby="describedBy"
      aria-haspopup="listbox"
      @click="handleTriggerClick"
      @keydown="handleTriggerKeydown"
    >
      <span class="glass-select-text">
        <strong>{{ selected?.label || placeholder || '请选择' }}</strong>
        <small v-if="selected?.hint">{{ selected.hint }}</small>
      </span>
      <span class="glass-select-arrow" aria-hidden="true" />
    </button>

    <div v-if="open" :id="listId" class="glass-select-menu" role="listbox" :aria-label="ariaLabel || placeholder || '选择'" @click.stop>
        <div
          v-for="(option, index) in options"
          :key="String(option.value)"
          :id="optionId(index)"
          class="glass-select-option"
          :class="{ selected: String(option.value) === String(modelValue) }"
          role="option"
          :aria-selected="String(option.value) === String(modelValue)"
          :data-active="index === activeIndex"
          @mouseenter="activeIndex = index"
          @mousedown.stop.prevent
          @click="handleOptionClick($event, option.value)"
        >
          <span>
            <strong>{{ option.label }}</strong>
            <small v-if="option.hint">{{ option.hint }}</small>
          </span>
          <span v-if="String(option.value) === String(modelValue)" class="glass-select-check" aria-hidden="true">✓</span>
        </div>
    </div>
  </div>
</template>
