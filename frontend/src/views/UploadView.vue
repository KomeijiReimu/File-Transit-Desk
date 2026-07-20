<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import { currentSessionBinding } from '@/authEpoch'
import { createUploadItem, useUploadQueue } from '@/composables/useUploadQueue'
import type { DirectoryInfo, UploadLimits } from '@/types'
import { acquireUploadSessionHold } from '@/useSessionActivity'
import { formatBytes, formatDuration, formatSpeed } from '@/utils'

const route = useRoute()
const dirs = ref<DirectoryInfo[]>([])
const selectedDirId = ref('')
const targetPath = ref('')
const fileInput = ref<HTMLInputElement | null>(null)
const targetPathInput = ref<HTMLInputElement | null>(null)
const loading = ref(true)
const dragOver = ref(false)
const error = ref('')
const notice = ref('')
const sessionNotice = ref('')
const uploadSessionExpired = ref(false)
const uploadSubjectChanged = ref(false)
const limits = ref<UploadLimits | null>(null)
const pendingTargetDirId = ref('')
const pendingPathUnlock = ref(false)
let uploadCounter = 0

const {
  queue: uploadQueue,
  batchActive: uploadBatchActive,
  uploading,
  busy: uploadBusy,
  hasPendingUploads,
  totalBytes,
  finishedBytes,
  finishedCount,
  overallProgress,
  currentSpeed,
  add: addUploadItem,
  remove: removeUpload,
  clear: clearUploadQueue,
  uploadAll: runUploadAll,
  retry: uploadItem,
  cancel: cancelUpload,
  cancelCurrent: cancelCurrentUpload,
  dispose: disposeUploadQueue,
} = useUploadQueue({
  acquireHold: acquireUploadSessionHold,
  errorMessage: uploadErrorMessage,
  getBatchIdentity: currentSessionBinding,
  onBatchIdentityMismatch: ({ capturedIdentity }) => {
    markUploadSubjectChanged(!capturedIdentity)
  },
  uploadFile: async (item, options) => {
    if (!options.batchIdentity || currentSessionBinding() !== options.batchIdentity) {
      throw new ApiError('登录身份已变化，旧上传任务未执行。', 409, undefined, 'session_subject_changed')
    }
    // 每次执行（包括重试）都重新申请 lease，旧授权不会残留到下一次尝试。
    const lease = await api.createUploadLease({
      dirId: selectedDirId.value,
      path: targetPath.value,
      fileName: item.file.name,
      fileSize: item.file.size,
    })
    await api.uploadByLease(lease, item.file, options)
  },
  shouldStopBatch: (_item, queue) => {
    if (!queue.some((entry) => entry.status === 'queued' || entry.status === 'error')) return false
    if (uploadSubjectChanged.value) return true
    if (!uploadSessionExpired.value) return false
    error.value = '登录状态已过期，当前已授权文件处理完成；队列中未授权文件需要重新登录后继续。'
    return true
  },
  onBatchFinished: ({ queue }) => {
    if (!queue.some((item) => item.status === 'error') && queue.some((item) => item.status === 'success')) notice.value = '上传完成。'
    if (uploadSessionExpired.value && !uploadSubjectChanged.value) {
      window.dispatchEvent(new Event('ft:auth-expired'))
      notice.value = '上传完成。登录状态已过期，请重新登录后查看文件。'
    }
  },
})

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))
const canUpload = computed(() => Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
// 返回浏览页时带回当前目录和上传路径，用户可直接检查刚上传的位置。
const filesRoute = computed(() => ({ name: 'files', query: { dirId: selectedDirId.value, path: targetPath.value } }))
const targetChangeOpen = computed(() => Boolean(pendingTargetDirId.value) || pendingPathUnlock.value)
const pendingTargetName = computed(() => dirs.value.find((dir) => dir.id === pendingTargetDirId.value)?.label || dirs.value.find((dir) => dir.id === pendingTargetDirId.value)?.name || '')
const unfinishedQueueCount = computed(() => uploadQueue.value.filter((item) => item.status !== 'success').length)
const targetChangeMessage = computed(() => unfinishedQueueCount.value
  ? '队列中仍有未完成文件。更改目录或路径会清空当前队列，需要重新选择这些文件。'
  : '更改目录或路径会清空当前队列中的完成记录，但不会删除已经上传的文件。')
const targetChangeDetail = computed(() => {
  const queueSummary = `当前队列：${uploadQueue.value.length} 个文件，共 ${formatBytes(totalBytes.value)}`
  return pendingTargetDirId.value ? `新目录：${pendingTargetName.value} · ${queueSummary}` : queueSummary
})

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
  if (uploadBusy.value) {
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
    addUploadItem(createUploadItem(`local-${Date.now()}-${++uploadCounter}`, file))
  })
}

