<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
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
  { label: '正常行为', value: 'ok' },
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
  return logOutcome(log) === 'failed' ? '需关注' : '完成'
}
</script>

<template>
  <section class="page-stack audit-page">
    <header class="page-header split">
      <h1>访问记录</h1>
      <button class="ghost-btn" :disabled="loading" @click="refresh">刷新</button>
    </header>

    <div class="panel filter-bar">
      <label>筛选全部记录<input v-model="keyword" maxlength="200" placeholder="按动作代码、IP 或详情筛选" /></label>
      <label>状态
        <GlassSelect v-model="status" :options="statusOptions" aria-label="筛选访问记录状态" />
      </label>
      <button class="ghost-btn" type="button" :disabled="loading && !hasFilters" @click="clearFilters">清空筛选</button>
      <p class="filter-scope-note">筛选覆盖全部审计记录，支持动作代码、IP 和详情；界面中的中文动作名称不参与关键词匹配。</p>
    </div>

    <div v-if="displaysCurrentFilter && total > 0" class="panel pagination-bar">
      <span>第 {{ page }} / {{ totalPages || 1 }} 页，共 {{ total }} 条</span>
      <div class="row-actions">
        <button class="mini-btn" type="button" :disabled="loading || dataStale || page <= 1" @click="goPage(page - 1)">上一页</button>
        <button class="mini-btn" type="button" :disabled="loading || dataStale || page >= totalPages" @click="goPage(page + 1)">下一页</button>
      </div>
    </div>

    <StateBlock :loading="loading && !lastSuccessfulAt" :error="!lastSuccessfulAt ? error : ''" retry-label="重新加载" @retry="refresh" />
    <div v-if="displaysCurrentFilter && dataStale" class="alert info stale-alert" role="status">
      <span>刷新失败，当前显示的是 {{ lastSuccessfulAt?.toLocaleTimeString() }} 获取的旧数据。</span>
      <button class="ghost-btn" type="button" :disabled="loading" @click="refresh">立即重试</button>
    </div>
    <div class="timeline" v-if="displaysCurrentFilter && logs.length" :aria-busy="loading">
      <article v-for="(log, index) in logs" :key="log.id || index" class="timeline-item" :data-status="logTone(log)">
        <span class="timeline-dot" aria-hidden="true" />
        <div>
          <strong>{{ log.actionLabel || log.action }}</strong>
          <p>{{ log.detail || [log.dirId, log.path].filter(Boolean).join(' / ') || '无附加信息' }}</p>
          <div class="audit-meta">
            <span>{{ formatDate(log.createdAt || log.time) }}</span>
            <span>{{ log.ip || '未知 IP' }}</span>
            <span>{{ statusText(log) }}</span>
          </div>
        </div>
      </article>
    </div>
    <EmptyState
      v-else-if="displaysCurrentFilter && !loading && !dataStale && (!error || lastSuccessfulAt)"
      :title="hasFilters ? '没有匹配记录' : '暂无访问记录'"
    />
  </section>
</template>
