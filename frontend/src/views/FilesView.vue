<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
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
const router = useRouter()
const selectedDirId = ref('')
const currentPath = ref('')
const entries = ref<FileEntry[]>([])
const loading = ref(true)
const loadingMore = ref(false)
const listError = ref('')
const appendError = ref('')
const actionError = ref('')
const downloadingPath = ref('')
const responseCanUpload = ref<boolean | null>(null)
const responseCanDownload = ref<boolean | null>(null)
// 从上传页返回时会携带 dirId/path；恢复阶段暂停 watch，避免重复请求和路径被重置。
let restoringQuery = false
let dirsRequestId = 0
let filesRequestId = 0
let focusSummaryAfterLoad = false
const defaultPageSize = 100
const effectivePageSize = ref(defaultPageSize)
const page = ref(1)
const hasMore = ref(false)
const truncated = ref(false)
const totalKnown = ref(false)
const total = ref<number | null>(null)
const scannedEntries = ref(0)
const scanLimit = ref(0)
const failedAppendRequest = ref<{ page: number; pageSize: number } | null>(null)
const liveMessage = ref('')
const listStateRef = ref<HTMLElement | null>(null)
const listSummaryRef = ref<HTMLElement | null>(null)
const resourceBrowserRef = ref<HTMLElement | null>(null)
const appendErrorRef = ref<HTMLElement | null>(null)
const actionErrorRef = ref<HTMLElement | null>(null)
const loadMoreButtonRef = ref<HTMLButtonElement | null>(null)

const selectedDir = computed(() => dirs.value.find((dir) => dir.id === selectedDirId.value))
const canUpload = computed(() => responseCanUpload.value ?? Boolean(selectedDir.value?.canUpload ?? selectedDir.value?.allowUpload))
const canDownload = computed(() => responseCanDownload.value ?? (selectedDir.value ? selectedDir.value.canDownload !== false && selectedDir.value.allowDownload !== false : false))
const selectedIsFileResource = computed(() => selectedDir.value?.type === 'file')
const crumbs = computed(() => [{ name: selectedDir.value?.name || '目录', path: '' }, ...currentPath.value.split('/').filter(Boolean).map((part, index, arr) => ({ name: part, path: arr.slice(0, index + 1).join('/') }))])
const motionKey = computed(() => (!loading.value && dirs.value.length ? `${dirs.value.length}:${selectedDirId.value || 'none'}` : ''))
const listingSummary = computed(() => {
  if (totalKnown.value && total.value !== null) return `共 ${total.value} 项 · 已显示 ${entries.value.length} 项`
  return `总数未知 · 已显示 ${entries.value.length} 项`
})
const truncationMessage = computed(() => {
  const scanned = scannedEntries.value || scanLimit.value
  if (scanned > 0) return `服务端最多扫描了前 ${scanned} 个目录项；当前列表只包含其中可显示的结果，未扫描部分不计入。`
  return '目录内容超过服务端扫描窗口；当前列表只包含已扫描范围内的结果，未扫描部分不计入。'
})
const showLoadMoreFooter = computed(() => Boolean(entries.value.length && (hasMore.value || loadingMore.value || appendError.value || page.value > 1)))

useGsapEntrance(pageRef, { refreshKey: () => motionKey.value })

function tokenQuery(type: 'download' | 'upload', entry?: FileEntry) {
  const path = entry ? (entry.path || joinPath(currentPath.value, entry.name)) : currentPath.value
  return { type, dirId: selectedDirId.value, path }
}

function hasEntryAction(entry: FileEntry) {
  if (entryCanDownload(entry)) return true
  if (isAdmin.value && entryIsSafeDir(entry) && canUpload.value) return true
  return false
}

function entryIsDir(entry: FileEntry) {
  return entry.isDir || entry.type === 'dir' || entry.type === 'directory'
}

function entryIsSafeDir(entry: FileEntry) {
  const directoryType = entry.type === 'dir' || entry.type === 'directory' || (!entry.type && entry.isDir === true)
  return directoryType && entry.metadataKnown !== false && entry.symlink !== true && entry.selectable !== false
}

function entryIsKnownFile(entry: FileEntry) {
  if (entry.type === 'file' || entry.type === 'symlink') return true
  if (entry.type === 'unknown' || entry.type === 'other') return false
  return !entry.type && !entryIsDir(entry)
}

