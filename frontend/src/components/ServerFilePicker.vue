<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import GlassSelect from '@/components/GlassSelect.vue'
import type { FilePickerItem, FilePickerRoot, FilePickerSelection } from '@/types'

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
const rootId = ref('')
const currentPath = ref('/')
const addressInput = ref('/')
const parentPath = ref('')
const items = ref<FilePickerItem[]>([])
const page = ref(1)
const pageSize = 100
const hasMore = ref(false)
const loading = ref(false)
const validating = ref(false)
const error = ref('')
const sortValue = ref('name:asc')
const suppressRootWatch = ref(false)

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
  emit('update:open', false)
}

function focusableElements() {
  return Array.from(dialogRef.value?.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])') || [])
}

function focusFirstControl() {
  void nextTick(() => focusableElements()[0]?.focus())
}

function handleDialogKeydown(event: KeyboardEvent) {
  if (event.key === 'Escape') {
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
  error.value = ''
  rootsLoaded.value = false
  try {
    roots.value = await api.filePickerRoots()
    const hadRoot = Boolean(rootId.value)
    if (!rootId.value && roots.value.length) rootId.value = roots.value[0].id
    if (hadRoot && rootId.value) await loadPath('/')
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '文件选择器根目录加载失败。'
  } finally {
    rootsLoaded.value = true
  }
}

async function loadPath(path: string, nextPage = 1, append = false) {
  if (!rootId.value) return
  loading.value = true
  error.value = ''
  try {
    const [sort, order] = sortValue.value.split(':')
    const result = await api.filePickerList(rootId.value, path, nextPage, pageSize, sort, order)
    currentPath.value = result.path || '/'
    addressInput.value = displayAddress(currentRoot.value, currentPath.value)
    parentPath.value = result.parentPath || ''
    sortValue.value = `${result.sort}:${result.order}`
    page.value = result.page
    hasMore.value = result.hasMore
    items.value = append ? [...items.value, ...result.items] : result.items
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '目录读取失败。'
  } finally {
    loading.value = false
  }
}

async function loadPathInRoot(nextRootId: string, path: string) {
  if (!nextRootId) return
  if (nextRootId !== rootId.value) {
    suppressRootWatch.value = true
    rootId.value = nextRootId
    await nextTick()
    suppressRootWatch.value = false
  }
  await loadPath(path)
}

function jumpToAddress() {
  const target = parseAddress(addressInput.value)
  if (!target) return
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
      error.value = `当前服务器没有 ${drive.toUpperCase()}: 入口。`
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
  if (item.type === 'directory' && item.readable) {
    void loadPath(item.path)
  }
}

async function selectPath(path: string) {
  if (!rootId.value || validating.value) return
  validating.value = true
  error.value = ''
  try {
    // 最终选择必须由后端重新校验，不能只信任前端列表里的 selectable 标记。
    const selection = await api.validateFilePickerSelection(rootId.value, path, props.mode)
    emit('confirm', selection)
    close()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '选择结果校验失败。'
  } finally {
    validating.value = false
  }
}

function formatSize(value?: number | null) {
  if (!value) return '—'
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  if (value < 1024 * 1024 * 1024) return `${(value / 1024 / 1024).toFixed(1)} MB`
  return `${(value / 1024 / 1024 / 1024).toFixed(1)} GB`
}

function itemTypeLabel(item: FilePickerItem) {
  if (item.symlink) return '符号链接'
  if (item.type === 'directory') return '目录'
  if (item.type === 'file') return '文件'
  return '其他'
}

watch(rootId, () => {
  if (suppressRootWatch.value) return
  currentPath.value = '/'
  addressInput.value = displayAddress(currentRoot.value, '/')
  items.value = []
  if (props.open && rootId.value) void loadPath('/')
})

watch(sortValue, () => {
  if (props.open && rootId.value) void loadPath(currentPath.value || '/')
})

watch(() => props.open, (open) => {
  if (open && !roots.value.length) void loadRoots()
  else if (open && rootId.value) void loadPath(currentPath.value || '/')
  if (open) focusFirstControl()
})

onMounted(() => {
  if (props.open) void loadRoots()
})

onBeforeUnmount(() => close())
</script>

<template>
  <Teleport to="body">
    <Transition name="fade">
      <div v-if="open" class="picker-backdrop" @click.self="close">
        <section ref="dialogRef" class="picker-dialog" role="dialog" aria-modal="true" aria-labelledby="server-file-picker-title" @keydown="handleDialogKeydown">
          <header class="picker-header">
            <div>
              <p class="eyebrow">Server picker</p>
              <h2 id="server-file-picker-title">选择服务端{{ mode === 'file' ? '文件' : '目录' }}</h2>
            </div>
            <button class="mini-btn" type="button" @click="close">关闭</button>
          </header>

          <div v-if="!rootsLoaded && !error" class="state-block"><span class="loader" /> 正在读取可选根目录…</div>
          <div v-if="rootsLoaded && !roots.length && !error" class="state-block">没有可用位置。</div>
          <div v-if="error" class="alert error">{{ error }}</div>

          <template v-if="roots.length">
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
                <button class="ghost-btn" type="button" :disabled="!parentPath || loading" @click="loadPath(parentPath || '/')">上级</button>
                <button class="ghost-btn" type="button" :disabled="loading" @click="loadPath(currentPath)">刷新</button>
                <button class="primary-btn" type="submit" :disabled="loading">跳转</button>
              </div>
            </form>

            <nav class="picker-breadcrumbs" aria-label="当前位置">
              <button v-for="crumb in breadcrumbs" :key="crumb.path" type="button" @click="loadPath(crumb.path)">{{ crumb.name }}</button>
            </nav>

            <div class="picker-list" :aria-busy="loading">
              <div class="picker-list-head">
                <span>名称</span><span>类型</span><span>大小</span><span>修改时间</span><span>操作</span>
              </div>
              <div v-for="item in items" :key="item.path" class="picker-row" :data-muted="!item.readable || item.symlink" @dblclick="enter(item)">
                <span class="picker-name"><span class="picker-icon">{{ item.type === 'directory' ? '📁' : item.symlink ? '↪' : '📄' }}</span>{{ item.name }}</span>
                <span>{{ itemTypeLabel(item) }}</span>
                <span>{{ formatSize(item.size) }}</span>
                <span>{{ item.modifiedAt ? new Date(item.modifiedAt).toLocaleString() : '—' }}</span>
                <span class="picker-row-actions">
                  <button v-if="item.type === 'directory'" class="mini-btn" type="button" @click.stop="enter(item)">进入</button>
                  <button class="mini-btn" type="button" :disabled="!item.selectable || item.type !== mode || validating" @click.stop="selectPath(item.path)">选择</button>
                </span>
              </div>
              <div v-if="loading" class="state-block compact"><span class="loader" /> 正在读取目录…</div>
              <div v-if="!loading && !items.length" class="state-block compact">当前目录没有可显示的文件或文件夹。</div>
            </div>

            <footer class="picker-footer">
              <div>
                <strong>{{ currentRoot?.name }}</strong>
                <small>{{ currentPath }}</small>
              </div>
              <div class="picker-actions">
                <button v-if="hasMore" class="ghost-btn" type="button" :disabled="loading" @click="loadPath(currentPath, page + 1, true)">加载更多</button>
                <button class="ghost-btn" type="button" @click="close">取消</button>
                <button v-if="mode === 'directory'" class="primary-btn" type="button" :disabled="!canSelectCurrentDirectory || validating" @click="selectPath(currentPath)">选择当前目录</button>
              </div>
            </footer>
          </template>
        </section>
      </div>
    </Transition>
  </Teleport>
</template>