function applyDirectoryChange(nextDirId: string) {
  if (!clearUploadQueue()) return false
  selectedDirId.value = nextDirId
  targetPath.value = ''
  notice.value = ''
  error.value = ''
  return true
}

function requestDirectoryChange(value: string | number) {
  const nextDirId = String(value)
  if (!nextDirId || nextDirId === selectedDirId.value || uploadBusy.value) return
  if (uploadQueue.value.length) {
    pendingTargetDirId.value = nextDirId
    pendingPathUnlock.value = false
    return
  }
  applyDirectoryChange(nextDirId)
}

function requestPathUnlock() {
  if (uploadBusy.value) return
  if (!uploadQueue.value.length) {
    targetPathInput.value?.focus()
    return
  }
  pendingTargetDirId.value = ''
  pendingPathUnlock.value = true
}

function cancelTargetChange() {
  pendingTargetDirId.value = ''
  pendingPathUnlock.value = false
}

async function confirmTargetChange() {
  const nextDirId = pendingTargetDirId.value
  const unlockPath = pendingPathUnlock.value
  cancelTargetChange()
  if (nextDirId) applyDirectoryChange(nextDirId)
  else if (unlockPath) {
    if (!clearUploadQueue()) return
    notice.value = ''
    await nextTick()
    targetPathInput.value?.focus()
  }
}

function uploadErrorMessage(err: unknown) {
  if (err instanceof ApiError) {
    if (err.code === 'session_subject_changed') {
      markUploadSubjectChanged(false)
      return '登录身份已变化，该文件未获得上传授权。'
    }
    if ((err.details as { aborted?: boolean } | undefined)?.aborted) return '上传已取消。'
    if (err.status === 413) return err.message || '文件超过上传大小限制。'
    if (err.status === 0) return err.message
    return err.message
  }
  return '上传失败，可稍后重试。'
}

function markUploadSubjectChanged(missingIdentity: boolean) {
  uploadSubjectChanged.value = true
  uploadSessionExpired.value = false
  if (missingIdentity) {
    sessionNotice.value = '当前登录身份不可用，上传未开始。请重新登录后重试。'
    error.value = '当前登录身份不可用，未申请任何文件上传授权。'
    return
  }
  sessionNotice.value = '登录身份已变化，上传队列已停止；未获授权的文件不会继续。请在新身份下重新选择或重试。'
  error.value = '登录身份已变化，队列中其余文件未继续上传。'
}

function resetUploadBatchNotices() {
  notice.value = ''
  error.value = ''
  sessionNotice.value = ''
  uploadSessionExpired.value = false
  uploadSubjectChanged.value = false
}

async function uploadAll() {
  if (uploadBusy.value) return
  resetUploadBatchNotices()
  await runUploadAll()
}

async function retryUpload(item: (typeof uploadQueue.value)[number]) {
  if (uploadBusy.value) return
  resetUploadBatchNotices()
  await uploadItem(item)
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.files?.length) addUploadFiles(input.files)
  input.value = ''
}

function chooseFiles() {
  // 拖拽区整块可点击，但实际仍复用隐藏 file input，以保留浏览器原生文件选择能力。
  if (!canUpload.value || uploadBusy.value) return
  fileInput.value?.click()
}

function onDrop(event: DragEvent) {
  event.preventDefault()
  dragOver.value = false
  if (!canUpload.value || uploadBusy.value) return
  if (event.dataTransfer?.files?.length) addUploadFiles(event.dataTransfer.files)
}

function beforeUnload(event: BeforeUnloadEvent) {
  if (!uploading.value && !uploadBatchActive.value) return
  event.preventDefault()
  event.returnValue = '文件正在上传，关闭页面会中断上传。'
}

function handleUploadSessionExpired() {
  if ((!uploading.value && !uploadBatchActive.value) || uploadSubjectChanged.value) return
  uploadSessionExpired.value = true
  sessionNotice.value = '登录状态已过期，已授权的当前上传会继续；上传完成后请重新登录查看文件。'
}

onMounted(loadDirs)
onMounted(() => {
  window.addEventListener('beforeunload', beforeUnload)
  window.addEventListener('ft:upload-session-expired', handleUploadSessionExpired)
})
onUnmounted(() => {
  window.removeEventListener('beforeunload', beforeUnload)
  window.removeEventListener('ft:upload-session-expired', handleUploadSessionExpired)
  disposeUploadQueue()
})
</script>

