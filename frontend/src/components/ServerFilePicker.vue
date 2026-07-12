<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import type { FilePickerItem, FilePickerRoot, FilePickerSelection } from '@/types'
import { acquireModalIsolation } from '@/useModalIsolation'

const props = defineProps<{
  open: boolean
  mode: 'file' | 'directory'
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
  confirm: [selection: FilePickerSelection]
}>()

const roots = ref<FilePickerRoot[]>([])
const rootsLoaded = ref(false)
const dialogRef = ref<HTMLElement | null>(null)
const closeRef = ref<HTMLButtonElement | null>(null)
const rootId = ref('')
const currentPath = ref('/')
const addressInput = ref('/')
const parentPath = ref('')
const items = ref<FilePickerItem[]>([])
const page = ref(1)
const defaultPageSize = 100
const effectivePageSize = ref(defaultPageSize)
const hasMore = ref(false)
const truncated = ref(false)
const totalKnown = ref(false)
const total = ref<number | null>(null)
const scannedEntries = ref(0)
const scanLimit = ref(0)
const loading = ref(false)
const validating = ref(false)
const rootError = ref('')
const listError = ref('')
const appendError = ref('')
const actionError = ref('')
const sortValue = ref('name:asc')
const suppressRootWatch = ref(false)
const suppressSortWatch = ref(false)
let rootsRequestId = 0
let pathRequestId = 0
let validationRequestId = 0
let returnFocusEl: HTMLElement | null = null
let releaseModalIsolation: (() => void) | undefined
interface FailedPathRequest {
  rootId: string
  path: string
  sortValue: string
  page: number
  pageSize: number
  append: boolean
}
const failedPathRequest = ref<FailedPathRequest | null>(null)
const liveMessage = ref('')
const pickerResultsRef = ref<HTMLElement | null>(null)
const listSummaryRef = ref<HTMLElement | null>(null)
const rootErrorRef = ref<HTMLElement | null>(null)
const listErrorRef = ref<HTMLElement | null>(null)
const appendErrorRef = ref<HTMLElement | null>(null)
const actionErrorRef = ref<HTMLElement | null>(null)
const loadMoreButtonRef = ref<HTMLButtonElement | null>(null)

const rootOptions = computed(() => roots.value.map((root) => ({ label: root.name, value: root.id })))
const sortOptions = [
  { label: '名称 A-Z', value: 'name:asc' },
  { label: '名称 Z-A', value: 'name:desc' },
  { label: '类型', value: 'type:asc' },
  { label: '大小从大到小', value: 'size:desc' },
  { label: '大小从小到大', value: 'size:asc' },
  { label: '最新修改', value: 'modifiedAt:desc' },
  { label: '最早修改', value: 'modifiedAt:asc' },
]
const currentRoot = computed(() => roots.value.find((root) => root.id === rootId.value))
const canSelectCurrentDirectory = computed(() => props.mode === 'directory' && Boolean(currentRoot.value?.allowSelectDirs))
const listingSummary = computed(() => {
  if (totalKnown.value && total.value !== null) return `共 ${total.value} 项 · 已显示 ${items.value.length} 项`
  return `总数未知 · 已显示 ${items.value.length} 项`
})
const truncationMessage = computed(() => {
  const scanned = scannedEntries.value || scanLimit.value
  if (scanned > 0) return `服务端最多扫描了前 ${scanned} 个目录项；当前列表和排序只覆盖已扫描范围，未扫描部分不计入。`
  return '目录内容超过服务端扫描窗口；当前列表和排序只覆盖已扫描范围，未扫描部分不计入。'
})
const showLoadMoreFooter = computed(() => Boolean(items.value.length && (hasMore.value || loading.value || appendError.value || page.value > 1)))
const currentDirectorySelectionNote = computed(() => props.mode === 'directory' && !canSelectCurrentDirectory.value ? '当前位置不允许直接作为选择结果。' : '')
const breadcrumbs = computed(() => {
  const clean = currentPath.value === '/' ? '' : currentPath.value.replace(/^\//, '')
  const parts = clean ? clean.split('/').filter(Boolean) : []
  const out = [{ name: currentRoot.value?.name || '根目录', path: '/' }]
  let acc = ''
  parts.forEach((part) => {
    acc += `/${part}`
    out.push({ name: part, path: acc })
  })
  return out
})

function close() {
  pathRequestId += 1
  validationRequestId += 1
  loading.value = false
  validating.value = false
  emit('update:open', false)
}

function focusableElements() {
  return Array.from(dialogRef.value?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])') || [])
}