function entryCanDownload(entry: FileEntry) {
  return canDownload.value && entryIsKnownFile(entry) && entry.metadataKnown !== false && entry.downloadable !== false && entry.selectable !== false
}

function entryVisualKind(entry: FileEntry) {
  if (entry.metadataKnown === false || entry.type === 'unknown' || entry.type === 'other') return 'unknown'
  return entryIsDir(entry) ? 'folder' : 'file'
}

function entryUnavailableReason(entry: FileEntry) {
  if (entry.metadataKnown === false) return '元数据不可用'
  if (entry.type === 'unknown' || entry.type === 'other') return '类型无法识别'
  if (entryIsDir(entry)) {
    if (entry.symlink) return '符号链接不可进入'
    if (entry.selectable === false) return '当前目录不可进入'
    if (entryIsSafeDir(entry)) return '可从名称进入'
    return '目录不可用'
  }
  if (!canDownload.value) return '当前资源禁止下载'
  if (entry.downloadable === false || entry.selectable === false) return '当前文件不可下载'
  return '没有可用操作'
}

function resetPagination() {
  page.value = 1
  hasMore.value = false
  truncated.value = false
  totalKnown.value = false
  total.value = null
  scannedEntries.value = 0
  scanLimit.value = 0
  effectivePageSize.value = defaultPageSize
  failedAppendRequest.value = null
  loadingMore.value = false
}

function appendUniqueByPath(current: FileEntry[], incoming: FileEntry[]) {
  const seen = new Set(current.map((entry) => entry.path).filter((path): path is string => Boolean(path)))
  const merged = [...current]
  incoming.forEach((entry) => {
    if (entry.path && seen.has(entry.path)) return
    if (entry.path) seen.add(entry.path)
    merged.push(entry)
  })
  return merged
}

async function loadDirs() {
  const requestId = ++dirsRequestId
  const shouldRestoreFocus = Boolean(listError.value)
  loading.value = true
  listError.value = ''
  appendError.value = ''
  actionError.value = ''
  try {
    const result = await api.dirs()
    if (requestId !== dirsRequestId) return
    dirs.value = result
    const queryDir = String(route.query.dirId || '')
    restoringQuery = true
    selectedDirId.value = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value.length === 1 ? dirs.value[0]?.id || '' : ''
    currentPath.value = selectedDirId.value ? String(route.query.path || '') : ''
    if (selectedDirId.value) await loadFiles()
    else if (shouldRestoreFocus) {
      await nextTick()
      resourceBrowserRef.value?.focus()
    }
    restoringQuery = false
  } catch (err) {
    if (requestId !== dirsRequestId) return
    listError.value = err instanceof ApiError ? err.message : '目录加载失败。'
    await nextTick()
    listStateRef.value?.focus()
  } finally {
    if (requestId === dirsRequestId) loading.value = false
  }
}

