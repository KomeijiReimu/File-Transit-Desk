<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { TransferRecord } from '@/types'
import { useGsapEntrance } from '@/useGsapEntrance'
import { formatBytes, formatDate, formatDuration, formatSpeed } from '@/utils'

const pageRef = ref<HTMLElement | null>(null)
const loading = ref(true)
const error = ref('')
const transfers = ref<TransferRecord[]>([])
const canceling = ref('')
let refreshTimer: number | undefined

useGsapEntrance(pageRef)

const uploads = computed(() => transfers.value.filter((item) => item.type === 'upload' && item.status !== 'completed'))
const downloads = computed(() => transfers.value.filter((item) => item.type === 'download' && item.status !== 'completed'))

function progressOf(item: TransferRecord) {
  const total = item.totalBytes || 0
  if (!total) return 0
  return Math.min(100, Math.round(((item.transferredBytes || 0) / total) * 100))
}

function progressLabel(item: TransferRecord) {
  if (!item.totalBytes) return item.bestEffort ? '传输状态观测中' : '总量未知'
  return `${formatBytes(item.transferredBytes)} / ${formatBytes(item.totalBytes)} · ${progressOf(item)}%`
}

function fileTitle(item: TransferRecord) {
  if (item.fileName) return item.fileName
  const cleanPath = (item.path || '').split('/').filter(Boolean)
  return cleanPath[cleanPath.length - 1] || item.path || '未命名传输任务'
}

function resourcePath(item: TransferRecord) {
  return item.path || '/'
}

function resourceTitle(item: TransferRecord) {
  return `${item.dirId || '未指定资源'} · ${resourcePath(item)}`
}

function transferKind(item: TransferRecord) {
  return item.type === 'upload' ? '上传' : '下载'
}

function statusLabel(item: TransferRecord) {
  if (item.status === 'canceling') return '正在取消'
  if (item.status === 'completed') return '刚完成'
  if (item.type === 'upload') return '上传中'
  if (item.bestEffort) return '下载通道运行中'
  return '下载中'
}

function observerNote(item: TransferRecord) {
  if (item.totalBytes) return ''
  if (item.bestEffort) return '下载走极速通道，进度可能稍后更新。'
  return '后端暂未返回总量，正在持续观察传输状态。'
}

function speedOf(item: TransferRecord) {
  return formatSpeed(item.currentSpeedBps || item.averageSpeedBps)
}

function sourceOf(item: TransferRecord) {
  return item.clientIP || '未知 IP'
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
  <section ref="pageRef" class="page-stack transfers-page">
    <header class="page-header split">
      <div>
        <p class="eyebrow">Transfers</p>
        <h1>正在传输</h1>
        <p>查看当前上传和下载。上传可以可靠取消；下载保持极速通道，只做运行状态观测。</p>
      </div>
      <button class="ghost-btn" type="button" :disabled="loading" @click="load()">刷新</button>
    </header>

    <StateBlock :loading="loading" :error="error" />

    <div v-if="!loading" class="grid two transfer-summary-grid" data-motion>
      <div class="panel insight-card compact">
        <span class="big-number">{{ uploads.length }}</span>
        <strong>上传中</strong>
      </div>
      <div class="panel insight-card compact">
        <span class="big-number">{{ downloads.length }}</span>
        <strong>下载中</strong>
      </div>
    </div>

    <div v-if="!loading" class="panel table-panel transfer-panel" data-motion>
      <div class="panel-head">
        <div>
          <h2>活跃传输</h2>
          <p class="muted-text">自动观察当前上传与下载；下载通道以速度优先，进度可能延迟更新。</p>
        </div>
        <span class="pill muted">每 2 秒刷新</span>
      </div>
      <div v-if="transfers.length" class="transfer-list">
        <article v-for="item in transfers" :key="item.id" class="transfer-card" :data-kind="item.type === 'upload' ? 'upload' : 'download'">
          <div class="transfer-card-main">
            <header class="transfer-card-head">
              <div class="transfer-title-block">
                <div class="transfer-title-row">
                  <span class="pill" :class="item.type === 'upload' ? 'ok' : 'muted'">{{ transferKind(item) }}</span>
                  <span class="transfer-status">{{ statusLabel(item) }}</span>
                </div>
                <h3 :title="fileTitle(item)">{{ fileTitle(item) }}</h3>
              </div>
              <button v-if="item.cancelable" class="mini-btn danger transfer-cancel" type="button" :disabled="Boolean(canceling)" @click="cancelTransfer(item)">
                {{ canceling === item.id ? '取消中…' : '取消传输' }}
              </button>
              <span v-else class="transfer-passive-status">仅观测</span>
            </header>

            <div class="transfer-path" :title="resourceTitle(item)">
              <span>{{ item.dirId || '未指定资源' }}</span>
              <strong>{{ resourcePath(item) }}</strong>
            </div>

            <div class="transfer-progress-zone">
              <div class="transfer-progress-head">
                <strong>{{ item.totalBytes ? `${progressOf(item)}%` : '观测中' }}</strong>
                <small>{{ progressLabel(item) }}</small>
              </div>
              <div
                v-if="item.totalBytes"
                class="upload-progress transfer-progress"
                role="progressbar"
                :aria-valuenow="progressOf(item)"
                aria-valuemin="0"
                aria-valuemax="100"
                :aria-label="`${fileTitle(item)} 传输进度 ${progressOf(item)}%`"
              >
                <span :style="{ width: `${progressOf(item)}%` }" />
              </div>
              <div v-else class="upload-progress transfer-progress indeterminate" role="progressbar" aria-valuemin="0" aria-valuemax="100" :aria-label="`${fileTitle(item)} 正在观测传输状态`">
                <span />
              </div>
              <small v-if="observerNote(item)" class="transfer-note">{{ observerNote(item) }}</small>
            </div>
          </div>

          <div class="transfer-metrics" aria-label="传输指标">
            <div>
              <span>速度</span>
              <strong>{{ speedOf(item) }}</strong>
            </div>
            <div>
              <span>已用</span>
              <strong>{{ elapsedOf(item) }}</strong>
            </div>
            <div>
              <span>来源 IP</span>
              <strong>{{ sourceOf(item) }}</strong>
              <small>{{ item.source || '—' }}</small>
            </div>
            <div>
              <span>开始时间</span>
              <strong>{{ formatDate(item.startedAt) }}</strong>
            </div>
          </div>
        </article>
      </div>
      <EmptyState v-else title="没有活跃传输" description="当前没有正在上传或下载的任务。" />
    </div>
  </section>
</template>
