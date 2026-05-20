<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, FileEntry } from '@/types'
import { formatBytes, formatDate, joinPath, parentPath } from '@/utils'

const dirs = ref<DirectoryInfo[]>([])
const route = useRoute()
const selectedDirId = ref('')
const currentPath = ref('')
const entries = ref<FileEntry[]>([])
const loading = ref(true)
const error = ref('')
const downloadingPath = ref('')
const responseCanUpload = ref<boolean | null>(null)
const responseCanDownload = ref<boolean | null>(null)
// 从上传页返回时会携带 dirId/path；恢复阶段暂停 watch，避免重复请求和路径被重置。
let restoringQuery = false

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))
const canUpload = computed(() => responseCanUpload.value ?? Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const canDownload = computed(() => responseCanDownload.value ?? (selectedDir.value ? selectedDir.value.canDownload !== false && selectedDir.value.allowDownload !== false : false))
const crumbs = computed(() => [{ name: selectedDir.value?.name || '目录', path: '' }, ...currentPath.value.split('/').filter(Boolean).map((part, index, arr) => ({ name: part, path: arr.slice(0, index + 1).join('/') }))])

function entryIsDir(entry: FileEntry) {
  return entry.isDir || entry.type === 'dir' || entry.type === 'directory'
}

async function loadDirs() {
  loading.value = true
  error.value = ''
  try {
    dirs.value = await api.dirs()
    const queryDir = String(route.query.dirId || '')
    restoringQuery = true
    selectedDirId.value = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value[0]?.id || ''
    currentPath.value = String(route.query.path || '')
    if (selectedDirId.value) await loadFiles()
    restoringQuery = false
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '目录加载失败。'
  } finally {
    loading.value = false
  }
}

async function loadFiles() {
  if (!selectedDirId.value) return
  loading.value = true
  error.value = ''
  try {
    const result = await api.listFiles(selectedDirId.value, currentPath.value)
    entries.value = result.entries || result.files || []
    responseCanUpload.value = typeof result.canUpload === 'boolean' ? result.canUpload : null
    responseCanDownload.value = typeof result.canDownload === 'boolean' ? result.canDownload : null
  } catch (err) {
    entries.value = []
    responseCanUpload.value = null
    responseCanDownload.value = null
    error.value = err instanceof ApiError ? err.message : '文件列表加载失败。'
  } finally {
    loading.value = false
  }
}

function openDir(entry: FileEntry) {
  currentPath.value = joinPath(currentPath.value, entry.name)
}

async function startDownload(entry: FileEntry) {
  if (!selectedDirId.value || entryIsDir(entry) || downloadingPath.value) return
  const path = entry.path || joinPath(currentPath.value, entry.name)
  downloadingPath.value = path
  error.value = ''
  try {
    const lease = await api.createDownloadLease(selectedDirId.value, path)
    // 下载使用独立票据地址，页面会话随后空闲过期也不会中断已授权文件传输。
    window.location.assign(lease.url)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '下载链接创建失败，请稍后重试。'
  } finally {
    downloadingPath.value = ''
  }
}

watch(selectedDirId, async () => {
  if (restoringQuery) return
  // 用户主动切换目录时回到根路径；从 query 恢复时不走这里，保留原路径上下文。
  currentPath.value = ''
  responseCanUpload.value = null
  responseCanDownload.value = null
  if (selectedDirId.value) await loadFiles()
})
watch(currentPath, () => {
  if (restoringQuery) return
  loadFiles()
})
onMounted(loadDirs)
</script>

<template>
  <section class="page-stack">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Files</p>
        <h1>文件浏览</h1>
        <p>选择目录并浏览路径，文件列表会直接显示在当前目录下方。</p>
      </div>
      <GlassSelect v-model="selectedDirId" class="page-select" :options="dirOptions" aria-label="选择目录" placeholder="选择目录" />
    </header>

    <StateBlock :loading="loading" :error="error" />

    <EmptyState v-if="!loading && !dirs.length" title="还没有可用目录" description="请先在后端配置可访问目录。" />

    <template v-else-if="selectedDir">
      <div class="panel dir-summary">
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || selectedDir.root || '已配置目录' }}</p>
        </div>
        <div class="pill-row">
          <span class="pill" :class="canDownload ? 'ok' : 'muted'">{{ canDownload ? '允许下载' : '禁止下载' }}</span>
          <span class="pill" :class="canUpload ? 'ok' : 'muted'">{{ canUpload ? '允许上传' : '禁止上传' }}</span>
        </div>
      </div>

      <div class="toolbar panel">
        <nav class="breadcrumbs" aria-label="面包屑">
          <button v-for="crumb in crumbs" :key="crumb.path || 'root'" type="button" @click="currentPath = crumb.path">
            {{ crumb.name }}
          </button>
        </nav>
        <div class="toolbar-actions">
          <RouterLink v-if="canUpload" class="ghost-btn" :to="{ name: 'upload', query: { dirId: selectedDirId, path: currentPath } }">上传到此处</RouterLink>
          <button class="ghost-btn" :disabled="!currentPath" @click="currentPath = parentPath(currentPath)">返回上级</button>
        </div>
      </div>

      <div class="table-card">
        <table v-if="entries.length" class="data-table">
          <thead><tr><th>名称</th><th>大小</th><th>修改时间</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="entry in entries" :key="entry.path || entry.name">
              <td data-label="名称">
                <button v-if="entryIsDir(entry)" class="link-cell file-name" @click="openDir(entry)">
                  <span class="file-type-ico folder" aria-hidden="true" />
                  <span>{{ entry.name }}</span>
                </button>
                <span v-else class="file-name">
                  <span class="file-type-ico file" aria-hidden="true" />
                  <span>{{ entry.name }}</span>
                </span>
              </td>
              <td data-label="大小">{{ entryIsDir(entry) ? '目录' : formatBytes(entry.size) }}</td>
              <td data-label="修改时间">{{ formatDate(entry.modifiedAt || entry.mtime) }}</td>
              <td data-label="操作">
                <button v-if="!entryIsDir(entry) && canDownload" class="mini-btn" type="button" :disabled="Boolean(downloadingPath)" @click="startDownload(entry)">
                  {{ downloadingPath === (entry.path || joinPath(currentPath, entry.name)) ? '准备中…' : '下载' }}
                </button>
                <span v-else class="muted-text">—</span>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState v-else-if="!loading" title="当前路径为空" description="这里暂时没有文件或子目录。" />
      </div>
    </template>
  </section>
</template>
