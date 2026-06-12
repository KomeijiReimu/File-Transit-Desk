<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { ApiError, api } from '@/api'
import ConfirmDialog from '@/components/ConfirmDialog.vue'
import EmptyState from '@/components/EmptyState.vue'
import GlassSelect from '@/components/GlassSelect.vue'
import StateBlock from '@/components/StateBlock.vue'
import type { CreateTokenResponse, DirectoryInfo, ShareOriginCandidate, TokenInfo } from '@/types'
import { buildSharePath, buildShareUrl, configuredPublicShareOrigin, copyToClipboard, extractShareToken, formatDate } from '@/utils'

const tokens = ref<TokenInfo[]>([])
const dirs = ref<DirectoryInfo[]>([])
const shareOrigins = ref<ShareOriginCandidate[]>([])
const route = useRoute()
const loading = ref(true)
const saving = ref(false)
const error = ref('')
const createdLink = ref('')
const createdPath = ref('')
const createdToken = ref('')
const copyState = ref('idle')
const rowCopyId = ref<string | number | null>(null)
const pendingDelete = ref<TokenInfo | null>(null)
const deleteError = ref('')
const deleting = ref(false)
let appliedInitialQuery = false

const form = reactive({ type: 'download' as 'download' | 'upload', dirId: '', path: '', ttlMinutes: 60, maxUses: 1 })
const canSubmit = computed(() => form.dirId && form.ttlMinutes > 0 && form.maxUses > 0)
const typeOptions = [
  { label: '下载令牌', value: 'download', hint: '外部访客领取文件' },
  { label: '上传令牌', value: 'upload', hint: '外部访客提交文件' },
]
const selectedDir = computed(() => dirs.value.find((dir) => dir.id === form.dirId))
const selectedDirIsFile = computed(() => selectedDir.value?.type === 'file')
const pathPlaceholder = computed(() => selectedDirIsFile.value ? '单文件资源无需填写路径' : form.type === 'download' ? '必填，填写已存在文件路径，例如 sub/file.zip' : '可选，填写接收目录，例如 inbox/，留空为根目录')
const pathHelp = computed(() => selectedDirIsFile.value ? '该资源已经绑定到一个具体文件，创建下载令牌时无需再填写相对路径。' : form.type === 'download' ? '下载令牌必须指向已存在的具体文件。' : '上传令牌会把文件保存到此目录，留空则保存到目录根路径。')
const usableDirs = computed(() => dirs.value.filter((dir) => form.type === 'download' ? dir.canDownload !== false && dir.allowDownload !== false : dir.type !== 'file' && Boolean(dir.canUpload || dir.allowUpload)))
const dirOptions = computed(() => usableDirs.value.map((dir) => ({
  label: dir.label || dir.name,
  value: dir.id,
  hint: `${dir.type === 'file' ? '单文件' : '目录'} · ${dir.description || dir.root || dir.id}`,
})))

function tokenType(token: TokenInfo) {
  return token.type || token.kind || 'download'
}

function shareUrlFor(token: TokenInfo | CreateTokenResponse): string {
  // 后端只在创建响应返回一次明文 token；列表行若没有 url/token，就不再尝试拼出分享链接。
  const raw = token.url || token.link || token.token || ''
  const t = extractShareToken(raw)
  return t ? buildShareUrl(t) : ''
}

function sharePathFor(token: TokenInfo | CreateTokenResponse): string {
  const raw = token.url || token.link || token.token || ''
  const t = extractShareToken(raw)
  return t ? buildSharePath(t) : ''
}

const shareOriginOptions = computed(() => {
  const options: ShareOriginCandidate[] = []
  const seen = new Set<string>()
  const add = (item: ShareOriginCandidate) => {
    const origin = item.origin.replace(/\/+$/, '')
    if (!origin || seen.has(origin)) return
    seen.add(origin)
    options.push({ ...item, origin })
  }
  if (typeof window !== 'undefined') {
    add({ origin: window.location.origin, label: '当前访问地址', source: 'current' })
  }
  const configured = configuredPublicShareOrigin()
  if (configured) add({ origin: configured, label: '公开地址', source: 'configured' })
  shareOrigins.value.forEach(add)
  return options
})

