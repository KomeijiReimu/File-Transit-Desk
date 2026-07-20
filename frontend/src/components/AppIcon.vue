<script setup lang="ts">
export type AppIconName =
  | 'inbox'
  | 'alert-triangle'
  | 'download'
  | 'folder'
  | 'file'
  | 'link'
  | 'folder-cog'
  | 'file-cog'
  | 'message-circle'
  | 'send'
  | 'arrow-up'
  | 'trash'
  | 'undo'
  | 'shield-check'
  | 'wifi-off'
  | 'chevron-down'
  | 'refresh'

withDefaults(defineProps<{
  name: AppIconName
  size?: number | string
}>(), {
  size: 20,
})
</script>

<template>
  <svg
    class="app-icon"
    :width="size"
    :height="size"
    viewBox="0 0 24 24"
    fill="none"
    stroke="currentColor"
    stroke-width="1.8"
    stroke-linecap="round"
    stroke-linejoin="round"
    aria-hidden="true"
    focusable="false"
  >
    <template v-if="name === 'inbox'">
      <path d="M4 13.5 6.6 5.8A2.6 2.6 0 0 1 9.1 4h5.8a2.6 2.6 0 0 1 2.5 1.8l2.6 7.7" />
      <path d="M4 13.5V17a3 3 0 0 0 3 3h10a3 3 0 0 0 3-3v-3.5" />
      <path d="M8 14h2.1a2 2 0 0 1 3.8 0H16" />
    </template>
    <template v-else-if="name === 'alert-triangle'">
      <path d="M10.4 4.4 2.8 17.6A2.2 2.2 0 0 0 4.7 21h14.6a2.2 2.2 0 0 0 1.9-3.4L13.6 4.4a1.85 1.85 0 0 0-3.2 0Z" />
      <path d="M12 9v4" />
      <path d="M12 17h.01" />
    </template>
    <template v-else-if="name === 'download'">
      <path d="M12 3v11" />
      <path d="m7 10 5 5 5-5" />
      <path d="M5 20h14" />
    </template>
    <template v-else-if="name === 'folder'">
      <path d="M3.5 6.8A2.3 2.3 0 0 1 5.8 4.5h4l2 2h6.4a2.3 2.3 0 0 1 2.3 2.3v8.4a2.3 2.3 0 0 1-2.3 2.3H5.8a2.3 2.3 0 0 1-2.3-2.3Z" />
    </template>
    <template v-else-if="name === 'file'">
      <path d="M14 3.5H7.4A2.4 2.4 0 0 0 5 5.9v12.2a2.4 2.4 0 0 0 2.4 2.4h9.2a2.4 2.4 0 0 0 2.4-2.4V8.5Z" />
      <path d="M14 3.5v5h5" />
    </template>
    <template v-else-if="name === 'link'">
      <path d="M9.5 14.5 14.5 9.5" />
      <path d="M11 6.8 12.7 5a4 4 0 0 1 5.7 5.7l-1.8 1.8" />
      <path d="m13 17.2-1.7 1.8a4 4 0 0 1-5.7-5.7l1.8-1.8" />
    </template>
    <template v-else-if="name === 'folder-cog'">
      <path d="M3.5 7A2.4 2.4 0 0 1 5.9 4.6h3.8l2 2h6.4a2.4 2.4 0 0 1 2.4 2.4v3.3" />
      <path d="M3.5 8.7v8.1a2.5 2.5 0 0 0 2.5 2.5h6.5" />
      <path d="M17.5 14.2v1.1" />
      <path d="M17.5 20.7v1.1" />
      <path d="m14.7 15.8 1 .6" />
      <path d="m19.3 18.6 1 .6" />
      <path d="m14.7 20.2 1-.6" />
      <path d="m19.3 17.4 1-.6" />
      <circle cx="17.5" cy="18" r="2" />
    </template>
    <template v-else-if="name === 'file-cog'">
      <path d="M13.5 3.5H7.4A2.4 2.4 0 0 0 5 5.9v12.2a2.4 2.4 0 0 0 2.4 2.4h4.2" />
      <path d="M13.5 3.5v5H18" />
      <path d="M18 8.6v3.2" />
      <path d="M17.5 14.2v1.1" />
      <path d="M17.5 20.7v1.1" />
      <path d="m14.7 15.8 1 .6" />
      <path d="m19.3 18.6 1 .6" />
      <path d="m14.7 20.2 1-.6" />
      <path d="m19.3 17.4 1-.6" />
      <circle cx="17.5" cy="18" r="2" />
    </template>
    <template v-else-if="name === 'message-circle'">
      <path d="M3 16.3a2 2 0 0 1 .1 1.2L2 20.8A1 1 0 0 0 3.3 22l3.4-1a2 2 0 0 1 1.1.1A10 10 0 1 0 3 16.3Z" />
    </template>
    <template v-else-if="name === 'send'">
      <path d="M3.7 3a.5.5 0 0 0-.7.7l2.9 7.6a2 2 0 0 1 0 1.4L3 20.3a.5.5 0 0 0 .7.7l18-8.5a.5.5 0 0 0 0-.9Z" />
      <path d="M6 12h16" />
    </template>
    <template v-else-if="name === 'arrow-up'">
      <path d="m5 12 7-7 7 7" />
      <path d="M12 19V5" />
    </template>
    <template v-else-if="name === 'trash'">
      <path d="M10 11v6M14 11v6M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
    </template>
    <template v-else-if="name === 'undo'">
      <path d="m9 14-5-5 5-5" />
      <path d="M4 9h10.5a5.5 5.5 0 0 1 0 11H11" />
    </template>
    <template v-else-if="name === 'shield-check'">
      <path d="M20 13c0 5-3.5 7.5-7.7 9a1 1 0 0 1-.6 0C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.2-2.7a1.2 1.2 0 0 1 1.6 0C14.5 3.8 17 5 19 5a1 1 0 0 1 1 1Z" />
      <path d="m9 12 2 2 4-4" />
    </template>
    <template v-else-if="name === 'wifi-off'">
      <path d="M12 20h.01M8.5 16.4a5 5 0 0 1 7 0M5 12.9a10 10 0 0 1 5.2-2.7M19 12.9a10 10 0 0 0-2-1.6M2 8.8a15 15 0 0 1 4.2-2.6M22 8.8A15 15 0 0 0 10.7 5.1M2 2l20 20" />
    </template>
    <template v-else-if="name === 'chevron-down'">
      <path d="m6 9 6 6 6-6" />
    </template>
    <template v-else-if="name === 'refresh'">
      <path d="M3 12a9 9 0 0 1 15.7-6.3L21 8M21 3v5h-5M21 12a9 9 0 0 1-15.7 6.3L3 16M3 21v-5h5" />
    </template>
    <template v-else>
      <circle cx="12" cy="12" r="8" />
      <path d="M12 8v4l2.5 2" />
    </template>
  </svg>
</template>
