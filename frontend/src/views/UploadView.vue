<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo } from '@/types'
import { formatBytes } from '@/utils'

interface UploadItem {
  id: string
  file: File
  status: 'queued' | 'uploading' | 'success' | 'error'
  error?: string
}

const route = useRoute()
const dirs = ref<DirectoryInfo[]>([])
const selectedDirId = ref('')
const targetPath = ref('')
const fileInput = ref<HTMLInputElement | null>(null)
const loading = ref(true)
const dragOver = ref(false)
const error = ref('')
const notice = ref('')
const uploadQueue = ref<UploadItem[]>([])
let uploadCounter = 0

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))
const canUpload = computed(() => Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const hasPendingUploads = computed(() => uploadQueue.value.some((item) => item.status === 'queued' || item.status === 'error'))
const uploading = computed(() => uploadQueue.value.some((item) => item.status === 'uploading'))
// 返回浏览页时带回当前目录和上传路径，用户可直接检查刚上传的位置。
const filesRoute = computed(() => ({ name: 'files', query: { dirId: selectedDirId.value, path: targetPath.value } }))
let restoringInitialQuery = true

async function loadDirs() {
  loading.value = true
  error.value = ''
  try {
    dirs.value = await api.dirs()
    const queryDir = String(route.query.dirId || '')
    selectedDirId.value = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value[0]?.id || ''
    targetPath.value = String(route.query.path || '')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '目录加载失败。'
  } finally {
    loading.value = false
  }
}

function addUploadFiles(files: FileList | File[]) {
  if (!canUpload.value) {
    notice.value = '当前目录不允许上传。'
    return
  }
  if (uploading.value) {
    // 当前实现逐项串行上传，禁止上传中追加文件，避免队列状态和目标路径发生竞态。
    notice.value = '正在上传，请等待当前队列完成后再追加文件。'
    return
  }
  Array.from(files).forEach((file) => {
    uploadQueue.value.push({ id: `local-${Date.now()}-${++uploadCounter}`, file, status: 'queued' })
  })
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.files?.length) addUploadFiles(input.files)
  input.value = ''
}

function chooseFiles() {
  // 拖拽区整块可点击，但实际仍复用隐藏 file input，以保留浏览器原生文件选择能力。
  if (!canUpload.value || uploading.value) return
  fileInput.value?.click()
}

function onDrop(event: DragEvent) {
  event.preventDefault()
  dragOver.value = false
  if (!canUpload.value || uploading.value) return
  if (event.dataTransfer?.files?.length) addUploadFiles(event.dataTransfer.files)
}

async function uploadItem(item: UploadItem) {
  if (!selectedDirId.value) return
  if (item.status === 'uploading' || item.status === 'success' || uploading.value) return
  item.status = 'uploading'
  item.error = undefined
  try {
    // 队列逐个调用单文件接口，失败项可以单独重试，不影响已完成项。
    await api.uploadOne(selectedDirId.value, targetPath.value, item.file)
    item.status = 'success'
    notice.value = `已上传 ${item.file.name}`
  } catch (err) {
    item.status = 'error'
    item.error = err instanceof ApiError ? err.message : '上传失败，可稍后重试。'
  }
}

async function uploadAll() {
  if (uploading.value) return
  for (const item of uploadQueue.value.filter((item) => item.status === 'queued' || item.status === 'error')) {
    await uploadItem(item)
  }
}

function removeUpload(id: string) {
  uploadQueue.value = uploadQueue.value.filter((item) => item.id !== id)
}

watch(selectedDirId, () => {
  if (restoringInitialQuery) {
    restoringInitialQuery = false
    return
  }
  // 用户主动切换目录时清空旧路径，避免把上一个目录的子路径误用于新目录。
  targetPath.value = ''
  uploadQueue.value = []
  notice.value = ''
})

onMounted(loadDirs)
</script>

<template>
  <section class="page-stack">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Upload</p>
        <h1>文件上传</h1>
        <p>上传操作独立在这里完成，文件浏览页会保持更清爽的列表视图。</p>
      </div>
      <div class="header-actions">
        <RouterLink class="ghost-btn" :to="filesRoute">查看此目录文件</RouterLink>
        <GlassSelect v-model="selectedDirId" class="page-select" :options="dirOptions" aria-label="选择上传目录" placeholder="选择目录" />
      </div>
    </header>

    <StateBlock :loading="loading" :error="error" />
    <div v-if="notice" class="alert success">{{ notice }}</div>

    <EmptyState v-if="!loading && !dirs.length" title="还没有可用目录" description="请先在后端配置可上传目录。" />

    <template v-else-if="selectedDir">
      <div class="panel upload-target-card">
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || selectedDir.root || '已配置目录' }}</p>
        </div>
        <label class="upload-path-field">
          <span>上传路径</span>
          <input v-model.trim="targetPath" placeholder="空为目录根路径，或填写 subdir" :disabled="uploading" />
        </label>
      </div>

      <div class="panel upload-panel">
        <div
          class="dropzone compact"
          :class="{ over: dragOver }"
          role="button"
          tabindex="0"
          @dragover.prevent="dragOver = !uploading"
          @dragleave="dragOver = false"
          @drop="onDrop"
          @click="chooseFiles"
          @keydown.enter.prevent="chooseFiles"
          @keydown.space.prevent="chooseFiles"
        >
          <div class="dropzone-symbol" aria-hidden="true"><span /></div>
          <div class="dropzone-copy">
            <strong>拖拽文件到此上传</strong>
            <small>或点击按钮选择文件，队列支持失败重试</small>
          </div>
          <label class="upload-btn" @click.stop>
            选择文件
            <input ref="fileInput" type="file" multiple :disabled="uploading || !canUpload" @click.stop @change="onFileChange" />
          </label>
        </div>

        <div v-if="!canUpload" class="alert error">当前目录不允许上传，请切换到允许上传的目录。</div>

        <ul v-if="uploadQueue.length" class="upload-queue">
          <li v-for="item in uploadQueue" :key="item.id" :data-status="item.status">
            <div class="q-file">
              <strong>{{ item.file.name }}</strong>
              <small>{{ formatBytes(item.file.size) }} · {{ item.status === 'queued' ? '待上传' : item.status === 'uploading' ? '上传中…' : item.status === 'success' ? '已完成' : item.error }}</small>
            </div>
            <div class="q-actions">
              <button v-if="item.status === 'error'" class="mini-btn" type="button" :disabled="uploading" @click="uploadItem(item)">重试</button>
              <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" @click="removeUpload(item.id)">移除</button>
            </div>
          </li>
        </ul>

        <button v-if="uploadQueue.length" class="primary-btn" type="button" :disabled="!hasPendingUploads || uploading || !canUpload" @click="uploadAll">
          {{ uploading ? '上传中…' : '上传队列' }}
        </button>
      </div>
    </template>
  </section>
</template>
