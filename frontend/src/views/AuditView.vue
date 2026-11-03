<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import AppIcon from '@/components/AppIcon.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { AuditLog } from '@/types'
import { formatDate } from '@/utils'

const logs = ref<AuditLog[]>([])
const loading = ref(true)
const error = ref('')
const page = ref(1)
const pageSize = ref(50)
const total = ref(0)
const totalPages = ref(0)
const lastSuccessfulAt = ref<Date | null>(null)
const dataStale = ref(false)
const keyword = ref('')
const status = ref('all')
const statusOptions = [
  { label: '全部记录', value: 'all' },
  { label: '成功', value: 'ok' },
  { label: '失败 / 拒绝', value: 'failed' },
]
const hasFilters = computed(() => Boolean(keyword.value.trim()) || status.value !== 'all')
const currentFilterKey = computed(() => `${status.value}\n${keyword.value.trim()}`)
let debounceTimer: number | undefined
let requestId = 0
let activeController: AbortController | undefined
const loadedFilterKey = ref('')
const displaysCurrentFilter = computed(() => loadedFilterKey.value === currentFilterKey.value)

async function load(nextPage = page.value) {
  const currentRequestId = ++requestId
  const requestedFilterKey = currentFilterKey.value
  const refreshingCurrentFilter = loadedFilterKey.value === requestedFilterKey
  if (!refreshingCurrentFilter) clearDisplayedResult()
  activeController?.abort()
  const controller = new AbortController()
  activeController = controller
  loading.value = true
  error.value = ''
  try {
    const result = await api.auditLogPage({
      page: nextPage,
      pageSize: pageSize.value,
      keyword: keyword.value.trim(),
      status: status.value,
    }, controller.signal)
    if (currentRequestId !== requestId) return
    logs.value = result.logs
    page.value = result.page
    pageSize.value = result.pageSize
    total.value = result.total
    totalPages.value = result.totalPages
    loadedFilterKey.value = requestedFilterKey
    lastSuccessfulAt.value = new Date()
    dataStale.value = false
  } catch (err) {
    if (currentRequestId !== requestId || (err instanceof ApiError && err.aborted)) return
    error.value = err instanceof ApiError ? err.message : '访问记录加载失败。'
    dataStale.value = refreshingCurrentFilter && lastSuccessfulAt.value !== null
  } finally {
    if (currentRequestId === requestId) {
      loading.value = false
      if (activeController === controller) activeController = undefined
    }
  }
}

function goPage(nextPage: number) {
  if (nextPage < 1 || (totalPages.value > 0 && nextPage > totalPages.value)) return
  void load(nextPage)
}

function clearDebounce() {
  if (debounceTimer !== undefined) window.clearTimeout(debounceTimer)
  debounceTimer = undefined
}

function clearDisplayedResult() {
  logs.value = []
  page.value = 1
  total.value = 0
  totalPages.value = 0
  lastSuccessfulAt.value = null
  dataStale.value = false
  loadedFilterKey.value = ''
}

function scheduleFilterLoad() {
  clearDebounce()
  requestId += 1
  activeController?.abort()
  activeController = undefined
  if (loadedFilterKey.value !== currentFilterKey.value) clearDisplayedResult()
  loading.value = true
  error.value = ''
  debounceTimer = window.setTimeout(() => {
    debounceTimer = undefined
    void load(1)
  }, 300)
}

function refresh() {
  clearDebounce()
  void load(loadedFilterKey.value === currentFilterKey.value ? page.value : 1)
}

function clearFilters() {
  const changed = Boolean(keyword.value) || status.value !== 'all'
  keyword.value = ''
  status.value = 'all'
  if (!changed) scheduleFilterLoad()
}

watch([keyword, status], scheduleFilterLoad, { flush: 'sync' })

onMounted(() => void load())
onUnmounted(() => {
  clearDebounce()
  requestId += 1
  activeController?.abort()
  activeController = undefined
})

const failureActions = new Set([
  'config_resource_published_sync_failed',
  'csrf_denied',
  'download_lease_file_changed',
  'download_lease_resource_changed',
  'file_picker_denied',
  'forbidden',
  'illegal_access',
  'login_failed',
  'login_rate_limited',
  'token_denied',
  'token_download_failed',
  'token_upload_denied',
  'token_upload_failed',
  'unauthorized',
  'upload_lease_failed',
  'upload_lease_resource_changed',
  'upload_temp_cleanup_failed',
])