async function loadFiles(nextPage = 1, append = false, pageSizeOverride?: number) {
  if (!selectedDirId.value) return
  if (append && (!hasMore.value || loading.value || loadingMore.value)) return
  const requestId = ++filesRequestId
  const targetDirId = selectedDirId.value
  const targetPath = currentPath.value
  const requestPageSize = pageSizeOverride ?? (append ? effectivePageSize.value : defaultPageSize)
  const previousCount = entries.value.length
  const shouldFocusSummary = !append && focusSummaryAfterLoad
  const activeElement = document.activeElement
  const appendTriggeredFromControls = append && (
    activeElement === loadMoreButtonRef.value
    || Boolean(appendErrorRef.value && activeElement instanceof Node && appendErrorRef.value.contains(activeElement))
  )
  if (!append) focusSummaryAfterLoad = false
  if (append) loadingMore.value = true
  else {
    loading.value = true
    entries.value = []
    resetPagination()
    listError.value = ''
    actionError.value = ''
  }
  appendError.value = ''
  let focusTarget: 'summary' | 'load-more' | 'list-error' | 'append-error' | null = null
  try {
    const result = await api.listFiles(targetDirId, targetPath, nextPage, requestPageSize)
    if (requestId !== filesRequestId || targetDirId !== selectedDirId.value || targetPath !== currentPath.value) return
    const nextEntries = result.entries || result.files || []
    entries.value = append ? appendUniqueByPath(entries.value, nextEntries) : nextEntries
    page.value = result.page ?? nextPage
    effectivePageSize.value = typeof result.pageSize === 'number' && result.pageSize > 0 ? result.pageSize : requestPageSize
    hasMore.value = result.hasMore === true
    truncated.value = result.truncated === true
    totalKnown.value = result.totalKnown === true
    total.value = typeof result.total === 'number' ? result.total : null
    scannedEntries.value = result.scannedEntries ?? entries.value.length
    scanLimit.value = result.scanLimit ?? 0
    failedAppendRequest.value = null
    responseCanUpload.value = typeof result.canUpload === 'boolean' ? result.canUpload : null
    responseCanDownload.value = typeof result.canDownload === 'boolean' ? result.canDownload : null
    if (append) {
      const addedCount = Math.max(0, entries.value.length - previousCount)
      liveMessage.value = `已新增 ${addedCount} 项，当前共显示 ${entries.value.length} 项。${hasMore.value ? '' : '已全部加载。'}`
      if (appendTriggeredFromControls) focusTarget = hasMore.value ? 'load-more' : 'summary'
    } else if (shouldFocusSummary) {
      focusTarget = 'summary'
    }
  } catch (err) {
    if (requestId !== filesRequestId) return
    if (append) {
      failedAppendRequest.value = { page: nextPage, pageSize: requestPageSize }
      appendError.value = err instanceof ApiError ? err.message : '后续文件加载失败。'
      focusTarget = 'append-error'
    } else {
      entries.value = []
      responseCanUpload.value = null
      responseCanDownload.value = null
      listError.value = err instanceof ApiError ? err.message : '文件列表加载失败。'
      focusTarget = 'list-error'
    }
  } finally {
    if (requestId === filesRequestId) {
      if (append) loadingMore.value = false
      else loading.value = false
      await nextTick()
      if (focusTarget === 'summary') listSummaryRef.value?.focus()
      else if (focusTarget === 'load-more') loadMoreButtonRef.value?.focus()
      else if (focusTarget === 'list-error') listStateRef.value?.focus()
      else if (focusTarget === 'append-error') appendErrorRef.value?.focus()
    }
  }
}

function openDir(entry: FileEntry) {
  if (loading.value || !entryIsSafeDir(entry)) return
  focusSummaryAfterLoad = true
  listSummaryRef.value?.focus({ preventScroll: true })
  loading.value = true
  entries.value = []
  currentPath.value = joinPath(currentPath.value, entry.name)
  syncFileLocation()
}

function navigateToPath(path: string) {
  if (loading.value || path === currentPath.value) return
  focusSummaryAfterLoad = true
  loading.value = true
  entries.value = []
  currentPath.value = path
  syncFileLocation()
}

function selectResource(dir: DirectoryInfo) {
  if (loading.value) return
  if (selectedDirId.value === dir.id && !currentPath.value) return
  focusSummaryAfterLoad = true
  loading.value = true
  entries.value = []
  if (selectedDirId.value === dir.id) {
    currentPath.value = ''
    syncFileLocation()
    return
  }
  selectedDirId.value = dir.id
  syncFileLocation()
}

function syncFileLocation() {
  if (restoringQuery || route.name !== 'files') return
  const query = selectedDirId.value
    ? { dirId: selectedDirId.value, ...(currentPath.value ? { path: currentPath.value } : {}) }
    : {}
  void router.push({ name: 'files', query })
}

function retryListLoad() {
  focusSummaryAfterLoad = true
  if (selectedDirId.value && dirs.value.length) void loadFiles(1, false)
  else void loadDirs()
}

function retryAppend() {
  const failed = failedAppendRequest.value
  if (!failed) return
  void loadFiles(failed.page, true, failed.pageSize)
}

async function dismissActionError() {
  actionError.value = ''
  await nextTick()
  listSummaryRef.value?.focus()
}

function resourceDescription(dir: DirectoryInfo) {
  return dir.description || dir.root || dir.id
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
  if (loading.value || !selectedDirId.value || !entryCanDownload(entry) || downloadingPath.value) return
  const path = entry.path || joinPath(currentPath.value, entry.name)
  downloadingPath.value = path
  actionError.value = ''
  try {
    const lease = await api.createDownloadLease(selectedDirId.value, path)
    // 下载使用独立票据地址，页面会话随后空闲过期也不会中断已授权文件传输。
    window.location.assign(buildTransferUrl(lease.url))
  } catch (err) {
    actionError.value = err instanceof ApiError ? err.message : '下载链接创建失败，请稍后重试。'
    await nextTick()
    actionErrorRef.value?.focus()
  } finally {
    downloadingPath.value = ''
  }
}