function focusFirstControl() {
  void nextTick(() => closeRef.value?.focus())
}

function handleDialogKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') {
    event.preventDefault()
    close()
    return
  }
  if (event.key !== 'Tab') return
  const focusables = focusableElements()
  if (!focusables.length) return
  const first = focusables[0]
  const last = focusables[focusables.length - 1]
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault()
    last.focus()
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault()
    first.focus()
  }
}

async function loadRoots() {
  const requestId = ++rootsRequestId
  rootError.value = ''
  listError.value = ''
  appendError.value = ''
  actionError.value = ''
  failedPathRequest.value = null
  rootsLoaded.value = false
  try {
    const result = await api.filePickerRoots()
    if (requestId !== rootsRequestId) return
    roots.value = result
    const hadRoot = Boolean(rootId.value)
    if (!rootId.value && roots.value.length) rootId.value = roots.value[0].id
    if (hadRoot && rootId.value) await loadPath('/')
  } catch (err) {
    if (requestId !== rootsRequestId) return
    rootError.value = err instanceof ApiError ? err.message : '文件选择器根目录加载失败。'
    await nextTick()
    rootErrorRef.value?.focus()
  } finally {
    if (requestId === rootsRequestId) rootsLoaded.value = true
  }
}

async function retryRoots() {
  closeRef.value?.focus({ preventScroll: true })
  await loadRoots()
  if (!rootError.value && roots.value.length) {
    await nextTick()
    listSummaryRef.value?.focus()
  }
}