function logOutcome(log: AuditLog): 'ok' | 'failed' {
  if (log.status === 'ok') return 'ok'
  if (log.status === 'failed') return 'failed'
  return failureActions.has(log.action.trim().toLowerCase()) ? 'failed' : 'ok'
}

function logTone(log: AuditLog) {
  if (logOutcome(log) === 'failed') return 'failed'
  if (log.action.trim().toLowerCase().startsWith('token_')) return 'token'
  return 'ok'
}

function statusText(log: AuditLog) {
  return logOutcome(log) === 'failed' ? '失败 / 拒绝' : '成功'
}
</script>

<template>
  <section class="page-stack audit-page">
    <header class="page-header split">
      <h1>访问记录<span v-if="displaysCurrentFilter && lastSuccessfulAt" class="count-badge">{{ total }}</span></h1>
      <button class="ghost-btn" :disabled="loading" @click="refresh">刷新</button>
    </header>

    <div class="data-workbench">
    <div class="filter-bar">
      <label>搜索记录<input v-model="keyword" maxlength="200" placeholder="IP、操作代码或详情" /></label>
      <label>状态
        <GlassSelect v-model="status" :options="statusOptions" aria-label="筛选访问记录状态" />
      </label>
      <button class="ghost-btn" type="button" :disabled="loading && !hasFilters" @click="clearFilters">清空筛选</button>
      <p v-if="keyword" class="filter-scope-note">搜索范围：IP、操作代码和详情，不包含中文操作名称。</p>
    </div>

    <StateBlock :loading="loading && !lastSuccessfulAt" :error="!lastSuccessfulAt ? error : ''" retry-label="重新加载" @retry="refresh" />
    <div v-if="displaysCurrentFilter && dataStale" class="alert info stale-alert" role="status">
      <span>刷新失败，当前显示的是 {{ lastSuccessfulAt?.toLocaleTimeString() }} 获取的旧数据。</span>
      <button class="ghost-btn" type="button" :disabled="loading" @click="refresh">立即重试</button>
    </div>
    <section v-if="displaysCurrentFilter && logs.length" class="table-card audit-records" :aria-busy="loading" aria-label="访问记录列表">
      <div class="audit-column-head" aria-hidden="true"><span>操作</span><span>结果</span><span>来源 IP</span><span>时间</span></div>
      <ol class="audit-list">
        <li v-for="(log, index) in logs" :key="log.id || index" class="audit-record" :data-status="logTone(log)">
          <details class="audit-event">
            <summary><span class="audit-dot" aria-hidden="true" /><strong>{{ log.actionLabel || log.action }}</strong><AppIcon name="chevron-down" :size="14" /></summary>
            <div class="audit-detail">
              <p v-if="log.detail || log.dirId || log.path">{{ log.detail || [log.dirId, log.path].filter(Boolean).join(' / ') }}</p>
              <code>{{ log.action }}</code>
            </div>
          </details>
          <span class="audit-result" :class="{ 'is-error': logOutcome(log) === 'failed' }">{{ statusText(log) }}</span>
          <span class="audit-source">{{ log.ip || '—' }}</span>
          <time class="audit-time" :datetime="log.createdAt || log.time">{{ formatDate(log.createdAt || log.time) }}</time>
        </li>
      </ol>
      <footer v-if="totalPages > 1" class="pagination-bar">
        <span>第 {{ page }} / {{ totalPages }} 页</span>
        <div class="row-actions">
          <button class="mini-btn" type="button" :disabled="loading || dataStale || page <= 1" @click="goPage(page - 1)">上一页</button>
          <button class="mini-btn" type="button" :disabled="loading || dataStale || page >= totalPages" @click="goPage(page + 1)">下一页</button>
        </div>
      </footer>
    </section>
    <EmptyState
      v-else-if="displaysCurrentFilter && !loading && !dataStale && (!error || lastSuccessfulAt)"
      :title="hasFilters ? '没有匹配记录' : '暂无访问记录'"
    />
    </div>
  </section>
</template>
