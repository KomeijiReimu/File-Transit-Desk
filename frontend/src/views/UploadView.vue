<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { DirectoryInfo, UploadLeaseResponse, UploadLimits } from '@/types'
import { setUploadSessionHold } from '@/useSessionActivity'
import { formatBytes, formatDuration, formatSpeed } from '@/utils'

interface UploadItem {
  id: string
  file: File
  status: 'queued' | 'uploading' | 'success' | 'error'
  progress: number
  loaded: number
  total: number
  error?: string
  controller?: AbortController
  uploadLease?: UploadLeaseResponse
  speedBps?: number
  averageSpeedBps?: number
  etaSeconds?: number
  startedAt?: number
  lastLoaded?: number
  lastProgressAt?: number
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
const sessionNotice = ref('')
const limits = ref<UploadLimits | null>(null)
const uploadQueue = ref<UploadItem[]>([])
const uploadBatchActive = ref(false)
let uploadCounter = 0
let stopUploadBatch = false

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))
const canUpload = computed(() => Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const hasPendingUploads = computed(() => uploadQueue.value.some((item) => item.status === 'queued' || item.status === 'error'))
const uploading = computed(() => uploadQueue.value.some((item) => item.status === 'uploading'))
const currentUpload = computed(() => uploadQueue.value.find((item) => item.status === 'uploading'))
const totalBytes = computed(() => uploadQueue.value.reduce((sum, item) => sum + item.file.size, 0))
const finishedBytes = computed(() => uploadQueue.value.reduce((sum, item) => sum + (item.status === 'success' ? item.file.size : item.status === 'uploading' ? item.loaded : 0), 0))
const finishedCount = computed(() => uploadQueue.value.filter((item) => item.status === 'success').length)
const overallProgress = computed(() => totalBytes.value > 0 ? Math.min(100, Math.round((finishedBytes.value / totalBytes.value) * 100)) : 0)
const currentSpeed = computed(() => uploadQueue.value.reduce((sum, item) => sum + (item.status === 'uploading' ? item.speedBps || 0 : 0), 0))
// 返回浏览页时带回当前目录和上传路径，用户可直接检查刚上传的位置。
const filesRoute = computed(() => ({ name: 'files', query: { dirId: selectedDirId.value, path: targetPath.value } }))
let restoringInitialQuery = true
let uploadHeartbeatTimer: number | undefined

async function loadDirs() {
  loading.value = true
  error.value = ''
  try {
    const [allDirs, uploadLimits] = await Promise.all([api.dirs(), api.uploadLimits()])
    limits.value = uploadLimits
    // 上传页只展示真正允许上传的目录资源；单文件资源和禁用上传的目录不出现在这里。
    dirs.value = allDirs.filter((dir) => dir.type !== 'file' && Boolean(dir.canUpload ?? dir.allowUpload))
    const queryDir = String(route.query.dirId || '')
    selectedDirId.value = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value[0]?.id || ''
    targetPath.value = String(route.query.path || '')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '目录加载失败。'
  } finally {
    loading.value = false
  }
}

function fileExtension(name: string) {
  const index = name.lastIndexOf('.')
  return index > -1 ? name.slice(index).toLowerCase() : ''
}

function validateFiles(files: File[]) {
  const policy = limits.value
  if (!policy) return ''
  for (const file of files) {
    const ext = fileExtension(file.name)
    if (policy.blockedExtensions.includes(ext)) return `“${file.name}” 的扩展名不允许上传。`
    if (policy.allowedExtensions.length && !policy.allowedExtensions.includes(ext)) return `“${file.name}” 不在允许上传的扩展名范围内。`
    if (file.size > policy.uploadMaxFileBytes) return `“${file.name}” 大小为 ${formatBytes(file.size)}，超过单文件上限 ${formatBytes(policy.uploadMaxFileBytes)}。`
    if (file.size > policy.uploadMaxBytes) return `“${file.name}” 超过单次上传上限 ${formatBytes(policy.uploadMaxBytes)}。`
  }
  return ''
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
  const list = Array.from(files)
  const validationError = validateFiles(list)
  if (validationError) {
    error.value = validationError
    return
  }
  error.value = ''
  list.forEach((file) => {
    uploadQueue.value.push({ id: `local-${Date.now()}-${++uploadCounter}`, file, status: 'queued', progress: 0, loaded: 0, total: file.size })
  })
}

function uploadErrorMessage(err: unknown) {
  if (err instanceof ApiError) {
    if ((err.details as { aborted?: boolean } | undefined)?.aborted) return '上传已取消。'
    if (err.status === 413) return err.message || '文件超过上传大小限制。'
    if (err.status === 0) return err.message
    return err.message
  }
  return '上传失败，可稍后重试。'
}

function updateUploadProgress(item: UploadItem, loaded: number, total: number, percent: number) {
  const now = Date.now()
  if (!item.startedAt) item.startedAt = now
  const previousLoaded = item.lastLoaded ?? 0
  const previousAt = item.lastProgressAt ?? now
  const hasPrevious = Boolean(item.lastProgressAt)
  const deltaSeconds = Math.max(0.001, (now - previousAt) / 1000)
  const instant = hasPrevious ? Math.max(0, (loaded - previousLoaded) / deltaSeconds) : 0
  item.speedBps = hasPrevious ? (item.speedBps ? item.speedBps * 0.7 + instant * 0.3 : instant) : 0
  item.averageSpeedBps = loaded > 0 ? loaded / Math.max(0.001, (now - item.startedAt) / 1000) : 0
  item.loaded = total > 0 ? Math.min(loaded, total) : loaded
  item.total = total || item.file.size
  item.progress = percent
  if (item.averageSpeedBps > 0 && item.total > item.loaded) item.etaSeconds = (item.total - item.loaded) / item.averageSpeedBps
  item.lastLoaded = loaded
  item.lastProgressAt = now
}

async function ensureUploadLease(item: UploadItem) {
  if (item.uploadLease) return
  const lease = await api.createUploadLease({ dirId: selectedDirId.value, path: targetPath.value, fileName: item.file.name, fileSize: item.file.size })
  item.uploadLease = lease
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
  item.progress = 0
  item.loaded = 0
  item.total = item.file.size
  item.speedBps = 0
  item.averageSpeedBps = 0
  item.etaSeconds = undefined
  item.startedAt = undefined
  item.lastLoaded = 0
  item.lastProgressAt = undefined
  item.error = undefined
  const controller = new AbortController()
  item.controller = controller
  try {
    await ensureUploadLease(item)
    // 队列逐个调用单文件接口，失败项可以单独重试，不影响已完成项。
    await api.uploadByLease(item.uploadLease!, item.file, {
      signal: controller.signal,
      onProgress: (progress) => {
        updateUploadProgress(item, progress.loaded, progress.total || item.file.size, progress.percent)
      },
    })
    item.progress = 100
    item.loaded = item.file.size
    item.status = 'success'
  } catch (err) {
    item.status = 'error'
    item.error = uploadErrorMessage(err)
    item.uploadLease = undefined
  } finally {
    item.controller = undefined
  }
}

function cancelUpload(item: UploadItem) {
  stopUploadBatch = true
  item.controller?.abort()
}

function cancelCurrentUpload() {
  if (currentUpload.value) cancelUpload(currentUpload.value)
}

async function uploadAll() {
  if (uploading.value) return
  stopUploadBatch = false
  uploadBatchActive.value = true
  setUploadSessionHold(true)
  notice.value = ''
  sessionNotice.value = ''
  const pending = uploadQueue.value.filter((item) => item.status === 'queued' || item.status === 'error')
  for (const item of pending) {
    if (stopUploadBatch) break
    await uploadItem(item)
    if (sessionNotice.value && uploadQueue.value.some((entry) => entry.status === 'queued' || entry.status === 'error')) {
      error.value = '登录状态已过期，当前已授权文件处理完成；队列中未授权文件需要重新登录后继续。'
      break
    }
  }
  if (!uploadQueue.value.some((item) => item.status === 'error') && uploadQueue.value.some((item) => item.status === 'success')) {
    notice.value = '上传完成。'
  }
  if (sessionNotice.value) {
    window.dispatchEvent(new Event('ft:auth-expired'))
    notice.value = '上传完成。登录状态已过期，请重新登录后查看文件。'
  }
  uploadBatchActive.value = false
  setUploadSessionHold(false)
}

async function heartbeatDuringUpload() {
  try {
    await api.heartbeat()
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) {
      sessionNotice.value = '登录状态已过期，已授权的当前上传会继续；上传完成后请重新登录查看文件。'
    }
  }
}

