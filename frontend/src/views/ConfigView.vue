<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo } from '@/types'

const dirs = ref<DirectoryInfo[]>([])
const loading = ref(true)
const error = ref('')

async function load() {
  loading.value = true
  error.value = ''
  // 管理员可看到 root 字段，普通用户进入该页前已被路由守卫拦回文件浏览。
  try { dirs.value = await api.dirs() }
  catch (err) { error.value = err instanceof ApiError ? err.message : '配置加载失败。' }
  finally { loading.value = false }
}
onMounted(load)
</script>

<template>
  <section class="page-stack">
    <header class="page-header"><p class="eyebrow">Configuration</p><h1>配置 / 目录概览</h1><p>集中查看后端开放的目录与权限，便于确认传输边界。</p></header>
    <StateBlock :loading="loading" :error="error" />
    <div v-if="dirs.length" class="cards-grid">
      <article v-for="dir in dirs" :key="dir.id" class="config-card">
        <div class="config-icon">{{ (dir.label || dir.name).slice(0, 1) }}</div>
        <div>
          <h2>{{ dir.label || dir.name }}</h2>
          <p>{{ dir.description || '后端已注册目录' }}</p>
          <code>{{ dir.root || dir.id }}</code>
          <div class="pill-row">
            <span class="pill" :class="(dir.canDownload !== false && dir.allowDownload !== false) ? 'ok' : 'muted'">下载</span>
            <span class="pill" :class="(dir.canUpload || dir.allowUpload) ? 'ok' : 'muted'">上传</span>
            <span v-if="dir.readonly" class="pill danger">只读</span>
          </div>
        </div>
      </article>
    </div>
    <EmptyState v-else-if="!loading" title="没有目录配置" description="请在后端配置文件中添加目录。" />
  </section>
</template>