async function loadPath(path: string, nextPage = 1, append = false, retryRequest?: FailedPathRequest, focusAfter = false) {
  const targetRootId = retryRequest?.rootId || rootId.value
  if (!targetRootId) return
  const requestId = ++pathRequestId
  validationRequestId += 1
  validating.value = false
  const targetSortValue = retryRequest?.sortValue || sortValue.value
  const requestPage = retryRequest?.page ?? nextPage
  const requestPageSize = retryRequest?.pageSize ?? (append ? effectivePageSize.value : defaultPageSize)
  const requestAppend = retryRequest?.append ?? append
  const failedRequest: FailedPathRequest = {
    rootId: targetRootId,
    path,
    sortValue: targetSortValue,
    page: requestPage,
    pageSize: requestPageSize,
    append: requestAppend,
  }
  if (!retryRequest) failedPathRequest.value = null
  const previousCount = items.value.length
  const activeElement = document.activeElement
  const appendTriggeredFromControls = requestAppend && (
    activeElement === loadMoreButtonRef.value
    || Boolean(appendErrorRef.value && activeElement instanceof Node && appendErrorRef.value.contains(activeElement))
  )
  if (!requestAppend && focusAfter && activeElement instanceof Node && pickerResultsRef.value?.contains(activeElement)) {
    pickerResultsRef.value.focus({ preventScroll: true })
  }
  loading.value = true
  appendError.value = ''
  if (!requestAppend) {
    items.value = []
    page.value = 1
    hasMore.value = false
    truncated.value = false
    totalKnown.value = false
    total.value = null
    scannedEntries.value = 0
    scanLimit.value = 0
    effectivePageSize.value = defaultPageSize
    listError.value = ''
    actionError.value = ''
  }
  let focusTarget: 'summary' | 'load-more' | 'list-error' | 'append-error' | null = null
  try {
    const [sort, order] = targetSortValue.split(':')
    const result = await api.filePickerList(targetRootId, path, requestPage, requestPageSize, sort, order)
    if (requestId !== pathRequestId || targetRootId !== rootId.value || targetSortValue !== sortValue.value) return
    currentPath.value = result.path || '/'
    addressInput.value = displayAddress(currentRoot.value, currentPath.value)
    parentPath.value = result.parentPath || ''
    const effectiveSortValue = `${result.sort}:${result.order}`
    if (effectiveSortValue !== sortValue.value) {
      suppressSortWatch.value = true
      sortValue.value = effectiveSortValue
      suppressSortWatch.value = false
    }
    page.value = result.page ?? requestPage
    effectivePageSize.value = typeof result.pageSize === 'number' && result.pageSize > 0 ? result.pageSize : requestPageSize
    hasMore.value = result.hasMore === true
    truncated.value = result.truncated === true
    totalKnown.value = result.totalKnown === true
    total.value = typeof result.total === 'number' ? result.total : null
    scannedEntries.value = result.scannedEntries ?? items.value.length + result.items.length
    scanLimit.value = result.scanLimit ?? 0
    items.value = requestAppend ? appendUniqueByPath(items.value, result.items) : result.items
    failedPathRequest.value = null
    if (requestAppend) {
      const addedCount = Math.max(0, items.value.length - previousCount)
      liveMessage.value = `已新增 ${addedCount} 项，当前共显示 ${items.value.length} 项。${hasMore.value ? '' : '已全部加载。'}`
      if (appendTriggeredFromControls) focusTarget = hasMore.value ? 'load-more' : 'summary'
    } else if (focusAfter) {
      focusTarget = 'summary'
    }
  } catch (err) {
    if (requestId !== pathRequestId) return
    failedPathRequest.value = failedRequest
    if (requestAppend) {
      appendError.value = err instanceof ApiError ? err.message : '后续目录内容加载失败。'
      focusTarget = 'append-error'
    } else {
      listError.value = err instanceof ApiError ? err.message : '目录读取失败。'
      focusTarget = 'list-error'
    }
  } finally {
    if (requestId === pathRequestId) {
      loading.value = false
      await nextTick()
      if (focusTarget === 'summary') listSummaryRef.value?.focus()
      else if (focusTarget === 'load-more') loadMoreButtonRef.value?.focus()
      else if (focusTarget === 'list-error') listErrorRef.value?.focus()
      else if (focusTarget === 'append-error') appendErrorRef.value?.focus()
    }
  }
}

function appendUniqueByPath(current: FilePickerItem[], incoming: FilePickerItem[]) {
  const seen = new Set(current.map((item) => item.path))
  return [...current, ...incoming.filter((item) => {
    if (seen.has(item.path)) return false
    seen.add(item.path)
    return true
  })]
}

function retryLoadPath() {
  const failed = failedPathRequest.value
  if (!failed) return
  void loadPath(failed.path, failed.page, failed.append, failed, true)
}

async function loadPathInRoot(nextRootId: string, path: string) {
  if (!nextRootId) return
  if (nextRootId !== rootId.value) {
    failedPathRequest.value = null
    loading.value = true
    items.value = []
    currentPath.value = '/'
    parentPath.value = ''
    suppressRootWatch.value = true
    rootId.value = nextRootId
    await nextTick()
    suppressRootWatch.value = false
  }
  await loadPath(path, 1, false, undefined, true)
}

function jumpToAddress() {
  actionError.value = ''
  const target = parseAddress(addressInput.value)
  if (!target) {
    void nextTick(() => actionErrorRef.value?.focus())
    return
  }
  void loadPathInRoot(target.rootId, target.path)
}

function selectAddressInput(event: FocusEvent) {
  ;(event.target as HTMLInputElement | null)?.select()
}

