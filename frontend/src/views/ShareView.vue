<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import { createUploadItem, reconcileConservativeUploadedBytes, useUploadQueue } from '@/composables/useUploadQueue'
import type { TokenInfo } from '@/types'
import { acquireUploadSessionHold } from '@/useSessionActivity'
import { buildTransferUrl, formatBytes, formatDate, formatDuration, formatSpeed } from '@/utils'

const route = useRoute()
const tokenParam = computed(() => String(route.params.token || ''))

const info = ref<TokenInfo | null>(null)
const loading = ref(true)
const error = ref('')
const downloading = ref(false)
const batchNotice = ref('')

const tokenType = computed(() => info.value?.type || info.value?.kind || 'download')
const isUpload = computed(() => tokenType.value === 'upload')
const isDownload = computed(() => tokenType.value === 'download')

const validFlag = computed(() => {
  // 公开信息接口会返回 valid；这里保留本地兜底判断，兼容旧后端或缓存响应。
  if (!info.value) return false
  if (typeof info.value.valid === 'boolean') return info.value.valid
  if (info.value.revoked) return false
  if (info.value.expiresAt && new Date(info.value.expiresAt).getTime() < Date.now()) return false
  const maxUses = info.value.maxUses
  const uses = info.value.uses ?? info.value.used ?? 0
  if (typeof maxUses === 'number' && maxUses > 0 && uses >= maxUses) return false
  return true
})

const reasonLabel = computed(() => {
  if (!info.value) return ''
  if (validFlag.value) return ''
  const reason = info.value.reason
  if (reason === 'revoked' || info.value.revoked) return '该分享链接已被撤销。'
  if (reason === 'expired') return '该分享链接已超过有效期。'
  if (reason === 'exhausted') return '该分享链接已达到使用次数上限。'
  if (reason === 'upload_quota_exhausted') return '该上传链接已达到累计上传容量上限。'
  if (reason === 'resource_unavailable') return '该分享对应的资源已被移除。'
  if (reason === 'permission_disabled') return '该分享对应的权限已关闭。'
  return '该分享链接当前不可用。'
})

const headline = computed(() => {
  if (!info.value) return '正在准备分享…'
  if (!validFlag.value) return '分享已失效'
  return isUpload.value ? '收件箱已就绪' : '文件已为你准备好'
})

const subline = computed(() => {
  if (!info.value || !validFlag.value) return reasonLabel.value
  return isUpload.value
    ? '把文件拖到下方，或选择文件即可上传。'
    : '点击下方按钮即可开始下载。'
})

const usesLabel = computed(() => {
  if (!info.value) return ''
  const used = info.value.uses ?? info.value.used ?? 0
  const max = info.value.maxUses
  if (!max || max <= 0) return `已使用 ${used} 次 · 不限次数`
  return `已使用 ${used} / ${max} 次`
})
const remainingUploadBytes = computed(() => {
  const max = info.value?.uploadMaxBytes || 0
  if (!max) return 0
  return Math.max(0, max - (info.value?.uploadedBytes || 0) - uploadedBytesSinceInfo.value)
})
const extensionPolicyLabel = computed(() => {
  const allowed = info.value?.allowedExtensions || []
  const blocked = info.value?.blockedExtensions || []
  if (allowed.length) return `允许：${allowed.join('、')}`
  if (blocked.length) return `不允许：${blocked.join('、')}`
  return '文件类型不限'
})

async function loadInfo() {
  loading.value = true
  error.value = ''
  try {
    info.value = await api.publicTokenInfo(tokenParam.value)
    uploadedBytesSinceInfo.value = 0
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '链接信息获取失败。'
    info.value = null
  } finally {
    loading.value = false
  }
}

async function startPublicDownload() {
  if (!validFlag.value || downloading.value) return
  downloading.value = true
  error.value = ''
  try {
    const lease = await api.createPublicDownloadLease(tokenParam.value)
    // 公开下载先兑换票据：只消耗一次使用次数，后续 Range 续传不会重复扣次。
    window.location.assign(buildTransferUrl(lease.url))
    try { info.value = await api.publicTokenInfo(tokenParam.value) } catch { /* ignore */ }
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '下载链接创建失败，请稍后重试。'
  } finally {
    downloading.value = false
  }
}