function beforeUnload(event: BeforeUnloadEvent) {
  if (!uploading.value && !uploadBatchActive.value) return
  event.preventDefault()
  event.returnValue = '文件正在上传，关闭页面会中断上传。'
}

function handleUploadSessionExpired() {
  if (uploading.value || uploadBatchActive.value) sessionNotice.value = '登录状态已过期，已授权的当前上传会继续；上传完成后请重新登录查看文件。'
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
onMounted(() => {
  window.addEventListener('beforeunload', beforeUnload)
  window.addEventListener('ft:upload-session-expired', handleUploadSessionExpired)
})
onUnmounted(() => {
  window.removeEventListener('beforeunload', beforeUnload)
  window.removeEventListener('ft:upload-session-expired', handleUploadSessionExpired)
  if (uploadHeartbeatTimer) window.clearInterval(uploadHeartbeatTimer)
  uploadBatchActive.value = false
  setUploadSessionHold(false)
})

watch(uploading, (active) => {
  if (uploadHeartbeatTimer) {
    window.clearInterval(uploadHeartbeatTimer)
    uploadHeartbeatTimer = undefined
  }
  if (active) {
    setUploadSessionHold(true)
    heartbeatDuringUpload()
    uploadHeartbeatTimer = window.setInterval(heartbeatDuringUpload, 30_000)
  } else if (!uploadBatchActive.value) {
    setUploadSessionHold(false)
  }
})
</script>

<template>
  <section class="page-stack">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Upload</p>
        <h1>文件上传</h1>
        <p>只显示可上传的位置；上传过程可随时取消，并会显示明确进度和错误原因。</p>
      </div>
      <div class="header-actions">
        <RouterLink class="ghost-btn" :to="filesRoute">查看此目录文件</RouterLink>
        <GlassSelect v-model="selectedDirId" class="page-select" :options="dirOptions" aria-label="选择上传目录" placeholder="选择目录" />
      </div>
    </header>

    <StateBlock :loading="loading" :error="error" />
    <div v-if="notice" class="alert success">{{ notice }}</div>
    <div v-if="sessionNotice" class="alert info">{{ sessionNotice }}</div>

    <EmptyState v-if="!loading && !dirs.length" title="还没有可用目录" description="请先在后端配置可上传目录。" />

    <template v-else-if="selectedDir">
      <div class="panel upload-target-card">
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || '已选择上传目标' }}</p>
        </div>
        <label class="upload-path-field">
          <span>上传路径</span>
          <input v-model="targetPath" placeholder="空为目录根路径，或填写 subdir" :disabled="uploading" />
        </label>
      </div>

      <div class="panel upload-panel">
        <div
          class="dropzone compact"
          :class="{ over: dragOver }"
          role="button"
          tabindex="0"
          :aria-disabled="uploading || !canUpload"
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

        <div v-if="limits" class="upload-limit-note">
          单文件上限 {{ formatBytes(limits.uploadMaxFileBytes) }}；单次请求上限 {{ formatBytes(limits.uploadMaxBytes) }}。
          每个文件开始传输前会建立临时授权；请不要关闭页面，关闭或断网会中断上传。
        </div>

        <ul v-if="uploadQueue.length" class="upload-queue">
          <li v-for="item in uploadQueue" :key="item.id" :data-status="item.status">
            <div class="q-file">
              <strong>{{ item.file.name }}</strong>
              <small>{{ formatBytes(item.file.size) }} · {{ item.status === 'queued' ? '待上传' : item.status === 'uploading' ? item.progress >= 100 ? '处理中…' : `上传中 ${item.progress}%` : item.status === 'success' ? '已完成' : item.error }}</small>
              <div class="upload-progress" role="progressbar" :aria-valuenow="item.status === 'success' ? 100 : item.progress" aria-valuemin="0" aria-valuemax="100" :aria-label="`${item.file.name} 上传进度 ${item.status === 'success' ? 100 : item.progress}%`">
                <span :style="{ width: `${item.status === 'success' ? 100 : item.progress}%` }" />
              </div>
              <small v-if="item.status === 'uploading'" class="upload-progress-text">
                {{ formatBytes(item.loaded) }} / {{ formatBytes(item.total || item.file.size) }} · {{ formatSpeed(item.speedBps) }} · 剩余 {{ formatDuration(item.etaSeconds) }}
              </small>
            </div>
            <div class="q-actions">
              <button v-if="item.status === 'error'" class="mini-btn" type="button" :disabled="uploading" @click="uploadItem(item)">重试</button>
              <button v-if="item.status === 'uploading'" class="mini-btn danger" type="button" @click="cancelUpload(item)">取消上传</button>
              <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" @click="removeUpload(item.id)">移除</button>
            </div>
          </li>
        </ul>

        <div v-if="uploadQueue.length" class="upload-summary">
          <div>
            <strong>{{ uploading ? `整体进度 ${overallProgress}%` : `已完成 ${finishedCount} / ${uploadQueue.length}` }}</strong>
            <small>{{ formatBytes(finishedBytes) }} / {{ formatBytes(totalBytes) }}<template v-if="uploading"> · {{ formatSpeed(currentSpeed) }}</template></small>
          </div>
          <div class="upload-progress wide" role="progressbar" :aria-valuenow="overallProgress" aria-valuemin="0" aria-valuemax="100" aria-label="整体上传进度"><span :style="{ width: `${overallProgress}%` }" /></div>
          <button class="primary-btn" type="button" :disabled="!hasPendingUploads || uploading || !canUpload" @click="uploadAll">
            {{ uploading ? '上传中…' : '开始上传' }}
          </button>
          <button v-if="uploading" class="ghost-btn danger" type="button" @click="cancelCurrentUpload">终止当前上传</button>
        </div>
      </div>
    </template>
  </section>
</template>
