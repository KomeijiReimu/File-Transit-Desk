<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import { isAdmin } from '@/auth'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, FileEntry } from '@/types'
import { useGsapEntrance } from '@/useGsapEntrance'
import { buildTransferUrl, formatBytes, formatDate, joinPath, parentPath } from '@/utils'

const pageRef = ref<HTMLElement | null>(null)
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
const canUpload = computed(() => responseCanUpload.value ?? Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const canDownload = computed(() => responseCanDownload.value ?? (selectedDir.value ? selectedDir.value.canDownload !== false && selectedDir.value.allowDownload !== false : false))
const selectedIsFileResource = computed(() => selectedDir.value?.type === 'file')
const crumbs = computed(() => [{ name: selectedDir.value?.name || '目录', path: '' }, ...currentPath.value.split('/').filter(Boolean).map((part, index, arr) => ({ name: part, path: arr.slice(0, index + 1).join('/') }))])
const motionKey = computed(() => (!loading.value && dirs.value.length ? `${dirs.value.length}:${selectedDirId.value || 'none'}` : ''))

useGsapEntrance(pageRef, { refreshKey: () => motionKey.value })

function tokenQuery(type: 'download' | 'upload', entry?: FileEntry) {
  const path = entry ? (entry.path || joinPath(currentPath.value, entry.name)) : currentPath.value
  return { type, dirId: selectedDirId.value, path }
}

function hasEntryAction(entry: FileEntry) {
  if (!entryIsDir(entry) && canDownload.value) return true
  if (isAdmin.value && entryIsDir(entry) && canUpload.value) return true
  return false
}

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
    selectedDirId.value = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value.length === 1 ? dirs.value[0]?.id || '' : ''
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

function selectResource(dir: DirectoryInfo) {
  if (selectedDirId.value === dir.id && !currentPath.value) return
  if (selectedDirId.value === dir.id) {
    currentPath.value = ''
    return
  }
  selectedDirId.value = dir.id
}

function resourceDescription(dir: DirectoryInfo) {
  return dir.description || (dir.type === 'file' ? '单文件共享' : '共享目录')
}

function resourcePermissionLabel(dir: DirectoryInfo) {
  const upload = Boolean(dir.canUpload ?? dir.allowUpload)
  const download = dir.canDownload !== false && dir.allowDownload !== false
  if (upload && download) return '可上传 · 可下载'
  if (upload) return '可上传'
  if (download) return '可下载'
  return '只读配置'
}

async function startDownload(entry: FileEntry) {
  if (!selectedDirId.value || entryIsDir(entry) || downloadingPath.value) return
  const path = entry.path || joinPath(currentPath.value, entry.name)
  downloadingPath.value = path
  error.value = ''
  try {
    const lease = await api.createDownloadLease(selectedDirId.value, path)
    // 下载使用独立票据地址，页面会话随后空闲过期也不会中断已授权文件传输。
    window.location.assign(buildTransferUrl(lease.url))
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
  entries.value = []
  if (selectedDirId.value) await loadFiles()
})
watch(currentPath, () => {
  if (restoringQuery) return
  loadFiles()
})
onMounted(loadDirs)
</script>

<template>
  <section ref="pageRef" class="page-stack files-page">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Files</p>
        <h1>文件浏览</h1>
        <p>像打开文件夹一样进入共享位置，再沿路径查看文件。</p>
      </div>
    </header>

    <StateBlock :loading="loading" :error="error" />

    <EmptyState v-if="!loading && !dirs.length" title="还没有可用目录" description="请先在后端配置可访问目录。" />

    <div v-if="!loading && dirs.length" class="resource-browser" data-motion>
      <button
        v-for="dir in dirs"
        :key="dir.id"
        class="resource-tile"
        type="button"
        :data-active="dir.id === selectedDirId"
        :data-type="dir.type === 'file' ? 'file' : 'directory'"
        @click="selectResource(dir)"
      >
        <span class="resource-icon" aria-hidden="true"><span /></span>
        <span class="resource-copy">
          <strong>{{ dir.label || dir.name }}</strong>
          <small>{{ resourceDescription(dir) }}</small>
          <small>{{ resourcePermissionLabel(dir) }}</small>
        </span>
      </button>
    </div>

    <EmptyState v-if="!loading && dirs.length && !selectedDir" title="请选择共享位置" description="点击上方文件夹即可进入对应位置。" />

    <template v-if="selectedDir">
      <div class="panel dir-summary" data-motion>
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || selectedDir.root || '已配置目录' }}</p>
        </div>
        <div class="pill-row">
          <span class="pill" :class="selectedIsFileResource ? 'ok' : ''">{{ selectedIsFileResource ? '单文件' : '目录' }}</span>
          <span class="pill" :class="canDownload ? 'ok' : 'muted'">{{ canDownload ? '允许下载' : '禁止下载' }}</span>
          <span class="pill" :class="canUpload ? 'ok' : 'muted'">{{ canUpload ? '允许上传' : '禁止上传' }}</span>
        </div>
      </div>

      <div class="toolbar panel" data-motion>
        <nav class="breadcrumbs" aria-label="面包屑">
          <button v-for="crumb in crumbs" :key="crumb.path || 'root'" type="button" @click="currentPath = crumb.path">
            {{ crumb.name }}
          </button>
        </nav>
        <div class="toolbar-actions">
          <RouterLink v-if="canUpload" class="ghost-btn" :to="{ name: 'upload', query: { dirId: selectedDirId, path: currentPath } }">上传到此处</RouterLink>
          <RouterLink v-if="isAdmin && canUpload" class="ghost-btn" :to="{ name: 'tokens', query: tokenQuery('upload') }">创建上传分享</RouterLink>
          <button class="ghost-btn" :disabled="!currentPath" @click="currentPath = parentPath(currentPath)">返回上级</button>
        </div>
      </div>

      <div class="table-card" data-motion>
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
                <div class="row-actions">
                  <button v-if="!entryIsDir(entry) && canDownload" class="mini-btn" type="button" :disabled="Boolean(downloadingPath)" @click="startDownload(entry)">
                    {{ downloadingPath === (entry.path || joinPath(currentPath, entry.name)) ? '准备中…' : '下载' }}
                  </button>
                  <RouterLink v-if="isAdmin && !entryIsDir(entry) && canDownload" class="mini-btn" :to="{ name: 'tokens', query: tokenQuery('download', entry) }">创建分享</RouterLink>
                  <RouterLink v-if="isAdmin && entryIsDir(entry) && canUpload" class="mini-btn" :to="{ name: 'tokens', query: tokenQuery('upload', entry) }">上传分享</RouterLink>
                  <span v-if="!hasEntryAction(entry)" class="muted-text">—</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState v-else-if="!loading" title="当前路径为空" description="这里暂时没有文件或子目录。" />
      </div>
    </template>
  </section>
</template>
