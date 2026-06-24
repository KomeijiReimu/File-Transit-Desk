<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { TransferRecord } from '@/types'
import { formatBytes, formatDate, formatDuration, formatSpeed } from '@/utils'

const loading = ref(true)
const error = ref('')
const transfers = ref<TransferRecord[]>([])
const canceling = ref('')
let refreshTimer: number | undefined

const uploads = computed(() => transfers.value.filter((item) => item.type === 'upload' && item.status !== 'completed'))
const downloads = computed(() => transfers.value.filter((item) => item.type === 'download' && item.status !== 'completed'))

function progressOf(item: TransferRecord) {
  const total = item.totalBytes || 0
  if (!total) return 0
  return Math.min(100, Math.round(((item.transferredBytes || 0) / total) * 100))
}

function progressLabel(item: TransferRecord) {
  if (!item.totalBytes) return item.bestEffort ? '观测中' : '总量未知'
  return `${formatBytes(item.transferredBytes)} / ${formatBytes(item.totalBytes)} · ${progressOf(item)}%`
}

function typeLabel(item: TransferRecord) {
  const suffix = item.status === 'completed' ? ' · 刚完成' : ''
  if (item.type === 'upload') return `上传${suffix}`
  return `${item.bestEffort ? '下载 · 极速路径' : '下载'}${suffix}`
}

function elapsedOf(item: TransferRecord) {
  if (!item.startedAt) return '—'
  return formatDuration((Date.now() - new Date(item.startedAt).getTime()) / 1000)
}

async function load(silent = false) {
  if (!silent) loading.value = true
  error.value = ''
  try {
    const result = await api.activeTransfers()
    transfers.value = result.transfers || []
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '传输状态加载失败。'
  } finally {
    loading.value = false
  }
}

async function cancelTransfer(item: TransferRecord) {
  if (!item.cancelable || canceling.value) return
  canceling.value = item.id
  error.value = ''
  try {
    await api.cancelTransfer(item.id)
    await load(true)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '取消传输失败。'
  } finally {
    canceling.value = ''
  }
}

onMounted(() => {
  load()
  refreshTimer = window.setInterval(() => load(true), 2000)
})

onUnmounted(() => {
  if (refreshTimer) window.clearInterval(refreshTimer)
})
</script>

<template>
  <section class="page-stack transfers-page">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Transfers</p>
        <h1>正在传输</h1>
        <p>查看当前上传和下载。上传可以可靠取消；下载保持极速通道，只做运行状态观测。</p>
      </div>
      <button class="ghost-btn" type="button" :disabled="loading" @click="load()">刷新</button>
    </header>

    <StateBlock :loading="loading" :error="error" />

    <div v-if="!loading" class="grid two transfer-summary-grid">
      <div class="panel insight-card compact">
        <span class="big-number">{{ uploads.length }}</span>
        <strong>上传中</strong>
      </div>
      <div class="panel insight-card compact">
        <span class="big-number">{{ downloads.length }}</span>
        <strong>下载中</strong>
      </div>
    </div>

    <div v-if="!loading" class="panel table-panel">
      <div class="panel-head">
        <h2>活跃传输</h2>
        <p class="muted-text">速度为后端观测值；下载因走极速发送路径，可能只显示最佳努力状态。</p>
      </div>
      <table v-if="transfers.length" class="data-table transfer-table">
        <thead>
          <tr><th>类型</th><th>资源 / 文件</th><th>来源</th><th>进度</th><th>速度</th><th>开始</th><th>操作</th></tr>
        </thead>
        <tbody>
          <tr v-for="item in transfers" :key="item.id">
            <td data-label="类型"><span class="pill" :class="item.type === 'upload' ? 'ok' : 'muted'">{{ typeLabel(item) }}</span></td>
            <td data-label="资源 / 文件">
              <strong>{{ item.fileName || item.path || '—' }}</strong><br />
              <small>{{ item.dirId || '—' }} · {{ item.path || '/' }}</small>
            </td>
            <td data-label="来源">{{ item.clientIP || '—' }}<br /><small>{{ item.source || '—' }}</small></td>
            <td data-label="进度">
              <div v-if="item.totalBytes" class="upload-progress"><span :style="{ width: `${progressOf(item)}%` }" /></div>
              <small>{{ progressLabel(item) }}</small>
            </td>
            <td data-label="速度">{{ formatSpeed(item.currentSpeedBps || item.averageSpeedBps) }}<br /><small>{{ item.bestEffort ? '最佳努力观测' : `已用 ${elapsedOf(item)}` }}</small></td>
            <td data-label="开始">{{ formatDate(item.startedAt) }}</td>
            <td data-label="操作" class="actions">
              <button class="mini-btn danger" type="button" :disabled="!item.cancelable || canceling === item.id" @click="cancelTransfer(item)">
                {{ item.cancelable ? (canceling === item.id ? '取消中…' : '取消') : '不可取消' }}
              </button>
            </td>
          </tr>
        </tbody>
      </table>
      <EmptyState v-else title="没有活跃传输" description="当前没有正在上传或下载的任务。" />
    </div>
  </section>
</template>