// 上传队列只存在于当前页面内，刷新或关闭页面不会保留待上传文件，避免持久化本地文件引用。
const dragOver = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
let counter = 0
const uploadedBytesSinceInfo = ref(0)
const nextId = () => `up-${Date.now()}-${++counter}`

const {
  queue,
  uploading,
  busy: uploadBusy,
  hasPendingUploads,
  totalBytes,
  finishedBytes,
  finishedCount,
  overallProgress,
  currentSpeed,
  add: addUploadItem,
  remove: removeItem,
  uploadAll: runUploadAll,
  retry: retryItem,
  cancel: cancelUpload,
  cancelCurrent: cancelCurrentUpload,
  dispose: disposeUploadQueue,
} = useUploadQueue({
  acquireHold: acquireUploadSessionHold,
  errorMessage: uploadErrorMessage,
  uploadFile: async (item, options) => {
    await api.publicUpload(tokenParam.value, item.file, options)
  },
  onItemSuccess: async (item) => {
    const previousServerBytes = info.value?.uploadedBytes || 0
    uploadedBytesSinceInfo.value += item.file.size
    const refreshed = await api.publicTokenInfo(tokenParam.value)
    uploadedBytesSinceInfo.value = reconcileConservativeUploadedBytes(
      previousServerBytes,
      uploadedBytesSinceInfo.value,
      refreshed.uploadedBytes || 0,
    )
    info.value = refreshed
  },
  onBatchFinished: ({ stopped, succeeded, failed, queue }) => {
    if (!stopped && succeeded > 0 && failed === 0 && !queue.some((item) => item.status === 'queued')) {
      batchNotice.value = `上传完成，共 ${succeeded} 个文件。你可以继续追加文件，或直接关闭此页面。`
    } else if (succeeded > 0 && failed > 0) {
      batchNotice.value = `已上传 ${succeeded} 个文件，另有 ${failed} 个文件需要重试。`
    }
  },
})

function pickFiles() {
  if (uploadBusy.value) return
  fileInput.value?.click()
}

function addFiles(files: FileList | File[]) {
  if (!validFlag.value || uploadBusy.value) return
  const list = Array.from(files || [])
  const maxFileBytes = info.value?.uploadMaxFileBytes || 0
  const maxRequestBytes = info.value?.uploadRequestMaxBytes || 0
  const allowed = info.value?.allowedExtensions || []
  const blocked = info.value?.blockedExtensions || []
  for (const file of list) {
    const ext = fileExtension(file.name)
    if (blocked.includes(ext)) {
      error.value = `“${file.name}” 的扩展名不允许上传。`
      return
    }
    if (allowed.length && !allowed.includes(ext)) {
      error.value = `“${file.name}” 不在允许上传的扩展名范围内。`
      return
    }
    if (maxFileBytes > 0 && file.size > maxFileBytes) {
      error.value = `“${file.name}” 大小为 ${formatBytes(file.size)}，超过单文件上限 ${formatBytes(maxFileBytes)}。`
      return
    }
    if (maxRequestBytes > 0 && file.size > maxRequestBytes) {
      error.value = `“${file.name}” 超过单次上传上限 ${formatBytes(maxRequestBytes)}。`
      return
    }
  }
  const maxBytes = info.value?.uploadMaxBytes || 0
  const usedBytes = (info.value?.uploadedBytes || 0) + uploadedBytesSinceInfo.value
  if (maxBytes > 0) {
    let projected = usedBytes + queue.value.reduce((sum, item) => sum + (item.status === 'queued' || item.status === 'error' ? item.file.size : 0), 0)
    for (const file of list) {
      projected += file.size
      if (projected > maxBytes) {
        error.value = `“${file.name}” 会超过该上传链接的剩余容量。`
        return
      }
    }
  }
  error.value = ''
  batchNotice.value = ''
  for (const file of list) {
    // 使用本地唯一 ID 跟踪状态，避免同名文件在队列中互相覆盖。
    addUploadItem(createUploadItem(nextId(), file))
  }
}