function parseAddress(rawValue: string): { rootId: string; path: string } | null {
  const raw = rawValue.trim()
  if (!raw) return { rootId: rootId.value, path: '/' }
  const windowsDrive = raw.match(/^([a-zA-Z]):[\\/]?(.*)$/)
  if (windowsDrive) {
    const drive = windowsDrive[1].toLowerCase()
    const root = roots.value.find((item) => rootDrive(item) === drive || item.id === `drive_${drive}`)
    if (!root) {
      actionError.value = `当前服务器没有 ${drive.toUpperCase()}: 入口。`
      return null
    }
    return { rootId: root.id, path: toVirtualPath(windowsDrive[2] || '') }
  }
  if (raw.startsWith('/') || raw.startsWith('\\')) {
    const systemRoot = roots.value.find((item) => rootComparablePath(item) === '' || item.id === 'system_root')
    if (systemRoot) return { rootId: systemRoot.id, path: toVirtualPath(raw) }
    const matched = roots.value
      .map((item) => ({ item, base: rootComparablePath(item) }))
      .filter(({ base }) => base && normalizedPath(raw).startsWith(base))
      .sort((a, b) => b.base.length - a.base.length)[0]
    if (matched) {
      const rest = normalizedPath(raw).slice(matched.base.length)
      return { rootId: matched.item.id, path: toVirtualPath(rest) }
    }
  }
  return { rootId: rootId.value, path: toVirtualPath(raw) }
}

function toVirtualPath(value: string) {
  const cleaned = value.replace(/\\/g, '/').replace(/^\/+/, '')
  return cleaned ? `/${cleaned}` : '/'
}

function normalizedPath(value: string) {
  return value.trim().replace(/\\/g, '/').replace(/\/+$/g, '').toLowerCase()
}

function rootComparablePath(root?: FilePickerRoot) {
  if (!root) return ''
  const raw = root.path || root.name || ''
  if (raw === '/') return ''
  return normalizedPath(raw)
}

function rootDrive(root?: FilePickerRoot) {
  const raw = (root?.path || root?.name || '').trim()
  const match = raw.match(/^([a-zA-Z]):[\\/]?$/)
  return match?.[1].toLowerCase() || ''
}