<template>
  <section class="page-stack">
    <header class="page-header split">
      <h1>文件上传</h1>
      <div class="header-actions">
        <RouterLink class="ghost-btn" :to="filesRoute">查看此目录文件</RouterLink>
        <GlassSelect :model-value="selectedDirId" class="page-select" :options="dirOptions" aria-label="选择上传目录" placeholder="选择目录" @update:model-value="requestDirectoryChange" />
      </div>
    </header>

    <StateBlock :loading="loading" :error="error" :retry-label="!dirs.length ? '重新加载' : ''" @retry="loadDirs" />
    <div v-if="notice" class="alert success" role="status" aria-live="polite">{{ notice }}</div>
    <div v-if="sessionNotice" class="alert info" role="status" aria-live="polite">{{ sessionNotice }}</div>

    <EmptyState v-if="!loading && !error && !dirs.length" title="还没有可用目录" />

    <template v-else-if="selectedDir">
      <div class="panel upload-target-card">
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || selectedDir.root || selectedDir.id }}</p>
        </div>
        <div class="upload-path-field">
          <label for="upload-target-path"><span>上传路径</span></label>
          <input id="upload-target-path" ref="targetPathInput" v-model="targetPath" placeholder="空为目录根路径，或填写 subdir" :disabled="uploadBusy || uploadQueue.length > 0" />
          <small v-if="uploadQueue.length">队列已绑定到当前目标。若需修改路径，请先确认清空队列。</small>
          <button v-if="uploadQueue.length && !uploading" class="mini-btn" type="button" :disabled="uploadBusy" @click="requestPathUnlock">更改上传路径</button>
        </div>
      </div>

      <div class="panel upload-panel">
        <section
          class="dropzone compact"
          :class="{ over: dragOver }"
          aria-label="文件拖放区域"
          @dragover.prevent="dragOver = !uploadBusy"
          @dragleave="dragOver = false"
          @drop="onDrop"
        >
          <div class="dropzone-symbol" aria-hidden="true"><span /></div>
          <strong>拖拽文件到此上传</strong>
          <button class="upload-btn" type="button" :disabled="uploadBusy || !canUpload" @click="chooseFiles">选择文件</button>
          <input ref="fileInput" class="visually-hidden" type="file" multiple :disabled="uploadBusy || !canUpload" @change="onFileChange" />
        </section>

        <div v-if="limits" class="upload-limit-note">
          单文件上限 {{ formatBytes(limits.uploadMaxFileBytes) }}；单次请求上限 {{ formatBytes(limits.uploadMaxBytes) }}。
          请不要关闭页面，关闭或断网会中断上传。
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
              <button v-if="item.status === 'error'" class="mini-btn" type="button" :disabled="uploadBusy" @click="retryUpload(item)">重试</button>
              <button v-if="item.status === 'uploading'" class="mini-btn danger" type="button" @click="cancelUpload(item)">取消上传</button>
              <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" :disabled="uploadBusy" @click="removeUpload(item.id)">移除</button>
            </div>
          </li>
        </ul>

        <div v-if="uploadQueue.length" class="upload-summary">
          <div>
            <strong>{{ uploading ? `整体进度 ${overallProgress}%` : `已完成 ${finishedCount} / ${uploadQueue.length}` }}</strong>
            <small>{{ formatBytes(finishedBytes) }} / {{ formatBytes(totalBytes) }}<template v-if="uploading"> · {{ formatSpeed(currentSpeed) }}</template></small>
          </div>
          <div class="upload-progress wide" role="progressbar" :aria-valuenow="overallProgress" aria-valuemin="0" aria-valuemax="100" aria-label="整体上传进度"><span :style="{ width: `${overallProgress}%` }" /></div>
          <button class="primary-btn" type="button" :disabled="!hasPendingUploads || uploadBusy || !canUpload" @click="uploadAll">
            {{ uploading ? '上传中…' : '开始上传' }}
          </button>
          <button v-if="uploading" class="ghost-btn danger" type="button" @click="cancelCurrentUpload">终止当前上传</button>
        </div>
      </div>
    </template>

    <ConfirmDialog
      :open="targetChangeOpen"
      title="清空队列并更改上传目标？"
      :message="targetChangeMessage"
      :detail="targetChangeDetail"
      confirm-label="清空并更改"
      :danger="true"
      @cancel="cancelTargetChange"
      @confirm="confirmTargetChange"
    />
  </section>
</template>
