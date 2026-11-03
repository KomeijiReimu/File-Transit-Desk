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
const addressPending = ref(false)
const addressError = ref('')
const addressErrorRef = ref<HTMLElement | null>(null)
const selectedFilePath = ref('')
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
const selectionBlocked = computed(() => loading.value || validating.value || addressPending.value || Boolean(listError.value) || Boolean(rootError.value))
const listingSummary = computed(() => {
  if (totalKnown.value && total.value !== null) return items.value.length === total.value ? `${total.value} 项` : `已显示 ${items.value.length} / ${total.value} 项`
  return `已显示 ${items.value.length} 项`
})
const truncationMessage = computed(() => {
  const scanned = scannedEntries.value
  if (scanned > 0) return `当前列表和排序仅覆盖已扫描的 ${scanned} 项，文件夹仍有未显示内容。`
  return '文件夹尚未完整显示，排序仅适用于已扫描范围。'
})
const showLoadMoreFooter = computed(() => Boolean(items.value.length && (hasMore.value || loading.value || appendError.value || page.value > 1)))
const currentDirectorySelectionNote = computed(() => props.mode === 'directory' && !canSelectCurrentDirectory.value ? '当前文件夹不可选择。' : '')
const breadcrumbs = computed(() => {
  const clean = currentPath.value === '/' ? '' : currentPath.value.replace(/^\//, '')
  const parts = clean ? clean.split('/').filter(Boolean) : []
  const rootName = currentRoot.value?.name
  const out = [{ name: !rootName || rootName === '/' ? '根目录' : rootName, path: '/' }]
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
    addressPending.value = true
    addressError.value = ''
    selectedFilePath.value = ''
    items.value = []
    page.value = 1
    hasMore.value = false
    truncated.value = false
    totalKnown.value = false
    total.value = null
    scannedEntries.value = 0
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
    addressPending.value = false
    addressError.value = ''
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
    items.value = requestAppend ? appendUniqueByPath(items.value, result.items) : result.items
    failedPathRequest.value = null
    if (requestAppend) {
      const addedCount = Math.max(0, items.value.length - previousCount)
      liveMessage.value = `已新增 ${addedCount} 项，当前共显示 ${items.value.length} 项。${hasMore.value ? '' : truncated.value ? '扫描范围内的项目已显示，文件夹仍有未显示内容。' : '已全部加载。'}`
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

async function jumpToAddress() {
  if (loading.value || validating.value || !addressInput.value.trim()) return
  const requestId = ++pathRequestId
  addressPending.value = true
  selectedFilePath.value = ''
  addressError.value = ''
  actionError.value = ''
  loading.value = true
  try {
    const resolved = await api.resolveFilePickerAddress(addressInput.value.trim())
    if (requestId !== pathRequestId || !props.open) return
    if (resolved.filePath && props.mode === 'directory') {
      addressError.value = '该路径是文件，请选择文件夹。'
      await nextTick()
      addressErrorRef.value?.focus()
      return
    }
    const index = roots.value.findIndex((root) => root.id === resolved.root.id)
    if (index < 0) roots.value.push(resolved.root)
    else roots.value[index] = resolved.root
    await loadPathInRoot(resolved.root.id, resolved.path)
    if (!props.open) return
    if (listError.value) {
      addressError.value = listError.value
      await nextTick()
      addressErrorRef.value?.focus()
      return
    }
    if (!addressPending.value && resolved.filePath && props.mode === 'file') {
      selectedFilePath.value = resolved.filePath
      liveMessage.value = `已选中文件 ${resolved.filePath.split('/').pop()}，请确认选择。`
      await nextTick()
      pickerResultsRef.value?.querySelector('[data-selected="true"]')?.scrollIntoView({ block: 'nearest' })
    }
  } catch (err) {
    if (requestId !== pathRequestId) return
    addressError.value = err instanceof ApiError ? err.message : '无法打开此路径，请检查后重试。'
    await nextTick()
    addressErrorRef.value?.focus()
  } finally {
    if (requestId === pathRequestId) loading.value = false
  }
}

function selectAddressInput(event: FocusEvent) {
  ;(event.target as HTMLInputElement | null)?.select()
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
    addressInput.value = displayAddress(currentRoot.value, item.path)
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
  if (item.metadataKnown === false) return '无法读取文件信息'
  if (item.type === 'unknown' || item.type === 'other') return '类型无法识别'
  if (item.type !== props.mode) return ''
  if (item.selectable !== true) return '当前位置不允许选择'
  return ''
}

function itemReasonId(index: number) {
  return `picker-item-reason-${index}`
}

function navigateToPath(path: string) {
  failedPathRequest.value = null
  addressInput.value = displayAddress(currentRoot.value, path)
  void loadPath(path, 1, false, undefined, true)
}

function refreshCurrentPath() {
  failedPathRequest.value = null
  addressInput.value = displayAddress(currentRoot.value, currentPath.value)
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
  if (selectionBlocked.value || !rootId.value) return
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
    selectedFilePath.value = ''
    addressError.value = ''
    addressPending.value = false
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
        <section ref="dialogRef" class="picker-dialog" role="dialog" aria-modal="true" aria-labelledby="server-file-picker-title" @keydown="handleDialogKeydown">
          <header class="picker-header">
            <div>
              <h2 id="server-file-picker-title">选择服务器上的{{ mode === 'file' ? '文件' : '文件夹' }}</h2>
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
                <form class="picker-address" @submit.prevent="jumpToAddress">
                  <label for="picker-address-input">完整路径
                    <input id="picker-address-input" v-model="addressInput" autocomplete="off" spellcheck="false" placeholder="粘贴完整路径" :disabled="loading || validating" :aria-invalid="Boolean(addressError)" :aria-describedby="addressError ? 'picker-address-error' : undefined" @focus="selectAddressInput" @input="addressPending = true; addressError = ''; selectedFilePath = ''" />
                  </label>
                  <button class="primary-btn" type="submit" :disabled="loading || validating || !addressInput.trim()">{{ loading ? '打开中…' : '打开' }}</button>
                  <p v-if="addressError" id="picker-address-error" ref="addressErrorRef" class="field-error picker-address-error" role="alert" tabindex="-1">{{ addressError }}</p>
                </form>

                <div class="picker-toolbar">
                  <label>位置<GlassSelect v-model="rootId" :options="rootOptions" aria-label="选择服务端文件根目录" /></label>
                  <label>排序<GlassSelect v-model="sortValue" :options="sortOptions" aria-label="选择排序方式" /></label>
                </div>
                <div class="picker-location-tools">
                  <nav class="picker-breadcrumbs" aria-label="当前位置">
                    <button v-for="crumb in breadcrumbs" :key="crumb.path" type="button" :aria-label="crumb.name" :aria-current="crumb.path === currentPath ? 'location' : undefined" :title="crumb.name" :disabled="loading" @click="navigateToPath(crumb.path)">{{ crumb.name }}</button>
                  </nav>
                  <div class="picker-actions">
                    <button class="ghost-btn" type="button" :disabled="!parentPath || loading" @click="navigateToPath(parentPath || '/')">上级</button>
                    <button class="ghost-btn" type="button" :disabled="loading" @click="refreshCurrentPath">刷新</button>
                  </div>
                </div>

                <div v-if="actionError" ref="actionErrorRef" class="alert error state-error" role="alert" tabindex="-1">
                  <span>{{ actionError }}</span>
                  <button class="ghost-btn" type="button" @click="dismissActionError">关闭提示</button>
                </div>
                <div v-if="truncated" class="alert info listing-notice" role="status" aria-live="polite">
                  {{ truncationMessage }}
                </div>
              </div>

              <div ref="pickerResultsRef" class="picker-results" tabindex="-1">
                <div v-if="listError && !addressError" ref="listErrorRef" class="alert error state-error picker-list-error" role="alert" tabindex="-1">
                  <span>{{ listError }}</span>
                  <button class="ghost-btn" type="button" @click="retryLoadPath">重试</button>
                </div>

                <template v-else-if="!addressError">
                  <p ref="listSummaryRef" class="picker-list-summary muted-text" role="status" tabindex="-1">{{ loading && !items.length ? '正在读取当前路径…' : listingSummary }}</p>
                  <p class="visually-hidden" aria-live="polite">{{ liveMessage }}</p>

                  <div id="server-picker-list" class="picker-list" role="table" aria-label="服务端文件列表" :aria-busy="loading">
                    <div class="picker-list-head" role="row">
                      <span role="columnheader">名称</span><span role="columnheader">类型</span><span role="columnheader">大小</span><span role="columnheader">修改时间</span><span role="columnheader">操作</span>
                    </div>
                    <div v-for="(item, index) in items" :key="item.path" class="picker-row" role="row" :data-selected="selectedFilePath === item.path" :data-muted="!itemCanEnter(item) && !itemCanSelect(item)" @dblclick="enter(item)">
                      <span class="picker-cell picker-name" role="cell" data-label="名称">
                        <button v-if="itemCanEnter(item)" class="picker-name-value" type="button" :disabled="loading" @click="enter(item)">
                          <span class="picker-icon"><AppIcon name="folder" :size="18" /></span>
                          <span>{{ item.name }}</span>
                        </button>
                        <button v-else-if="mode === 'file' && itemCanSelect(item)" class="picker-name-value" type="button" :disabled="selectionBlocked" :aria-pressed="selectedFilePath === item.path" @click="selectedFilePath = item.path">
                          <span class="picker-icon"><AppIcon name="file" :size="18" /></span><span>{{ item.name }}</span>
                        </button>
                        <span v-else class="picker-name-value">
                          <span class="picker-icon" :data-kind="itemVisualKind(item)">
                            <span v-if="itemVisualKind(item) === 'unknown'" class="unknown-glyph" aria-hidden="true">?</span>
                            <AppIcon v-else :name="itemIcon(item)" :size="18" />
                          </span>
                          <span>{{ item.name }}</span>
                        </span>
                      </span>
                      <span class="picker-cell" role="cell" data-label="类型">{{ itemTypeLabel(item) }}</span>
                      <span class="picker-cell" role="cell" data-label="大小">{{ item.metadataKnown === false ? '未知' : formatSize(item.size) }}</span>
                      <span class="picker-cell" role="cell" data-label="修改时间">{{ item.metadataKnown === false ? '未知' : item.modifiedAt ? new Date(item.modifiedAt).toLocaleString('zh-CN', { hour12: false }) : '—' }}</span>
                      <span class="picker-cell picker-row-actions" role="cell" data-label="操作">
                        <span class="picker-action-buttons">
                          <button v-if="item.type === 'directory'" class="mini-btn" type="button" :disabled="loading || !itemCanEnter(item)" @click.stop="enter(item)">打开</button>
                          <button v-if="mode === 'file' && item.type === 'file'" class="mini-btn" type="button" :disabled="selectionBlocked || !itemCanSelect(item)" :aria-pressed="selectedFilePath === item.path" :aria-describedby="itemUnavailableReason(item) ? itemReasonId(index) : undefined" @click.stop="selectedFilePath = item.path">{{ selectedFilePath === item.path ? '已选' : '选中' }}</button>
                          <button v-else-if="item.type === mode" class="mini-btn" type="button" :disabled="selectionBlocked || !itemCanSelect(item)" :aria-describedby="itemUnavailableReason(item) ? itemReasonId(index) : undefined" @click.stop="selectPath(item.path)">选择</button>
                        </span>
                        <small v-if="itemUnavailableReason(item)" :id="itemReasonId(index)" class="picker-unavailable-reason">{{ itemUnavailableReason(item) }}</small>
                      </span>
                    </div>
                    <div v-if="loading" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="state-block compact"><span class="loader" aria-hidden="true" /> 正在读取目录…</div></div>
                    <div v-if="!loading && !items.length && !truncated" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="state-block compact">当前目录没有可显示的文件或文件夹。</div></div>
                    <div v-else-if="!loading && !items.length && truncated" class="picker-table-state" role="row"><div role="cell" aria-colspan="5" class="empty-state compact"><strong>扫描范围内没有可显示项目</strong></div></div>
                  </div>

                  <div v-if="showLoadMoreFooter" class="load-more-footer picker-load-more-footer">
                    <div v-if="appendError" ref="appendErrorRef" class="alert error state-error" role="alert" tabindex="-1">
                      <span>{{ appendError }}</span>
                      <button class="ghost-btn" type="button" :disabled="loading" @click="retryLoadPath">重试本页</button>
                    </div>
                    <div class="load-more-actions">
                      <button
                        v-if="!appendError"
                        ref="loadMoreButtonRef"
                        class="ghost-btn"
                        type="button"
                        aria-controls="server-picker-list"
                        :disabled="loading || !hasMore"
                        @click="loadNextPage"
                      >{{ loading ? '加载中…' : hasMore ? '加载更多' : truncated ? '已显示扫描范围内的项目' : '已全部显示' }}</button>
                    </div>
                  </div>
                </template>
              </div>
            </template>
          </div>

          <footer v-if="roots.length" class="picker-footer">
            <div class="picker-current-location">
              <strong>{{ addressPending ? '待打开' : selectedFilePath ? selectedFilePath.split('/').pop() : '当前文件夹' }}</strong>
              <small>{{ addressPending ? addressInput : displayAddress(currentRoot, selectedFilePath || currentPath) }}</small>
              <small v-if="currentDirectorySelectionNote">{{ currentDirectorySelectionNote }}</small>
            </div>
            <div class="picker-actions">
              <button class="ghost-btn" type="button" @click="close">取消</button>
              <button v-if="mode === 'directory'" class="primary-btn" type="button" :disabled="selectionBlocked || !canSelectCurrentDirectory" :aria-busy="validating" @click="selectPath(currentPath)">{{ validating ? '确认中…' : '选择此文件夹' }}</button>
              <button v-else class="primary-btn" type="button" :disabled="selectionBlocked || !selectedFilePath" :aria-busy="validating" @click="selectPath(selectedFilePath)">{{ validating ? '确认中…' : '选择文件' }}</button>
            </div>
          </footer>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
