<script setup lang="ts">
withDefaults(defineProps<{
  loading?: boolean
  error?: string
  title?: string
  retryLabel?: string
}>(), {
  retryLabel: '',
})

defineEmits<{ retry: [] }>()
</script>

<template>
  <div v-if="loading" class="state-block" role="status" aria-live="polite">
    <span class="loader" aria-hidden="true" />
    <strong>{{ title || '正在加载' }}</strong>
  </div>
  <div v-else-if="error" class="alert error state-error" role="alert">
    <span>{{ error }}</span>
    <button v-if="retryLabel" class="ghost-btn" type="button" @click="$emit('retry')">{{ retryLabel }}</button>
  </div>
</template>