watch(selectedDirId, async () => {
  if (restoringQuery) return
  // 用户主动切换目录时回到根路径；从 query 恢复时不走这里，保留原路径上下文。
  restoringQuery = true
  currentPath.value = ''
  restoringQuery = false
  responseCanUpload.value = null
  responseCanDownload.value = null
  entries.value = []
  if (selectedDirId.value) await loadFiles()
}, { flush: 'sync' })
watch(currentPath, () => {
  if (restoringQuery) return
  loadFiles()
}, { flush: 'sync' })
watch(() => `${String(route.query.dirId || '')}\n${String(route.query.path || '')}`, () => {
  if (route.name !== 'files' || !dirs.value.length) return
  const queryDir = String(route.query.dirId || '')
  const nextDir = dirs.value.some((dir) => dir.id === queryDir) ? queryDir : dirs.value.length === 1 ? dirs.value[0].id : ''
  const nextPath = nextDir ? String(route.query.path || '') : ''
  if (nextDir === selectedDirId.value && nextPath === currentPath.value) return
  filesRequestId += 1
  focusSummaryAfterLoad = true
  restoringQuery = true
  selectedDirId.value = nextDir
  currentPath.value = nextPath
  responseCanUpload.value = null
  responseCanDownload.value = null
  entries.value = []
  restoringQuery = false
  if (nextDir) void loadFiles()
  else {
    resetPagination()
    loading.value = false
  }
})
onMounted(loadDirs)
onBeforeUnmount(() => {
  dirsRequestId += 1
  filesRequestId += 1
  loading.value = false
  loadingMore.value = false
})
</script>

