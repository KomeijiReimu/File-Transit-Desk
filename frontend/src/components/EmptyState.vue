<script setup lang="ts">
import AppIcon from '@/components/AppIcon.vue'
import type { AppIconName } from '@/components/AppIcon.vue'

defineProps<{ title: string; description?: string; icon?: AppIconName }>()
</script>

<template>
  <div class="empty-state">
    <div class="empty-icon" aria-hidden="true"><AppIcon :name="icon || 'inbox'" :size="28" /></div>
    <h3>{{ title }}</h3>
    <p v-if="description">{{ description }}</p>
    <slot />
  </div>
</template>