function fileExtension(name: string) {
  const index = name.lastIndexOf('.')
  return index > -1 ? name.slice(index).toLowerCase() : ''
}

function uploadErrorMessage(err: unknown) {
  if (err instanceof ApiError) {
    if ((err.details as { aborted?: boolean } | undefined)?.aborted) return '上传已取消。'
    if (err.status === 413) return err.message || '文件超过上传大小限制。'
    if (err.status === 0) return err.message
    return err.message
  }
  return '上传失败，请稍后重试。'
}

function onFileChange(event: Event) {
  const input = event.target as HTMLInputElement
  if (input.files?.length) addFiles(input.files)
  input.value = ''
}

function onDrop(event: DragEvent) {
  event.preventDefault()
  dragOver.value = false
  if (event.dataTransfer?.files?.length) addFiles(event.dataTransfer.files)
}

function onDragOver(event: DragEvent) {
  event.preventDefault()
  if (uploadBusy.value) return
  dragOver.value = true
}

function beforeUnload(event: BeforeUnloadEvent) {
  if (!uploading.value) return
  event.preventDefault()
  event.returnValue = '文件正在上传，关闭页面会中断上传。'
}

async function uploadAll() {
  if (uploadBusy.value) return
  batchNotice.value = ''
  await runUploadAll()
}

onMounted(() => {
  loadInfo()
  window.addEventListener('beforeunload', beforeUnload)
})
onUnmounted(() => {
  window.removeEventListener('beforeunload', beforeUnload)
  disposeUploadQueue()
})
</script>

