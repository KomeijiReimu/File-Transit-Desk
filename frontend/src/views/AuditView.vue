<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { AuditLog } from '@/types'
import { formatDate } from '@/utils'

const logs = ref<AuditLog[]>([])
const loading = ref(true)
const error = ref('')
const limit = ref(100)
const keyword = ref('')
const status = ref('all')
const statusOptions = [
  { label: '全部记录', value: 'all', hint: '显示所有审计事件' },
  { label: '正常行为', value: 'ok', hint: '登录、浏览、上传、下载' },
  { label: '失败 / 拒绝', value: 'failed', hint: '失败、限速、无权限' },
]

const filteredLogs = computed(() => {
  const kw = keyword.value.trim().toLowerCase()
  return logs.value.filter((log) => {
    // 后端只存 action/detail，前端把标签、IP 和状态拼成全文，支持轻量本地搜索。
    const text = [log.actionLabel, log.action, log.detail, log.ip, log.status].filter(Boolean).join(' ').toLowerCase()
    const matchKeyword = !kw || text.includes(kw)
    const failed = /failed|denied|forbidden|unauthorized|illegal|失败|拒绝|非法|未认证/.test(text)
    const matchStatus = status.value === 'all' || (status.value === 'failed' ? failed : !failed)
    return matchKeyword && matchStatus
  })
})

async function load(nextLimit = limit.value) {
  loading.value = true
  error.value = ''
  try {
    logs.value = await api.auditLogs({ limit: nextLimit })
    limit.value = nextLimit
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '访问记录加载失败。'
  } finally {
    loading.value = false
  }
}

function loadMore() {
  // 后端最多返回 500 条，前端逐步增大 limit，避免首次进入加载过多历史记录。
  load(Math.min(limit.value + 100, 500))
}

onMounted(() => load())

function logTone(log: AuditLog) {
  // 根据动作关键词给时间线着色；未知动作默认按正常事件展示。
  const text = [log.action, log.actionLabel, log.status, log.detail].filter(Boolean).join(' ').toLowerCase()
  if (/rate_limited|failed|denied|forbidden|unauthorized|illegal|失败|拒绝|非法|未认证|限速/.test(text)) return 'failed'
  if (/token|令牌/.test(text)) return 'token'
  return 'ok'
}
</script>

<template>
  <section class="page-stack audit-page">
    <header class="page-header split">
      <div><p class="eyebrow">Audit</p><h1>访问记录</h1><p>查看最近登录、下载、上传、令牌访问等行为。</p></div>
      <button class="ghost-btn" @click="load()">刷新</button>
    </header>

    <div class="panel filter-bar">
      <label>筛选关键字<input v-model.trim="keyword" placeholder="例如 登录、上传、某个 IP" /></label>
      <label>状态
        <GlassSelect v-model="status" :options="statusOptions" aria-label="筛选访问记录状态" />
      </label>
      <button class="ghost-btn" type="button" @click="keyword = ''; status = 'all'">清空筛选</button>
    </div>

    <StateBlock :loading="loading" :error="error" />
    <div class="timeline" v-if="filteredLogs.length">
      <article v-for="(log, index) in filteredLogs" :key="log.id || index" class="timeline-item" :data-status="logTone(log)">
        <span class="timeline-dot" />
        <div>
          <strong>{{ log.actionLabel || log.action }}</strong>
          <p>{{ log.detail || [log.dirId, log.path].filter(Boolean).join(' / ') || '无附加信息' }}</p>
          <small>{{ formatDate(log.createdAt || log.time) }} · {{ log.ip || '未知 IP' }} · {{ log.status || '完成' }}</small>
        </div>
      </article>
      <button v-if="limit < 500" class="ghost-btn full" type="button" :disabled="loading" @click="loadMore">加载更多记录</button>
    </div>
    <EmptyState v-else-if="!loading" title="暂无访问记录" description="调整筛选条件，或等待后端产生审计事件后再查看。" />
  </section>
</template>
