<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { CreateTokenResponse, DirectoryInfo, TokenInfo } from '@/types'
import { buildShareUrl, copyToClipboard, extractShareToken, formatDate } from '@/utils'

const tokens = ref<TokenInfo[]>([])
const dirs = ref<DirectoryInfo[]>([])
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const createdLink = ref('')
const createdToken = ref('')
const copyState = ref<'idle' | 'ok' | 'err'>('idle')
const rowCopyId = ref<string | number | null>(null)

const form = reactive({ type: 'download' as 'download' | 'upload', dirId: '', path: '', ttlMinutes: 60, maxUses: 1 })
const canSubmit = computed(() => form.dirId && form.ttlMinutes > 0 && form.maxUses > 0)

function tokenType(token: TokenInfo) {
  return token.type || token.kind || 'download'
}

function shareUrlFor(token: TokenInfo | CreateTokenResponse): string {
  const raw = token.url || token.link || token.token || ''
  const t = extractShareToken(raw)
  return t ? buildShareUrl(t) : ''
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [dirList, tokenList] = await Promise.all([api.dirs(), api.tokens()])
    dirs.value = dirList
    tokens.value = tokenList
    form.dirId ||= dirList[0]?.id || ''
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '令牌数据加载失败。'
  } finally {
    loading.value = false
  }
}

async function createToken() {
  error.value = ''
  createdLink.value = ''
  createdToken.value = ''
  copyState.value = 'idle'
  if (!canSubmit.value) {
    error.value = '请填写目录、有效期和最大使用次数。'
    return
  }
  saving.value = true
  try {
    const token = await api.createToken({ ...form })
    createdToken.value = token.token || extractShareToken(token.url || token.link || '')
    createdLink.value = shareUrlFor(token)
    await load()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '创建令牌失败。'
  } finally {
    saving.value = false
  }
}

async function copyCreated() {
  if (!createdLink.value) return
  const ok = await copyToClipboard(createdLink.value)
  copyState.value = ok ? 'ok' : 'err'
  window.setTimeout(() => { copyState.value = 'idle' }, 2200)
}

async function copyRow(token: TokenInfo) {
  const url = shareUrlFor(token)
  if (!url) return
  const ok = await copyToClipboard(url)
  if (ok) {
    rowCopyId.value = token.id
    window.setTimeout(() => {
      if (rowCopyId.value === token.id) rowCopyId.value = null
    }, 1800)
  }
}

async function revoke(id: string | number) {
  error.value = ''
  try {
    await api.revokeToken(id)
    await load()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '撤销令牌失败。'
  }
}

async function remove(id: string | number) {
  if (!confirm('确定删除这个令牌吗？')) return
  error.value = ''
  try {
    await api.deleteToken(id)
    tokens.value = tokens.value.filter((token) => token.id !== id)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '删除令牌失败。'
  }
}

onMounted(load)
</script>

<template>
  <section class="page-stack">
    <header class="page-header">
      <p class="eyebrow">Tokens</p>
      <h1>令牌管理</h1>
      <p>创建临时下载或上传链接，并控制目录、路径、有效期与使用次数。</p>
    </header>

    <StateBlock :loading="loading" :error="error" />

    <div class="grid two">
      <form class="panel form-grid" @submit.prevent="createToken">
        <h2>创建一次性链接</h2>
        <label>类型
          <div class="select-wrap">
            <select v-model="form.type" class="select">
              <option value="download">下载令牌</option>
              <option value="upload">上传令牌</option>
            </select>
          </div>
        </label>
        <label>目录
          <div class="select-wrap">
            <select v-model="form.dirId" class="select">
              <option v-for="dir in dirs" :key="dir.id" :value="dir.id">{{ dir.label || dir.name }}</option>
            </select>
          </div>
        </label>
        <label>路径
          <input v-model.trim="form.path" placeholder="空为目录根路径，或填写 sub/file.zip" />
        </label>
        <div class="inline-fields">
          <label>有效期（分钟）<input v-model.number="form.ttlMinutes" min="1" type="number" /></label>
          <label>最大次数<input v-model.number="form.maxUses" min="1" type="number" /></label>
        </div>
        <button class="primary-btn" type="submit" :disabled="saving || !canSubmit">{{ saving ? '创建中…' : '生成链接' }}</button>

        <div v-if="createdLink" class="created-link">
          <span class="muted-text">只显示一次，请立即复制：</span>
          <code class="link-code">{{ createdLink }}</code>
          <div class="created-actions">
            <button class="primary-btn" type="button" @click="copyCreated">
              {{ copyState === 'ok' ? '✓ 已复制' : copyState === 'err' ? '复制失败' : '复制链接' }}
            </button>
            <a class="ghost-btn" :href="createdLink" target="_blank" rel="noopener">在新窗口打开</a>
          </div>
        </div>
      </form>

      <div class="panel insight-card">
        <span class="big-number">{{ tokens.length }}</span>
        <strong>当前令牌</strong>
        <p>建议给外部人员使用短有效期、低次数链接；完成传输后可立即撤销。</p>
      </div>
    </div>

    <div class="table-card">
      <table v-if="tokens.length" class="data-table">
        <thead><tr><th>类型</th><th>目录 / 路径</th><th>使用</th><th>到期</th><th>状态</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="token in tokens" :key="token.id">
            <td data-label="类型"><span class="pill">{{ tokenType(token) === 'upload' ? '上传' : '下载' }}</span></td>
            <td data-label="目录 / 路径">{{ token.dirName || token.dirId || token.dir || '—' }}<br /><small>{{ token.path || '/' }}</small></td>
            <td data-label="使用">{{ token.uses ?? token.used ?? 0 }} / {{ token.maxUses ?? '不限' }}</td>
            <td data-label="到期">{{ formatDate(token.expiresAt) }}</td>
            <td data-label="状态">
              <span class="pill" :class="token.revoked ? 'danger' : (token.valid === false ? 'muted' : 'ok')">
                {{ token.revoked ? '已撤销' : token.valid === false ? (token.reason === 'expired' ? '已过期' : token.reason === 'exhausted' ? '已用尽' : '不可用') : '可用' }}
              </span>
            </td>
            <td data-label="操作" class="actions">
              <button class="mini-btn" type="button" @click="copyRow(token)">
                {{ rowCopyId === token.id ? '✓ 已复制' : '复制链接' }}
              </button>
              <button class="mini-btn" :disabled="token.revoked" @click="revoke(token.id)">撤销</button>
              <button class="mini-btn danger" @click="remove(token.id)">删除</button>
            </td>
          </tr>
        </tbody>
      </table>
      <EmptyState v-else-if="!loading" title="还没有令牌" description="创建一个短期链接后，它会显示在这里。" />
    </div>
  </section>
</template>