const shareLinks = computed(() => shareOriginOptions.value.map((item) => ({
  ...item,
  url: createdToken.value ? buildShareUrl(createdToken.value, item.origin) : '',
})))

function tokenStatusLabel(token: TokenInfo) {
  if (token.revoked) return '已撤销'
  if (token.valid === false) {
    if (token.reason === 'expired') return '已过期'
    if (token.reason === 'exhausted') return '已用尽'
    if (token.reason === 'upload_quota_exhausted') return '容量已满'
    if (token.reason === 'resource_unavailable') return '资源已移除'
    if (token.reason === 'permission_disabled') return '权限已关闭'
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

const deleteDialogTitle = computed(() => pendingDelete.value && canRevoke(pendingDelete.value) ? '删除并立即失效？' : '删除这条令牌记录？')
// 弹窗文案根据当前令牌是否仍可用动态切换，避免“删除记录”和“失效链接”混为一谈。
const deleteDialogMessage = computed(() => pendingDelete.value && canRevoke(pendingDelete.value)
  ? '这个令牌当前仍可用。删除后会立即失效，并清理它已经兑换但尚未过期的下载票据。'
  : '这条令牌已经不可用。删除后只会从管理列表中移除历史记录。')
const deleteDialogDetail = computed(() => pendingDelete.value ? `${tokenType(pendingDelete.value) === 'upload' ? '上传' : '下载'} · ${pendingDelete.value.dirName || pendingDelete.value.dirId || pendingDelete.value.dir || '未知目录'} · ${pendingDelete.value.path || '/'}` : '')

function applyRouteQuery(force = false) {
  if (!force && appliedInitialQuery) return
  const queryType = route.query.type === 'upload' ? 'upload' : route.query.type === 'download' ? 'download' : ''
  if (queryType) form.type = queryType
  const queryDir = String(route.query.dirId || '')
  if (queryDir && usableDirs.value.some((dir) => dir.id === queryDir)) form.dirId = queryDir
  else if (!form.dirId || !usableDirs.value.some((dir) => dir.id === form.dirId)) form.dirId = usableDirs.value[0]?.id || ''
  if ('path' in route.query) form.path = String(route.query.path || '')
  if (selectedDirIsFile.value) form.path = ''
  appliedInitialQuery = true
}

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [dirList, tokenList, originList] = await Promise.all([
      api.dirs(),
      api.tokens(),
      api.shareOrigins(typeof window !== 'undefined' ? window.location.origin : '').catch(() => [] as ShareOriginCandidate[]),
    ])
    dirs.value = dirList
    tokens.value = tokenList
    shareOrigins.value = originList
    applyRouteQuery(false)
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '令牌数据加载失败。'
  } finally {
    loading.value = false
  }
}

async function createToken() {
  error.value = ''
  createdLink.value = ''
  createdPath.value = ''
  createdToken.value = ''
  copyState.value = 'idle'
  if (!canSubmit.value) {
    error.value = '请填写目录、有效期和最大使用次数。'
    return
  }
  if (form.type === 'download' && !form.path && !selectedDirIsFile.value) {
    // 下载令牌必须绑定具体文件；上传令牌才允许留空表示目录根路径。
    error.value = '下载令牌需要填写具体文件路径，例如 sub/file.zip。'
    return
  }
  saving.value = true
  try {
    const token = await api.createToken({ ...form, path: selectedDirIsFile.value ? '' : form.path })
    createdToken.value = token.token || extractShareToken(token.url || token.link || '')
    createdLink.value = shareUrlFor(token)
    createdPath.value = sharePathFor(token)
    await load()
  } catch (err) {
    error.value = err instanceof ApiError ? err.message : '创建令牌失败。'
  } finally {
    saving.value = false
  }
}

async function copyCreated(value: string, okState: string) {
  if (!value) return
  const ok = await copyToClipboard(value)
  copyState.value = ok ? okState : 'err'
  window.setTimeout(() => { copyState.value = 'idle' }, 2200)
}

watch(() => form.type, () => {
  if (!usableDirs.value.some((dir) => dir.id === form.dirId)) form.dirId = usableDirs.value[0]?.id || ''
  if (selectedDirIsFile.value) form.path = ''
})
watch(() => route.fullPath, () => applyRouteQuery(true))