<template>
  <section ref="pageRef" class="page-stack files-page">
    <header class="page-header split">
      <h1>文件浏览</h1>
    </header>

    <div v-if="loading || listError" ref="listStateRef" class="list-state-anchor" :tabindex="listError ? -1 : undefined">
      <StateBlock :loading="loading" :error="listError" retry-label="重新加载列表" @retry="retryListLoad" />
    </div>
    <p class="visually-hidden" aria-live="polite">{{ liveMessage }}</p>

    <EmptyState v-if="!loading && !listError && !dirs.length" title="还没有可用目录" />

    <div v-if="dirs.length" ref="resourceBrowserRef" class="resource-browser" :aria-busy="loading" tabindex="-1" data-motion>
      <button
        v-for="dir in dirs"
        :key="dir.id"
        class="resource-tile"
        type="button"
        :disabled="loading"
        :aria-pressed="dir.id === selectedDirId"
        :data-active="dir.id === selectedDirId"
        :data-type="dir.type === 'file' ? 'file' : 'directory'"
        @click="selectResource(dir)"
      >
        <span class="resource-icon" aria-hidden="true"><span /></span>
        <span class="resource-copy">
          <strong>{{ dir.label || dir.name }}</strong>
          <small v-if="resourceDescription(dir)">{{ resourceDescription(dir) }}</small>
          <small>{{ resourcePermissionLabel(dir) }}</small>
        </span>
      </button>
    </div>

    <EmptyState v-if="!loading && dirs.length && !selectedDir" title="请选择共享位置" />

    <template v-if="selectedDir">
      <div class="panel dir-summary" data-motion>
        <div>
          <strong>{{ selectedDir.label || selectedDir.name }}</strong>
          <p>{{ selectedDir.description || selectedDir.root || selectedDir.id }}</p>
        </div>
        <div class="pill-row">
          <span class="pill" :class="selectedIsFileResource ? 'ok' : ''">{{ selectedIsFileResource ? '单文件' : '目录' }}</span>
          <span class="pill" :class="canDownload ? 'ok' : 'muted'">{{ canDownload ? '允许下载' : '禁止下载' }}</span>
          <span class="pill" :class="canUpload ? 'ok' : 'muted'">{{ canUpload ? '允许上传' : '禁止上传' }}</span>
        </div>
      </div>

      <div class="toolbar panel" data-motion>
        <nav class="breadcrumbs" aria-label="面包屑">
          <button v-for="crumb in crumbs" :key="crumb.path || 'root'" type="button" :disabled="loading" @click="navigateToPath(crumb.path)">
            {{ crumb.name }}
          </button>
        </nav>
        <div class="toolbar-actions">
          <RouterLink v-if="canUpload && !loading" class="ghost-btn" :to="{ name: 'upload', query: { dirId: selectedDirId, path: currentPath } }">上传到此处</RouterLink>
          <RouterLink v-if="isAdmin && canUpload && !loading" class="ghost-btn" :to="{ name: 'tokens', query: tokenQuery('upload') }">创建上传分享</RouterLink>
          <button class="ghost-btn" :disabled="loading || !currentPath" @click="navigateToPath(parentPath(currentPath))">返回上级</button>
        </div>
      </div>

      <div v-if="!listError" class="table-card files-list-card" :aria-busy="loading || loadingMore" data-motion>
        <div class="panel-head">
          <p ref="listSummaryRef" class="muted-text" role="status" tabindex="-1">{{ loading ? '正在读取当前路径…' : listingSummary }}</p>
        </div>
        <div v-if="actionError" ref="actionErrorRef" class="alert error state-error files-action-error" role="alert" tabindex="-1">
          <span>{{ actionError }}</span>
          <button class="ghost-btn" type="button" @click="dismissActionError">关闭提示</button>
        </div>
        <div v-if="truncated" class="alert info listing-notice" role="status" aria-live="polite">
          {{ truncationMessage }}<template v-if="!hasMore"> 当前扫描范围已全部显示，但目录中可能仍有未扫描内容。</template>
        </div>
        <table v-if="entries.length" id="files-list-table" class="data-table">
          <thead><tr><th>名称</th><th>大小</th><th>修改时间</th><th>操作</th></tr></thead>
          <tbody>
            <tr v-for="entry in entries" :key="entry.path || entry.name">
              <td data-label="名称">
                <button v-if="entryIsSafeDir(entry)" class="link-cell file-name" :disabled="loading" @click="openDir(entry)">
                  <span class="file-type-ico folder" aria-hidden="true" />
                  <span>{{ entry.name }}</span>
                </button>
                <span v-else class="file-name">
                  <span class="file-type-ico" :class="entryVisualKind(entry)" aria-hidden="true" />
                  <span>{{ entry.name }}</span>
                </span>
              </td>
              <td data-label="大小">{{ entry.metadataKnown === false ? '未知' : entryIsDir(entry) ? '目录' : formatBytes(entry.size ?? undefined) }}</td>
              <td data-label="修改时间">{{ entry.metadataKnown === false ? '未知' : formatDate(entry.modifiedAt || entry.mtime) }}</td>
              <td data-label="操作">
                <div class="row-actions">
                  <button v-if="entryCanDownload(entry)" class="mini-btn" type="button" :disabled="loading || Boolean(downloadingPath)" @click="startDownload(entry)">
                    {{ downloadingPath === (entry.path || joinPath(currentPath, entry.name)) ? '准备中…' : '下载' }}
                  </button>
                  <RouterLink v-if="isAdmin && !loading && entryCanDownload(entry)" class="mini-btn" :to="{ name: 'tokens', query: tokenQuery('download', entry) }">创建分享</RouterLink>
                  <RouterLink v-if="isAdmin && !loading && entryIsSafeDir(entry) && canUpload" class="mini-btn" :to="{ name: 'tokens', query: tokenQuery('upload', entry) }">上传分享</RouterLink>
                  <span v-if="!hasEntryAction(entry)" class="muted-text">{{ entryUnavailableReason(entry) }}</span>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
        <EmptyState v-else-if="!loading && !truncated" title="当前路径为空" />
        <EmptyState v-else-if="!loading && truncated" title="扫描范围内没有可显示项目" description="目录中可能仍有未扫描内容；可调整服务端扫描范围后重试。" />
        <div v-if="showLoadMoreFooter" class="load-more-footer">
          <div v-if="appendError" ref="appendErrorRef" class="alert error state-error" role="alert" tabindex="-1">
            <span>{{ appendError }}</span>
            <button class="ghost-btn" type="button" :disabled="loadingMore" @click="retryAppend">重试本页</button>
          </div>
          <div class="load-more-actions">
            <button
              v-if="!appendError"
              ref="loadMoreButtonRef"
              class="ghost-btn"
              type="button"
              aria-controls="files-list-table"
              :disabled="loading || loadingMore || !hasMore"
              @click="loadFiles(page + 1, true)"
            >{{ loadingMore ? '加载中…' : hasMore ? '加载更多' : '已全部加载' }}</button>
          </div>
        </div>
      </div>
    </template>
  </section>
</template>
