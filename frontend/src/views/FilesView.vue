<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { ApiError, api, downloadUrl } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, FileEntry } from '@/types'
import { formatBytes, formatDate, joinPath, parentPath } from '@/utils'

interface UploadItem {
  id: string
  file: File
  status: 'queued' | 'uploading' | 'success' | 'error'
  error?: string
}

const dirs = ref<DirectoryInfo[]>([])
const selectedDirId = ref('')
const currentPath = ref('')
const entries = ref<FileEntry[]>([])
const loading = ref(true)
const dragOver = ref(false)
const error = ref('')
const notice = ref('')
const uploadQueue = ref<UploadItem[]>([])
const responseCanUpload = ref<boolean | null>(null)
const responseCanDownload = ref<boolean | null>(null)
let uploadCounter = 0

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))
const canUpload = computed(() => responseCanUpload.value ?? Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const canDownload = computed(() => responseCanDownload.value ?? (selectedDir.value ? selectedDir.value.canDownload !== false && selectedDir.value.allowDownload !== false : false))
const hasPendingUploads = computed(() => uploadQueue.value.some((item) => item.status === 'queued' || item.status === 'error'))
const uploading = computed(() => uploadQueue.value.some((item) => item.status === 'uploading'))
const crumbs = computed(() => [{ name: selectedDir.value?.name || '目录', path: '' }, ...currentPath.value.split('/').filter(Boolean).map((part, index, arr) => ({ name: part, path: arr.slice(0, index + 1).join('/') }))])

function entryIsDir(entry: FileEntry) {
  return entry.isDir || entry.type === 'dir' || entry.type === 'directory'
}

async function loadDirs() {
  loading.value = true
  error.value = ''
  try {
    dirs.value = await api.dirs()
    selectedDirId.value = dirs.value[0]?.id || ''
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '目录加载失败。'
  } finally {
    loading.value = false
  }
}

async function loadFiles(clearNotice = true) {
  if (!selectedDirId.value) return
  loading.value = true
  error.value = ''
  if (clearNotice) notice.value = ''
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

function addUploadFiles(files: FileList | File[]) {
  Array.from(files).forEach((file) => {
    uploadQueue.value.push({ id: `local-${Date.now()}-${++uploadCounter}`, file, status: 'queued' })
  })
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.files?.length) addUploadFiles(input.files)
  input.value = ''
}

function onDrop(event: DragEvent) {
  event.preventDefault()
  dragOver.value = false
  if (event.dataTransfer?.files?.length) addUploadFiles(event.dataTransfer.files)
}

async function uploadItem(item: UploadItem) {
  if (!selectedDirId.value) return
  item.status = 'uploading'
  item.error = undefined
  try {
    await api.uploadOne(selectedDirId.value, currentPath.value, item.file)
    item.status = 'success'
    await loadFiles(false)
    notice.value = `已上传 ${item.file.name}`
  } catch (err) {
    item.status = 'error'
    item.error = err instanceof ApiError ? err.message : '上传失败，可稍后重试。'
  }
}

async function uploadAll() {
  for (const item of uploadQueue.value.filter((item) => item.status === 'queued' || item.status === 'error')) {
    await uploadItem(item)
  }
}

function removeUpload(id: string) {
  uploadQueue.value = uploadQueue.value.filter((item) => item.id !== id)
}

watch(selectedDirId, async () => {
  currentPath.value = ''
  uploadQueue.value = []
  responseCanUpload.value = null
  responseCanDownload.value = null
  if (selectedDirId.value) await loadFiles()
})
watch(currentPath, () => {
  uploadQueue.value = []
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
        <p>选择目录，浏览路径，按权限下载或上传临时文件。</p>
      </div>
      <GlassSelect v-model="selectedDirId" class="page-select" :options="dirOptions" aria-label="选择目录" placeholder="选择目录" />
    </header>

    <StateBlock :loading="loading" :error="error" />
    <div v-if="notice" class="alert success">{{ notice }}</div>

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
          <button class="ghost-btn" :disabled="!currentPath" @click="currentPath = parentPath(currentPath)">返回上级</button>
        </div>
      </div>

      <div v-if="canUpload" class="panel upload-panel">
        <div
          class="dropzone compact"
          :class="{ over: dragOver }"
          @dragover.prevent="dragOver = true"
          @dragleave="dragOver = false"
          @drop="onDrop"
        >
          <div class="dropzone-symbol" aria-hidden="true"><span /></div>
          <div><strong>拖拽文件到此上传</strong><small>或点击按钮选择文件，队列支持失败重试</small></div>
          <label class="upload-btn">
            选择文件
            <input type="file" multiple :disabled="uploading" @change="onFileChange" />
          </label>
        </div>
        <ul v-if="uploadQueue.length" class="upload-queue">
          <li v-for="item in uploadQueue" :key="item.id" :data-status="item.status">
            <div class="q-file">
              <strong>{{ item.file.name }}</strong>
              <small>{{ formatBytes(item.file.size) }} · {{ item.status === 'queued' ? '待上传' : item.status === 'uploading' ? '上传中…' : item.status === 'success' ? '已完成' : item.error }}</small>
            </div>
            <div class="q-actions">
              <button v-if="item.status === 'error'" class="mini-btn" type="button" @click="uploadItem(item)">重试</button>
              <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" @click="removeUpload(item.id)">移除</button>
            </div>
          </li>
        </ul>
        <button v-if="uploadQueue.length" class="primary-btn" type="button" :disabled="!hasPendingUploads || uploading" @click="uploadAll">
          {{ uploading ? '上传中…' : '上传队列' }}
        </button>
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
                <a v-if="!entryIsDir(entry) && canDownload" class="mini-btn" :href="downloadUrl(selectedDirId, entry.path || joinPath(currentPath, entry.name))">下载</a>
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
