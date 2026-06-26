<script setup lang="ts">
defineProps<{ loading?: boolean; error?: string; title?: string }>()
</script>

<template>
  <div v-if="loading" class="state-block">
    <span class="loader" />
    <strong>{{ title || '正在加载' }}</strong>
  </div>
  <div v-else-if="error" class="alert error" role="alert">{{ error }}</div>
</template>