function displayAddress(root: FilePickerRoot | undefined, path: string) {
  const base = root?.path || root?.name || ''
  const rel = path.replace(/^\//, '')
  if (/^[a-zA-Z]:[\\/]?$/.test(base)) return rel ? `${base.replace(/[\\/]?$/, '\\')}${rel.replace(/\//g, '\\')}` : base
  if (base === '/' || !base) return path || '/'
  return rel ? `${base.replace(/[\\/]$/, '')}/${rel}` : base
}

function enter(item: FilePickerItem) {
  if (!loading.value && itemCanEnter(item)) {
    void loadPath(item.path, 1, false, undefined, true)
  }
}

function itemCanEnter(item: FilePickerItem) {
  return item.type === 'directory' && item.readable !== false && item.metadataKnown !== false
}

function itemCanSelect(item: FilePickerItem) {
  return item.selectable === true && item.readable !== false && item.metadataKnown !== false && item.type === props.mode
}

function itemVisualKind(item: FilePickerItem) {
  if (item.metadataKnown === false || item.type === 'unknown' || item.type === 'other') return 'unknown'
  if (item.symlink || item.type === 'symlink') return 'link'
  return item.type === 'directory' ? 'folder' : 'file'
}

function itemUnavailableReason(item: FilePickerItem) {
  if (item.readable === false) return '不可读取'
  if (item.metadataKnown === false) return '元数据不可用'
  if (item.type === 'unknown' || item.type === 'other') return '类型无法识别'
  if (item.type !== props.mode) return props.mode === 'file' ? '当前需要选择文件' : '当前需要选择目录'
  if (item.selectable !== true) return '当前位置不允许选择'
  return ''
}

function itemReasonId(index: number) {
  return `picker-item-reason-${index}`
}

function navigateToPath(path: string) {
  failedPathRequest.value = null
  void loadPath(path, 1, false, undefined, true)
}

function refreshCurrentPath() {
  failedPathRequest.value = null
  void loadPath(currentPath.value || '/', 1, false, undefined, true)
}

function loadNextPage() {
  if (!hasMore.value || loading.value) return
  void loadPath(currentPath.value, page.value + 1, true)
}

async function dismissActionError() {
  actionError.value = ''
  await nextTick()
  if (listSummaryRef.value) listSummaryRef.value.focus()
  else pickerResultsRef.value?.focus()
}

async function selectPath(path: string) {
  if (loading.value || !rootId.value || validating.value) return
  const requestId = ++validationRequestId
  const targetRootId = rootId.value
  validating.value = true
  actionError.value = ''
  try {
    // 最终选择必须由后端重新校验，不能只信任前端列表里的 selectable 标记。
    const selection = await api.validateFilePickerSelection(targetRootId, path, props.mode)
    if (requestId !== validationRequestId || targetRootId !== rootId.value) return
    emit('confirm', selection)
    close()
  } catch (err) {
    if (requestId !== validationRequestId) return
    actionError.value = err instanceof ApiError ? err.message : '选择结果校验失败。'
    await nextTick()
    actionErrorRef.value?.focus()
  } finally {
    if (requestId === validationRequestId) validating.value = false
  }
}

function formatSize(value?: number | null) {
  if (value === undefined || value === null) return '—'
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  if (value < 1024 * 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)} MB`
  return `${(value / 1024 / 1024 / 1024).toFixed(1)} GB`
}

function itemTypeLabel(item: FilePickerItem) {
  if (item.type === 'directory') return item.symlink ? '目录 · 符号链接' : '目录'
  if (item.type === 'file') return item.symlink ? '文件 · 符号链接' : '文件'
  if (item.symlink || item.type === 'symlink') return '符号链接'
  if (item.type === 'unknown' || item.metadataKnown === false) return '未知'
  return '其他'
}

function itemIcon(item: FilePickerItem) {
  if (item.symlink) return 'link'
  if (item.type === 'directory') return 'folder'
  return 'file'
}

watch(rootId, () => {
  if (suppressRootWatch.value) return
  const focusAfter = rootsLoaded.value
  failedPathRequest.value = null
  listError.value = ''
  appendError.value = ''
  actionError.value = ''
  currentPath.value = '/'
  addressInput.value = displayAddress(currentRoot.value, '/')
  items.value = []
  if (props.open && rootId.value) void loadPath('/', 1, false, undefined, focusAfter)
}, { flush: 'sync' })

watch(sortValue, () => {
  if (suppressSortWatch.value) return
  failedPathRequest.value = null
  listError.value = ''
  appendError.value = ''
  actionError.value = ''
  if (props.open && rootId.value) void loadPath(currentPath.value || '/', 1, false, undefined, true)
}, { flush: 'sync' })

watch(() => props.open, (open) => {
  if (open) {
    returnFocusEl = document.activeElement instanceof HTMLElement ? document.activeElement : null
    releaseModalIsolation?.()
    releaseModalIsolation = acquireModalIsolation()
    if (!roots.value.length) void loadRoots()
    else if (rootId.value) void loadPath(currentPath.value || '/')
    focusFirstControl()
  } else {
    releaseModalIsolation?.()
    releaseModalIsolation = undefined
    void nextTick(() => {
      returnFocusEl?.focus()
      returnFocusEl = null
    })
  }
})

onMounted(() => {
  if (props.open) void loadRoots()
})

onBeforeUnmount(() => {
  rootsRequestId += 1
  pathRequestId += 1
  validationRequestId += 1
  releaseModalIsolation?.()
})
</script>

<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="open" class="picker-backdrop" @click.self="close">
        <section ref="dialogRef" class="picker-dialog" role="dialog" aria-modal="true" aria-labelledby="server-file-picker-title" aria-describedby="server-file-picker-description" @keydown="handleDialogKeydown">
          <header class="picker-header">
            <div>
              <p class="eyebrow">服务端位置</p>
              <h2 id="server-file-picker-title">选择服务端{{ mode === 'file' ? '文件' : '目录' }}</h2>
              <p id="server-file-picker-description">此选择器只用于定位现有文件或目录，不会修改服务端内容。</p>
            </div>
            <button ref="closeRef" class="mini-btn" type="button" @click="close">关闭</button>
          </header>

          <div class="picker-main">
            <div v-if="!rootsLoaded && !rootError" class="state-block" role="status"><span class="loader" aria-hidden="true" /> 正在读取可选根目录…</div>
            <div v-if="rootsLoaded && !roots.length && !rootError" class="state-block">没有可用位置。</div>
            <div v-if="rootError" ref="rootErrorRef" class="alert error state-error" role="alert" tabindex="-1">
              <span>{{ rootError }}</span>
              <button class="ghost-btn" type="button" @click="retryRoots">重新加载位置</button>
            </div>

            <template v-if="roots.length">
              <div class="picker-controls">
                <div class="picker-toolbar">
                  <label>位置
                    <GlassSelect v-model="rootId" :options="rootOptions" aria-label="选择服务端文件根目录" />
                  </label>
                  <label>排序
                    <GlassSelect v-model="sortValue" :options="sortOptions" aria-label="选择排序方式" />
                  </label>
                </div>

                <form class="picker-address" @submit.prevent="jumpToAddress">
                  <label>路径
                    <input v-model.trim="addressInput" autocomplete="off" spellcheck="false" placeholder="输入路径后回车跳转" :disabled="loading" @focus="selectAddressInput" />
                  </label>
                  <div class="picker-actions">
                    <button class="ghost-btn" type="button" :disabled="!parentPath || loading" @click="navigateToPath(parentPath || '/')">上级</button>
                    <button class="ghost-btn" type="button" :disabled="loading" @click="refreshCurrentPath">刷新</button>
                    <button class="primary-btn" type="submit" :disabled="loading">跳转</button>
                  </div>
                </form>

                <nav class="picker-breadcrumbs" aria-label="当前位置">
                  <button v-for="crumb in breadcrumbs" :key="crumb.path" type="button" :disabled="loading" @click="navigateToPath(crumb.path)">{{ crumb.name }}</button>
                </nav>

                <div v-if="actionError" ref="actionErrorRef" class="alert error state-error" role="alert" tabindex="-1">
                  <span>{{ actionError }}</span>
                  <button class="ghost-btn" type="button" @click="dismissActionError">关闭提示</button>
                </div>
                <div v-if="truncated" class="alert info listing-notice" role="status" aria-live="polite">
                  {{ truncationMessage }}<template v-if="!hasMore"> 当前扫描范围已全部显示，但目录中可能仍有未扫描内容。</template>
                </div>
              </div>

              <div ref="pickerResultsRef" class="picker-results" tabindex="-1">
                <div v-if="listError" ref="listErrorRef" class="alert error state-error picker-list-error" role="alert" tabindex="-1">
                  <span>{{ listError }}</span>
                  <button class="ghost-btn" type="button" @click="retryLoadPath">重试原请求</button>
                </div>

                <template v-else>
                  <p ref="listSummaryRef" class="picker-list-summary muted-text" role="status" tabindex="-1">{{ loading && !items.length ? '正在读取当前路径…' : listingSummary }}</p>
                  <p class="visually-hidden" aria-live="polite">{{ liveMessage }}</p>

                  <div id="server-picker-list" class="picker-list" role="table" aria-label="服务端文件列表" :aria-busy="loading">
                    <div class="picker-list-head" role="row">
                      <span role="columnheader">名称</span><span role="columnheader">类型</span><span role="columnheader">大小</span><span role="columnheader">修改时间</span><span role="columnheader">操作</span>
                    </div>
                    <div v-for="(item, index) in items" :key="item.path" class="picker-row" role="row" :data-muted="!itemCanEnter(item) && !itemCanSelect(item)" @dblclick="enter(item)">
                      <span class="picker-cell picker-name" role="cell" data-label="名称">
                        <span class="picker-name-value">
                          <span class="picker-icon" :data-kind="itemVisualKind(item)">
                            <span v-if="itemVisualKind(item) === 'unknown'" class="unknown-glyph" aria-hidden="true">?</span>
                            <AppIcon v-else :name="itemIcon(item)" :size="18" />
                          </span>
                          <span>{{ item.name }}</span>
                        </span>
                      </span>
                      <span class="picker-cell" role="cell" data-label="类型">{{ itemTypeLabel(item) }}</span>
                      <span class="picker-cell" role="cell" data-label="大小">{{ item.metadataKnown === false ? '未知' : formatSize(item.size) }}</span>
                      <span class="picker-cell" role="cell" data-label="修改时间">{{ item.metadataKnown === false ? '未知' : item.modifiedAt ? new Date(item.modifiedAt).toLocaleString() : '—' }}</span>
                      <span class="picker-cell picker-row-actions" role="cell" data-label="操作">
                        <span class="picker-action-buttons">
                          <button v-if="item.type === 'directory'" class="mini-btn" type="button" :disabled="loading || !itemCanEnter(item)" @click.stop="enter(item)">进入</button>
                          <button class="mini-btn" type="button" :disabled="loading || !itemCanSelect(item) || validating" :aria-describedby="itemUnavailableReason(item) ? itemReasonId(index) : undefined" @click.stop="selectPath(item.path)">选择</button>
                        </span>
                        <small v-if="itemUnavailableReason(item)" :id="itemReasonId(index)" class="picker-unavailable-reason">{{ itemUnavailableReason(item) }}</small>
                      </span>
                    </div>
                    <div v-if="loading" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="state-block compact"><span class="loader" aria-hidden="true" /> 正在读取目录…</div></div>
                    <div v-if="!loading && !items.length && !truncated" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="state-block compact">当前目录没有可显示的文件或文件夹。</div></div>
                    <div v-else-if="!loading && !items.length && truncated" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="empty-state compact"><strong>扫描范围内没有可显示项目</strong><small>目录中可能仍有未扫描内容。</small></div></div>
                  </div>

                  <div v-if="showLoadMoreFooter" class="load-more-footer picker-load-more-footer">
                    <div v-if="appendError" ref="appendErrorRef" class="alert error state-error" role="alert" tabindex="-1">
                      <span>{{ appendError }}</span>
                      <button class="ghost-btn" type="button" :disabled="loading" @click="retryLoadPath">重试本页</button>
                    </div>
                    <div class="load-more-actions">
                      <span class="muted-text">{{ listingSummary }}</span>
                      <button
                        v-if="!appendError"
                        ref="loadMoreButtonRef"
                        class="ghost-btn"
                        type="button"
                        aria-controls="server-picker-list"
                        :disabled="loading || !hasMore"
                        @click="loadNextPage"
                      >{{ loading ? '加载中…' : hasMore ? '加载更多' : '已全部加载' }}</button>
                    </div>
                  </div>
                </template>
              </div>
            </template>
          </div>

          <footer v-if="roots.length" class="picker-footer">
            <div class="picker-current-location">
              <strong>{{ currentRoot?.name }}</strong>
              <small>{{ currentPath }}</small>
              <small v-if="currentDirectorySelectionNote">{{ currentDirectorySelectionNote }}</small>
            </div>
            <div class="picker-actions">
              <button class="ghost-btn" type="button" @click="close">取消</button>
              <button v-if="mode === 'directory'" class="primary-btn" type="button" :disabled="loading || Boolean(listError) || !canSelectCurrentDirectory || validating" @click="selectPath(currentPath)">选择当前目录</button>
            </div>
          </footer>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