async function copyRow(token: TokenInfo) {
  const url = shareUrlFor(token)
  if (!url) return
  const ok = await copyToClipboard(url)
  if (ok) {
    // 每行复制状态只短暂显示，避免刷新列表或切换页面后残留“已复制”。
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

function requestRemove(token: TokenInfo) {
  pendingDelete.value = token
  deleteError.value = ''
}

async function confirmRemove() {
  const token = pendingDelete.value
  if (!token || deleting.value) return
  error.value = ''
  deleteError.value = ''
  deleting.value = true
  try {
    // 删除失败时错误显示在弹窗内部，避免被遮罩后的页面级错误提示挡住。
    await api.deleteToken(token.id)
    tokens.value = tokens.value.filter((item) => item.id !== token.id)
    pendingDelete.value = null
  } catch (err) {
    deleteError.value = err instanceof ApiError ? err.message : '删除令牌失败。'
  } finally {
    deleting.value = false
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
          <input v-model.trim="form.path" :disabled="selectedDirIsFile" :placeholder="pathPlaceholder" />
          <small>{{ pathHelp }}</small>
        </label>
        <div class="inline-fields">
          <label>有效期（分钟）<input v-model.number="form.ttlMinutes" min="1" type="number" /></label>
          <label>最大次数<input v-model.number="form.maxUses" min="1" type="number" /></label>
        </div>
        <button class="primary-btn" type="submit" :disabled="saving || !canSubmit">{{ saving ? '创建中…' : '生成链接' }}</button>

        <div v-if="createdLink" class="created-link">
          <span class="muted-text">只显示一次，请立即复制：</span>
          <div class="share-link-list">
            <div>
              <span>分享路径</span>
              <code class="link-code">{{ createdPath }}</code>
            </div>
            <div v-for="candidate in shareLinks" :key="candidate.origin">
              <span>{{ candidate.label }}</span>
              <code class="link-code">{{ candidate.url }}</code>
              <button class="mini-btn" type="button" @click="copyCreated(candidate.url, `origin:${candidate.origin}`)">
                {{ copyState === `origin:${candidate.origin}` ? '✓ 已复制' : '复制这个地址' }}
              </button>
            </div>
            <div>
              <span>令牌</span>
              <code class="link-code">{{ createdToken }}</code>
            </div>
          </div>
          <div class="created-actions">
            <button class="primary-btn" type="button" @click="copyCreated(createdPath, 'path-ok')">
              {{ copyState === 'path-ok' ? '✓ 已复制' : copyState === 'err' ? '复制失败' : '复制分享路径' }}
            </button>
            <button class="ghost-btn" type="button" @click="copyCreated(createdToken, 'token-ok')">
              {{ copyState === 'token-ok' ? '✓ 已复制' : '复制令牌' }}
            </button>
            <a class="ghost-btn" :href="createdLink" target="_blank" rel="noopener">在新窗口打开</a>
          </div>
        </div>
      </form>

      <div class="panel insight-card">
        <span class="big-number">{{ tokens.length }}</span>
        <strong>当前令牌</strong>
        <p>撤销用于立刻让链接失效并清理已兑换下载票据；删除对已失效令牌只移除记录，对仍可用令牌会一并失效。</p>
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
                <button class="mini-btn danger" :title="canRevoke(token) ? '删除记录并让仍可用的令牌失效' : '删除历史记录'" @click="requestRemove(token)">{{ deleteLabel(token) }}</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      <EmptyState v-else-if="!loading" title="还没有令牌" description="创建一个短期链接后，它会显示在这里。" />
    </div>

    <ConfirmDialog
      :open="Boolean(pendingDelete)"
      :title="deleteDialogTitle"
      :message="deleteDialogMessage"
      :detail="deleteDialogDetail"
      :error="deleteError"
      :confirm-label="pendingDelete ? deleteLabel(pendingDelete) : '删除'"
      :danger="true"
      :loading="deleting"
      @cancel="pendingDelete = null"
      @confirm="confirmRemove"
    />
  </section>
</template>