<template>
  <main id="main-content" class="share-page" tabindex="-1" data-route-focus>
    <div class="share-shell">
      <header class="share-header">
        <div class="brand" aria-label="文件传输台临时分享">
          <span class="brand-mark">FT</span>
          <span><strong>文件传输台</strong><small>临时分享</small></span>
        </div>
      </header>

      <StateBlock :loading="loading" :error="error" :retry-label="!info ? '重新加载' : ''" @retry="loadInfo" />

      <article v-if="info" class="share-card" :data-kind="tokenType">
        <div class="share-card-glow" aria-hidden="true" />
        <p class="eyebrow">{{ isUpload ? '临时收件' : '临时下载' }}</p>
        <h1>{{ headline }}</h1>
        <p class="share-sub">{{ subline }}</p>

        <dl class="share-meta">
          <div>
            <dt>目录</dt>
            <dd>{{ info.dirName || info.dir || info.dirId || '—' }}</dd>
          </div>
          <div>
            <dt>路径</dt>
            <dd><code>{{ info.path || '/' }}</code></dd>
          </div>
          <div>
            <dt>到期时间</dt>
            <dd>{{ formatDate(info.expiresAt) }}</dd>
          </div>
          <div>
            <dt>使用情况</dt>
            <dd>{{ usesLabel }}</dd>
          </div>
        </dl>

        <!-- Download mode -->
        <template v-if="isDownload">
          <div v-if="validFlag" class="share-actions">
            <button class="primary-btn big" type="button" :disabled="downloading" @click="startPublicDownload">
              <AppIcon name="download" :size="20" />
              {{ downloading ? '准备下载…' : '立即下载' }}
            </button>
          </div>
          <div v-else class="alert error" role="alert">{{ reasonLabel }}</div>
        </template>

        <!-- Upload mode -->
        <template v-else-if="isUpload">
          <template v-if="validFlag">
            <section
              class="dropzone"
              :class="{ over: dragOver }"
              aria-label="文件拖放区域"
              @dragover="onDragOver"
              @dragleave="dragOver = false"
              @drop="onDrop"
            >
              <div class="dropzone-symbol" aria-hidden="true"><span /></div>
              <strong>把文件拖到这里</strong>
              <small>或点击此处选择文件，支持多文件</small>
              <button class="upload-btn" type="button" :disabled="uploadBusy" @click="pickFiles">选择文件</button>
              <input ref="fileInput" class="visually-hidden" type="file" multiple :disabled="uploadBusy" @change="onFileChange" />
            </section>

            <div class="upload-constraints" aria-label="上传限制">
              <div v-if="info.uploadMaxFileBytes"><span>单文件上限</span><strong>{{ formatBytes(info.uploadMaxFileBytes) }}</strong></div>
              <div v-if="info.uploadRequestMaxBytes"><span>单次上限</span><strong>{{ formatBytes(info.uploadRequestMaxBytes) }}</strong></div>
              <div v-if="info.uploadMaxBytes"><span>剩余容量</span><strong>{{ formatBytes(remainingUploadBytes) }}</strong></div>
              <div><span>文件类型</span><strong>{{ extensionPolicyLabel }}</strong></div>
            </div>

            <div v-if="batchNotice" class="alert success" role="status" aria-live="polite">{{ batchNotice }}</div>

            <ul v-if="queue.length" class="upload-queue">
              <li v-for="item in queue" :key="item.id" :data-status="item.status">
                <div class="q-file">
                  <strong>{{ item.file.name }}</strong>
                  <small>{{ formatBytes(item.file.size) }} · <span class="q-status">{{
                    item.status === 'queued' ? '待上传'
                    : item.status === 'uploading' ? item.progress >= 100 ? '处理中…' : `上传中 ${item.progress}%`
                    : item.status === 'success' ? '已完成'
                    : item.error || '失败'
                  }}</span></small>
                  <div class="upload-progress" role="progressbar" :aria-valuenow="item.status === 'success' ? 100 : item.progress" aria-valuemin="0" aria-valuemax="100" :aria-label="`${item.file.name} 上传进度 ${item.status === 'success' ? 100 : item.progress}%`">
                    <span :style="{ width: `${item.status === 'success' ? 100 : item.progress}%` }" />
                  </div>
                  <small v-if="item.status === 'uploading'" class="upload-progress-text">
                    {{ formatBytes(item.loaded) }} / {{ formatBytes(item.total || item.file.size) }} · {{ formatSpeed(item.speedBps) }} · 剩余 {{ formatDuration(item.etaSeconds) }}
                  </small>
                </div>
                <div class="q-actions">
                  <button v-if="item.status === 'error'" class="mini-btn" type="button" :disabled="uploadBusy" @click="retryItem(item)">重试</button>
                  <button v-if="item.status === 'uploading'" class="mini-btn danger" type="button" @click="cancelUpload(item)">取消上传</button>
                  <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" :disabled="uploadBusy" @click="removeItem(item.id)">移除</button>
                </div>
              </li>
            </ul>

            <div class="share-actions">
              <div v-if="queue.length" class="upload-summary share-upload-summary">
                <div>
                  <strong>{{ uploading ? `整体进度 ${overallProgress}%` : `已完成 ${finishedCount} / ${queue.length}` }}</strong>
                  <small>{{ formatBytes(finishedBytes) }} / {{ formatBytes(totalBytes) }}<template v-if="uploading"> · {{ formatSpeed(currentSpeed) }}</template></small>
                </div>
                <div class="upload-progress wide" role="progressbar" :aria-valuenow="overallProgress" aria-valuemin="0" aria-valuemax="100" aria-label="整体上传进度"><span :style="{ width: `${overallProgress}%` }" /></div>
              </div>
              <button class="primary-btn big" type="button" :disabled="!hasPendingUploads || uploadBusy" @click="uploadAll">
                {{ uploading ? '上传中…' : '开始上传' }}
              </button>
              <button v-if="uploading" class="ghost-btn danger" type="button" @click="cancelCurrentUpload">终止当前上传</button>
              <button class="ghost-btn" type="button" :disabled="uploadBusy" @click="pickFiles">追加文件</button>
            </div>
          </template>
          <div v-else class="alert error" role="alert">{{ reasonLabel }}</div>
        </template>

        <template v-else>
          <EmptyState title="未知链接类型" description="无法判断该分享的用途，请确认链接是否完整。" />
        </template>
      </article>

      <footer class="share-foot">
        <small>由文件传输台提供临时分享</small>
      </footer>
    </div>
  </main>
</template>
