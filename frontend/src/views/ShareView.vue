<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api, publicDownloadUrl } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { TokenInfo } from '@/types'
import { formatBytes, formatDate } from '@/utils'

interface UploadItem {
  id: string
  file: File
  status: 'queued' | 'uploading' | 'success' | 'error'
  error?: string
}

const route = useRoute()
const tokenParam = computed(() => String(route.params.token || ''))

const info = ref<TokenInfo | null>(null)
const loading = ref(true)
const error = ref('')

const tokenType = computed(() => info.value?.type || info.value?.kind || 'download')
const isUpload = computed(() => tokenType.value === 'upload')
const isDownload = computed(() => tokenType.value === 'download')

const validFlag = computed(() => {
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
    ? '把文件拖到下方，或选择文件，即可上传到对方指定的位置。'
    : '点击下方按钮即可开始下载，链接将根据规则自动到期或失效。'
})

const downloadHref = computed(() => publicDownloadUrl(tokenParam.value))
const usesLabel = computed(() => {
  if (!info.value) return ''
  const used = info.value.uses ?? info.value.used ?? 0
  const max = info.value.maxUses
  if (!max || max <= 0) return `已使用 ${used} 次 · 不限次数`
  return `已使用 ${used} / ${max} 次`
})

async function loadInfo() {
  loading.value = true
  error.value = ''
  try {
    info.value = await api.publicTokenInfo(tokenParam.value)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '链接信息获取失败。'
    info.value = null
  } finally {
    loading.value = false
  }
}

// ---------- Upload queue ----------
const queue = ref<UploadItem[]>([])
const dragOver = ref(false)
const fileInput = ref<HTMLInputElement | null>(null)
let counter = 0
const nextId = () => `up-${Date.now()}-${++counter}`
const uploading = computed(() => queue.value.some((item) => item.status === 'uploading'))
const hasPendingUploads = computed(() => queue.value.some((item) => item.status === 'queued' || item.status === 'error'))

function pickFiles() {
  if (uploading.value) return
  fileInput.value?.click()
}

function addFiles(files: FileList | File[]) {
  if (!validFlag.value || uploading.value) return
  const list = Array.from(files || [])
  for (const file of list) {
    queue.value.push({ id: nextId(), file, status: 'queued' })
  }
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
  if (uploading.value) return
  dragOver.value = true
}

function removeItem(id: string) {
  queue.value = queue.value.filter((item) => item.id !== id)
}

async function uploadItem(item: UploadItem) {
  if (item.status === 'uploading' || item.status === 'success') return
  item.status = 'uploading'
  item.error = undefined
  try {
    await api.publicUpload(tokenParam.value, item.file)
    item.status = 'success'
  } catch (err) {
    item.status = 'error'
    item.error = err instanceof ApiError ? err.message : '上传失败，请稍后重试。'
  }
}

async function uploadAll() {
  if (uploading.value) return
  const pending = queue.value.filter((item) => item.status === 'queued' || item.status === 'error')
  for (const item of pending) {
    await uploadItem(item)
    if (item.status === 'success') {
      // refresh info to update use counter
      try { info.value = await api.publicTokenInfo(tokenParam.value) } catch { /* ignore */ }
    }
  }
}

function retryItem(item: UploadItem) {
  uploadItem(item)
}

onMounted(loadInfo)
</script>

<template>
  <main class="share-page">
    <div class="share-shell">
      <header class="share-header">
        <RouterLink to="/" class="brand">
          <span class="brand-mark">FT</span>
          <span><strong>文件传输台</strong><small>临时分享</small></span>
        </RouterLink>
      </header>

      <StateBlock :loading="loading" :error="error" />

      <article v-if="info" class="share-card" :data-kind="tokenType">
        <div class="share-card-glow" aria-hidden="true" />
        <p class="eyebrow">{{ isUpload ? 'Inbox link' : 'Download link' }}</p>
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
            <a class="primary-btn big" :href="downloadHref">⇩ 立即下载</a>
            <small>下载将按链接设置消耗一次使用机会。</small>
          </div>
          <div v-else class="alert error">{{ reasonLabel }}</div>
        </template>

        <!-- Upload mode -->
        <template v-else-if="isUpload">
          <template v-if="validFlag">
            <div
              class="dropzone"
              :class="{ over: dragOver }"
              @click="pickFiles"
              @dragover="onDragOver"
              @dragleave="dragOver = false"
              @drop="onDrop"
            >
              <div class="dropzone-symbol" aria-hidden="true"><span /></div>
              <strong>把文件拖到这里</strong>
              <small>或点击此处选择文件，支持多文件</small>
              <input
                ref="fileInput"
                type="file"
                multiple
                hidden
                @change="onFileChange"
              />
            </div>

            <ul v-if="queue.length" class="upload-queue">
              <li v-for="item in queue" :key="item.id" :data-status="item.status">
                <div class="q-file">
                  <strong>{{ item.file.name }}</strong>
                  <small>{{ formatBytes(item.file.size) }} · <span class="q-status">{{
                    item.status === 'queued' ? '待上传'
                    : item.status === 'uploading' ? '上传中…'
                    : item.status === 'success' ? '已完成'
                    : item.error || '失败'
                  }}</span></small>
                </div>
                <div class="q-actions">
                  <button v-if="item.status === 'error'" class="mini-btn" type="button" :disabled="uploading" @click="retryItem(item)">重试</button>
                  <button v-if="item.status !== 'uploading'" class="mini-btn danger" type="button" @click="removeItem(item.id)">移除</button>
                </div>
              </li>
            </ul>

            <div class="share-actions">
              <button class="primary-btn big" type="button" :disabled="!hasPendingUploads || uploading" @click="uploadAll">
                {{ uploading ? '上传中…' : '开始上传' }}
              </button>
              <button class="ghost-btn" type="button" :disabled="uploading" @click="pickFiles">追加文件</button>
            </div>
          </template>
          <div v-else class="alert error">{{ reasonLabel }}</div>
        </template>

        <template v-else>
          <EmptyState title="未知链接类型" description="无法判断该分享的用途，请确认链接是否完整。" />
        </template>
      </article>

      <footer class="share-foot">
        <small>由文件传输台为你安全分发 · 所有访问都会被记录</small>
      </footer>
    </div>
  </main>
</template>
