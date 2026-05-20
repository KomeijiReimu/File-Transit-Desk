<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { ApiError, api } from '@/api'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
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
const typeOptions = [
  { label: '下载令牌', value: 'download', hint: '外部访客领取文件' },
  { label: '上传令牌', value: 'upload', hint: '外部访客提交文件' },
]
const pathPlaceholder = computed(() => form.type === 'download' ? '必填，填写已存在文件路径，例如 sub/file.zip' : '可选，填写接收目录，例如 inbox/，留空为根目录')
const pathHelp = computed(() => form.type === 'download' ? '下载令牌必须指向已存在的具体文件。' : '上传令牌会把文件保存到此目录，留空则保存到目录根路径。')
const dirOptions = computed(() => dirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: dir.description || dir.root || dir.id,
})))

function tokenType(token: TokenInfo) {
  return token.type || token.kind || 'download'
}

function shareUrlFor(token: TokenInfo | CreateTokenResponse): string {
  const raw = token.url || token.link || token.token || ''
  const t = extractShareToken(raw)
  return t ? buildShareUrl(t) : ''
}

function tokenStatusLabel(token: TokenInfo) {
  if (token.revoked) return '已撤销'
  if (token.valid === false) {
    if (token.reason === 'expired') return '已过期'
    if (token.reason === 'exhausted') return '已用尽'
    if (token.reason === 'upload_quota_exhausted') return '容量已满'
    return '不可用'
  }
  return '可用'
}

function canCopyRow(token: TokenInfo) {
  return Boolean(shareUrlFor(token))
}

function canRevoke(token: TokenInfo) {
  return token.revoked !== true && token.valid !== false
}

function deleteLabel(token: TokenInfo) {
  return canRevoke(token) ? '删除并失效' : '删除记录'
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
  if (form.type === 'download' && !form.path) {
    error.value = '下载令牌需要填写具体文件路径，例如 sub/file.zip。'
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

async function remove(token: TokenInfo) {
  if (!confirm(canRevoke(token) ? '确定删除并立即让这个令牌失效吗？此操作会移除历史记录并清理相关下载票据。' : '确定删除这条令牌记录吗？')) return
  error.value = ''
  try {
    await api.deleteToken(token.id)
    tokens.value = tokens.value.filter((item) => item.id !== token.id)
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
        <p class="muted-text">下载令牌的路径必须是已存在的具体文件；上传令牌的路径表示接收目录。</p>
        <label>类型
          <GlassSelect v-model="form.type" :options="typeOptions" aria-label="选择令牌类型" />
        </label>
        <label>目录
          <GlassSelect v-model="form.dirId" :options="dirOptions" aria-label="选择目录" placeholder="选择目录" />
        </label>
        <label>路径
          <input v-model.trim="form.path" :placeholder="pathPlaceholder" />
          <small>{{ pathHelp }}</small>
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
        <p>撤销用于立刻让链接失效并清理已兑换下载票据；删除用于移除历史记录。两者不是重复操作。</p>
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
                {{ tokenStatusLabel(token) }}
              </span>
            </td>
            <td data-label="操作" class="actions">
              <div class="row-actions">
                <button class="mini-btn" type="button" :disabled="!canCopyRow(token)" :title="canCopyRow(token) ? '复制分享链接' : '明文链接只在创建时显示一次'" @click="copyRow(token)">
                  {{ rowCopyId === token.id ? '✓ 已复制' : canCopyRow(token) ? '复制链接' : '仅创建时可复制' }}
                </button>
                <button class="mini-btn" :disabled="!canRevoke(token)" :title="canRevoke(token) ? '立即让链接失效' : '该令牌已经不可用，无需再次撤销'" @click="revoke(token.id)">撤销</button>
                <button class="mini-btn danger" :title="canRevoke(token) ? '删除记录并让仍可用的令牌失效' : '删除历史记录'" @click="remove(token)">{{ deleteLabel(token) }}</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      <EmptyState v-else-if="!loading" title="还没有令牌" description="创建一个短期链接后，它会显示在这里。" />
    </div>
  </section>
</template>
