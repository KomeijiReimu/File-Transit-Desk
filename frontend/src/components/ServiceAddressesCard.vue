<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '@/api'
import AppIcon from '@/components/AppIcon.vue'
import type { ShareListenDiagnostic, ShareOriginCandidate } from '@/types'
import { configuredPublicShareOrigin, copyToClipboard } from '@/utils'

type AddressGroupKey = 'current' | 'configured' | 'interface' | 'other'
type CopyState = 'idle' | 'copied' | 'error'
type ListenMatchStatus = 'match' | 'mismatch' | 'unknown'
type CandidateTag = {
  label: string
  tone?: 'positive' | 'notice' | 'neutral'
  matchStatus?: ListenMatchStatus
}
type DisplayCandidate = ShareOriginCandidate & {
  origin: string
  sources: string[]
  group: AddressGroupKey
}

const groupDefinitions: Array<{ key: AddressGroupKey; title: string; description: string }> = [
  { key: 'current', title: '当前入口', description: '此浏览器正在使用' },
  { key: 'configured', title: '公开地址', description: '显式配置的对外访问地址' },
  { key: 'interface', title: '网络接口', description: '与监听范围匹配的本机地址' },
  { key: 'other', title: '其他候选', description: '由服务端补充提供' },
]

const candidates = ref<ShareOriginCandidate[]>([])
const loading = ref(true)
const loadError = ref(false)
const copyStates = ref<Record<string, CopyState>>({})
const copyAnnouncement = ref('')
const resetTimers = new Map<string, number>()

function normalizeOrigin(value: string) {
  try {
    const input = value.trim()
    if (!input || /[\\?#]/.test(input)) return ''
    const protocolEnd = input.indexOf('://')
    if (protocolEnd < 1) return ''
    const pathStart = input.indexOf('/', protocolEnd + 3)
    const authority = pathStart === -1 ? input.slice(protocolEnd + 3) : input.slice(protocolEnd + 3, pathStart)
    if (!authority || authority.endsWith(':')) return ''
    const url = new URL(input)
    if ((url.protocol !== 'http:' && url.protocol !== 'https:') || url.username || url.password || url.pathname !== '/' || url.search || url.hash || url.origin === 'null') return ''
    return url.origin
  } catch {
    return ''
  }
}

const currentOrigin = computed(() => typeof window === 'undefined' ? '' : normalizeOrigin(window.location.origin))
const configuredOrigin = computed(() => normalizeOrigin(configuredPublicShareOrigin()))

function unique(values: string[]) {
  return [...new Set(values.map((value) => value.trim()).filter(Boolean))]
}

function sourceRank(source: string) {
  if (source === 'current') return 0
  if (source === 'configured') return 1
  if (source === 'interface') return 2
  return 3
}

function mergeCandidate(previous: ShareOriginCandidate | undefined, incoming: ShareOriginCandidate, origin: string): ShareOriginCandidate {
  if (!previous) {
    return {
      ...incoming,
      origin,
      sources: unique([incoming.source, ...(incoming.sources || [])]),
      interfaces: unique([...(incoming.interfaces || []), incoming.interface || '']),
    }
  }

  const primarySource = sourceRank(previous.source) <= sourceRank(incoming.source) ? previous.source : incoming.source
  const fallbackLabel = incoming.label?.trim() || previous.label?.trim() || '候选地址'
  return {
    ...previous,
    ...incoming,
    origin,
    source: primarySource,
    label: primarySource === 'current' ? '当前访问地址' : primarySource === 'configured' ? '公开地址' : fallbackLabel,
    sources: unique([previous.source, ...(previous.sources || []), incoming.source, ...(incoming.sources || [])]),
    interfaces: unique([...(previous.interfaces || []), previous.interface || '', ...(incoming.interfaces || []), incoming.interface || '']),
  }
}

function groupFor(candidate: ShareOriginCandidate, origin: string): AddressGroupKey {
  const sources = unique([candidate.source, ...(candidate.sources || [])])
  if (origin === currentOrigin.value || sources.includes('current')) return 'current'
  if (candidate.source === 'configured') return 'configured'
  if (candidate.source === 'interface') return 'interface'
  if (sources.includes('configured') && !sources.includes('interface')) return 'configured'
  if (sources.includes('interface')) return 'interface'
  return 'other'
}

const displayCandidates = computed<DisplayCandidate[]>(() => {
  const byOrigin = new Map<string, ShareOriginCandidate>()
  const add = (candidate: ShareOriginCandidate) => {
    const origin = normalizeOrigin(candidate.origin)
    if (!origin) return
    byOrigin.set(origin, mergeCandidate(byOrigin.get(origin), candidate, origin))
  }

  for (const candidate of candidates.value) {
    add(candidate)
  }

  if (configuredOrigin.value) {
    add({
      origin: configuredOrigin.value,
      label: '公开地址',
      source: 'configured',
      sources: ['configured'],
      reachable: 'unknown',
    })
  }
  if (currentOrigin.value) {
    add({
      origin: currentOrigin.value,
      label: '当前访问地址',
      source: 'current',
      sources: ['current'],
      reachable: 'unknown',
    })
  }

  const rank: Record<AddressGroupKey, number> = { current: 0, configured: 1, interface: 2, other: 3 }
  return [...byOrigin.values()]
    .map((candidate) => {
      const origin = normalizeOrigin(candidate.origin)
      return {
        ...candidate,
        origin,
        sources: unique([candidate.source, ...(candidate.sources || [])]),
        group: groupFor(candidate, origin),
      }
    })
    .filter((candidate) => Boolean(candidate.origin))
    .sort((left, right) => rank[left.group] - rank[right.group] || (left.origin < right.origin ? -1 : left.origin > right.origin ? 1 : 0))
})

const candidateGroups = computed(() => groupDefinitions
  .map((group) => ({ ...group, items: displayCandidates.value.filter((candidate) => candidate.group === group.key) }))
  .filter((group) => group.items.length))

const hasAdditionalCandidates = computed(() => displayCandidates.value.some((candidate) => candidate.group !== 'current'))
const hasConfiguredCandidate = computed(() => displayCandidates.value.some((candidate) => candidate.sources.includes('configured')))
const showEmptyNotice = computed(() => !loading.value && !loadError.value && !hasAdditionalCandidates.value)
const listenDiagnostics = computed<ShareListenDiagnostic[]>(() => {
  const diagnostics: ShareListenDiagnostic[] = []
  const seen = new Set<string>()
  for (const candidate of candidates.value) {
    const listen = candidate.listen
    if (!listen || listen.source !== 'listen' || !listen.address) continue
    const key = [listen.network, listen.family, listen.mode, listen.host, String(listen.port), listen.address, listen.reachable].join('\u0000')
    if (seen.has(key)) continue
    seen.add(key)
    diagnostics.push(listen)
  }
  return diagnostics
})

function hostFor(candidate: DisplayCandidate) {
  try {
    return new URL(candidate.origin).hostname.replace(/^\[|\]$/g, '')
  } catch {
    return ''
  }
}

function addressFamily(candidate: DisplayCandidate) {
  const host = hostFor(candidate)
  if (host.includes(':')) return 'IPv6'
  if (/^(?:\d{1,3}\.){3}\d{1,3}$/.test(host)) return 'IPv4'
  return '域名'
}

function scopeLabel(scope?: string) {
  if (scope === 'loopback') return '仅本机'
  if (scope === 'private') return '私网'
  if (scope === 'link-local') return '链路本地'
  if (scope === 'carrier-grade-nat') return '运营商级 NAT（CGNAT）'
  if (scope === 'global-unicast' || scope === 'global') return '全局单播/其他网络'
  if (scope === 'other') return '其他网络'
  return ''
}

function sourceLabel(group: AddressGroupKey) {
  if (group === 'current') return '当前入口'
  if (group === 'configured') return '公开配置'
  if (group === 'interface') return '本机网卡'
  return '候选地址'
}

function candidateMatchStatus(candidate: DisplayCandidate): ListenMatchStatus | '' {
  if (candidate.listenMatchStatus === 'match' || candidate.listenMatchStatus === 'mismatch' || candidate.listenMatchStatus === 'unknown') {
    return candidate.listenMatchStatus
  }
  if (candidate.listen && typeof candidate.listenMatch === 'boolean') return candidate.listenMatch ? 'match' : 'mismatch'
  return ''
}

function candidateTags(candidate: DisplayCandidate): CandidateTag[] {
  const sourceTags = [sourceLabel(candidate.group)]
  if (candidate.group !== 'configured' && candidate.sources.includes('configured')) sourceTags.push('公开配置')
  if (candidate.group !== 'interface' && candidate.sources.includes('interface')) sourceTags.push('本机网卡')
  const tags: CandidateTag[] = unique([...sourceTags, addressFamily(candidate), scopeLabel(candidate.scope)])
    .map((label) => ({ label }))
  const status = candidateMatchStatus(candidate)
  if (status === 'match') tags.push({ label: '监听匹配', tone: 'positive', matchStatus: status })
  else if (status === 'mismatch') tags.push({ label: '监听入口不同', tone: 'notice', matchStatus: status })
  else if (status === 'unknown') tags.push({ label: '监听关系未知', tone: 'neutral', matchStatus: status })
  return tags
}

function candidateTitle(candidate: DisplayCandidate) {
  if (candidate.group === 'current') return '当前访问地址'
  if (candidate.group === 'configured') return '公开访问地址'
  if (candidate.group === 'interface' && candidate.scope === 'private') return '局域网地址'
  if (candidate.group === 'interface' && candidate.scope === 'loopback') return '本机地址'
  if (candidate.group === 'interface') return '网络接口地址'
  return candidate.label?.trim() || '候选地址'
}

function listenFamilyLabel(family: ShareListenDiagnostic['family']) {
  if (family === 'ipv4') return 'IPv4'
  if (family === 'ipv6') return 'IPv6'
  return '地址族未确认'
}

function listenModeLabel(mode: ShareListenDiagnostic['mode']) {
  if (mode === 'wildcard') return '通配监听'
  if (mode === 'specific') return '指定地址'
  return '主机名监听'
}

function copyState(origin: string): CopyState {
  return copyStates.value[origin] || 'idle'
}

function copyButtonText(origin: string) {
  const state = copyState(origin)
  if (state === 'copied') return '已复制'
  if (state === 'error') return '重试复制'
  return '复制地址'
}

async function copyAddress(candidate: DisplayCandidate) {
  const ok = await copyToClipboard(candidate.origin)
  copyStates.value = { ...copyStates.value, [candidate.origin]: ok ? 'copied' : 'error' }
  copyAnnouncement.value = ''
  await nextTick()
  copyAnnouncement.value = ok
    ? `${candidateTitle(candidate)}已复制到剪贴板。`
    : `复制失败，请手动选择并复制${candidateTitle(candidate)}。`

  const previousTimer = resetTimers.get(candidate.origin)
  if (previousTimer !== undefined) window.clearTimeout(previousTimer)
  const timer = window.setTimeout(() => {
    copyStates.value = { ...copyStates.value, [candidate.origin]: 'idle' }
    resetTimers.delete(candidate.origin)
  }, 2200)
  resetTimers.set(candidate.origin, timer)
}

function selectAddress(event: FocusEvent) {
  ;(event.target as HTMLInputElement | null)?.select()
}

async function load() {
  loading.value = true
  loadError.value = false
  try {
    const result = await api.shareOrigins(currentOrigin.value)
    candidates.value = Array.isArray(result) ? result : []
  } catch {
    loadError.value = true
  } finally {
    loading.value = false
  }
}

onMounted(load)
onBeforeUnmount(() => {
  resetTimers.forEach((timer) => window.clearTimeout(timer))
  resetTimers.clear()
})
</script>

<template>
  <section class="panel service-address-card" aria-labelledby="service-addresses-title">
    <span class="service-address-aurora" aria-hidden="true" />

    <header class="service-address-header">
      <div class="service-address-title-wrap">
        <span class="service-address-icon" aria-hidden="true">
          <AppIcon name="link" :size="26" />
        </span>
        <div>
          <p class="eyebrow">连接入口</p>
          <h2 id="service-addresses-title">服务访问地址</h2>
          <p>候选地址根据当前入口、显式公开地址和本机网络接口生成。地址仅供复制，不会在此页面直接打开。</p>
        </div>
      </div>
      <div class="service-address-summary" role="status" aria-live="polite">
        <span v-if="loading" class="service-address-loader" aria-hidden="true" />
        <span>{{ loading ? '正在获取其他候选' : `${displayCandidates.length} 个候选地址` }}</span>
      </div>
    </header>

    <p class="visually-hidden" aria-live="polite" aria-atomic="true">{{ copyAnnouncement }}</p>

    <div v-if="candidateGroups.length" class="service-address-groups">
      <section
        v-for="group in candidateGroups"
        :key="group.key"
        class="service-address-group"
        :data-group="group.key"
        :aria-labelledby="`service-address-group-${group.key}`"
      >
        <div class="service-address-group-head">
          <div>
            <h3 :id="`service-address-group-${group.key}`">{{ group.title }}</h3>
            <p>{{ group.description }}</p>
          </div>
          <span>{{ group.items.length }}</span>
        </div>

        <div class="service-address-list">
          <article
            v-for="candidate in group.items"
            :key="candidate.origin"
            class="service-address-item"
            :data-group="candidate.group"
            data-testid="service-address-item"
          >
            <div class="service-address-item-head">
              <strong>{{ candidateTitle(candidate) }}</strong>
              <div class="service-address-tags" :aria-label="`${candidateTitle(candidate)}的地址信息`">
                <span
                  v-for="tag in candidateTags(candidate)"
                  :key="`${tag.label}:${tag.matchStatus || ''}`"
                  :data-tone="tag.tone"
                  :data-match-status="tag.matchStatus"
                >{{ tag.label }}</span>
              </div>
            </div>
            <div class="service-address-action">
              <input
                class="service-address-value"
                :value="candidate.origin"
                readonly
                :aria-label="candidateTitle(candidate)"
                @focus="selectAddress"
              />
              <button
                class="mini-btn service-address-copy"
                type="button"
                :aria-label="`${copyState(candidate.origin) === 'copied' ? '再次复制' : '复制'}${candidateTitle(candidate)}`"
                @click="copyAddress(candidate)"
              >
                {{ copyButtonText(candidate.origin) }}
              </button>
              <p v-if="copyState(candidate.origin) === 'error'" class="service-address-copy-error">
                复制失败，请手动选中地址复制。
              </p>
            </div>
          </article>
        </div>
      </section>
    </div>

    <div v-if="loadError" class="service-address-inline-state" role="status">
      <span>{{ currentOrigin ? (hasConfiguredCandidate ? '未能获取网络接口候选，仍可复制上方的当前访问地址和公开地址。' : '未能获取其他候选地址，仍可复制上方的当前访问地址。') : '候选地址加载失败，请稍后重试。' }}</span>
      <button class="ghost-btn" type="button" :disabled="loading" @click="load">重新获取</button>
    </div>
    <p v-else-if="showEmptyNotice" class="service-address-empty" role="status">
      暂未发现其他候选，当前访问地址仍可复制使用。
    </p>

    <section v-if="listenDiagnostics.length" class="service-listener-diagnostics" aria-labelledby="service-listener-title">
      <header class="service-listener-head">
        <div>
          <p class="service-listener-kicker">后端监听诊断</p>
          <h3 id="service-listener-title">服务进程监听</h3>
        </div>
        <span>仅供诊断</span>
      </header>
      <p class="service-listener-description">这里显示后端进程实际监听的端点。使用反向代理时，它可能只是内部监听端口，并不是浏览器访问地址，因此不会加入上方复制列表。</p>
      <div class="service-listener-list">
        <article v-for="listen in listenDiagnostics" :key="`${listen.network}:${listen.address}`" class="service-listener-item" data-testid="listen-diagnostic">
          <div class="service-listener-address">
            <span>监听地址</span>
            <code>{{ listen.address }}</code>
          </div>
          <div class="service-listener-tags" aria-label="监听端点信息">
            <span>{{ listen.network }}</span>
            <span>{{ listenFamilyLabel(listen.family) }}</span>
            <span>{{ listenModeLabel(listen.mode) }}</span>
            <span>外部可达性未知</span>
          </div>
        </article>
      </div>
    </section>

    <footer class="service-address-caution">
      <span class="service-address-caution-icon" aria-hidden="true">
        <AppIcon name="alert-triangle" :size="20" />
      </span>
      <div>
        <strong>连通性未知</strong>
        <p>系统未检测这些候选的连通性；实际可用性取决于防火墙、代理、TLS 和所在网络。</p>
      </div>
    </footer>
  </section>
</template>

<style scoped>
.service-address-card {
  position: relative;
  isolation: isolate;
  overflow: hidden;
  padding: var(--space-card);
  display: grid;
  gap: 18px;
  background:
    linear-gradient(145deg, rgba(255, 255, 255, .095), rgba(255, 255, 255, .042)),
    var(--panel);
}

.service-address-aurora {
  position: absolute;
  z-index: -1;
  top: -150px;
  right: -100px;
  width: 360px;
  height: 300px;
  border-radius: 50%;
  background: radial-gradient(circle, rgba(94, 224, 198, .17), rgba(180, 140, 255, .08) 45%, transparent 70%);
  filter: blur(12px);
  pointer-events: none;
}

.service-address-header {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 24px;
}

.service-address-title-wrap {
  min-width: 0;
  display: grid;
  grid-template-columns: 56px minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}

.service-address-icon {
  width: 56px;
  height: 56px;
  display: grid;
  place-items: center;
  border: 1px solid rgba(94, 224, 198, .28);
  border-radius: 19px;
  color: #071316;
  background: linear-gradient(135deg, var(--accent-2), #62a9ff);
  box-shadow: 0 14px 34px rgba(39, 152, 157, .2), inset 0 1px 0 rgba(255, 255, 255, .32);
}

.service-address-title-wrap .eyebrow {
  margin-bottom: 5px;
}

.service-address-title-wrap h2 {
  margin: 0;
  font-family: "Songti SC", "Noto Serif CJK SC", serif;
  font-size: clamp(24px, 3vw, 34px);
  line-height: 1.12;
  text-wrap: balance;
}

.service-address-title-wrap p:last-child {
  max-width: 720px;
  margin: 8px 0 0;
  color: var(--muted);
  line-height: 1.65;
  text-wrap: pretty;
}

.service-address-summary {
  min-height: 44px;
  flex: 0 0 auto;
  display: inline-flex;
  align-items: center;
  gap: 9px;
  padding: 9px 13px;
  border: 1px solid rgba(255, 255, 255, .11);
  border-radius: 999px;
  color: var(--accent-2);
  background: rgba(4, 8, 17, .28);
  font-size: 13px;
  font-weight: 750;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}

.service-address-loader {
  width: 15px;
  height: 15px;
  flex: 0 0 auto;
  border: 2px solid rgba(94, 224, 198, .22);
  border-top-color: var(--accent-2);
  border-radius: 50%;
  animation: service-address-spin .8s linear infinite;
}

@keyframes service-address-spin {
  to { transform: rotate(360deg); }
}

.service-address-groups {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  align-items: start;
  gap: 12px;
}

.service-address-group {
  min-width: 0;
  padding: 10px;
  border: 1px solid rgba(255, 255, 255, .1);
  border-radius: 22px;
  background: rgba(3, 6, 14, .22);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, .035);
}

.service-address-group[data-group="current"] {
  grid-column: 1 / -1;
  border-color: rgba(255, 184, 77, .24);
  background:
    linear-gradient(125deg, rgba(255, 184, 77, .09), rgba(94, 224, 198, .055)),
    rgba(3, 6, 14, .24);
}

.service-address-group-head {
  min-height: 48px;
  padding: 5px 6px 10px;
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}

.service-address-group-head h3 {
  margin: 0;
  font: 700 15px/1.35 "Songti SC", "Noto Serif CJK SC", serif;
}

.service-address-group-head p {
  margin: 3px 0 0;
  color: var(--muted);
  font-size: 12px;
  line-height: 1.45;
}

.service-address-group-head > span {
  min-width: 26px;
  min-height: 26px;
  display: grid;
  place-items: center;
  padding: 3px 8px;
  border-radius: 999px;
  color: var(--muted);
  background: rgba(255, 255, 255, .07);
  font-size: 12px;
  font-variant-numeric: tabular-nums;
}

.service-address-list {
  display: grid;
  gap: 8px;
}

.service-address-item {
  min-width: 0;
  padding: 14px;
  display: grid;
  gap: 11px;
  border: 1px solid rgba(255, 255, 255, .1);
  border-radius: 14px;
  background: rgba(255, 255, 255, .045);
  box-shadow: 0 9px 28px rgba(0, 0, 0, .12), inset 0 1px 0 rgba(255, 255, 255, .04);
  transition: border-color var(--motion), background var(--motion), box-shadow var(--motion);
}

.service-address-item:focus-within {
  border-color: rgba(255, 184, 77, .36);
  background: rgba(255, 255, 255, .06);
  box-shadow: 0 9px 28px rgba(0, 0, 0, .16), inset 0 1px 0 rgba(255, 255, 255, .055);
}

.service-address-item-head {
  min-width: 0;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.service-address-item-head strong {
  min-width: 0;
  line-height: 1.4;
}

.service-address-tags {
  display: flex;
  justify-content: flex-end;
  gap: 6px;
  flex-wrap: wrap;
}

.service-address-tags span {
  display: inline-flex;
  align-items: center;
  min-height: 25px;
  padding: 4px 8px;
  border: 1px solid rgba(255, 255, 255, .07);
  border-radius: 999px;
  color: var(--muted);
  background: rgba(255, 255, 255, .055);
  font-size: 11px;
  line-height: 1;
  white-space: nowrap;
}

.service-address-item[data-group="current"] .service-address-tags span:first-child {
  color: var(--accent);
  border-color: rgba(255, 184, 77, .2);
  background: rgba(255, 184, 77, .11);
}

.service-address-tags span[data-tone="positive"] {
  color: var(--accent-2);
  border-color: rgba(94, 224, 198, .2);
  background: rgba(94, 224, 198, .1);
}

.service-address-tags span[data-tone="notice"] {
  color: #f8dfb8;
  border-color: rgba(255, 184, 77, .2);
  background: rgba(255, 184, 77, .09);
}

.service-address-tags span[data-tone="neutral"] {
  color: var(--muted);
  border-color: rgba(255, 255, 255, .09);
  background: rgba(255, 255, 255, .045);
}

.service-address-action {
  min-width: 0;
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: 8px;
}

.service-address-value {
  min-width: 0;
  height: 44px;
  padding: 0 12px;
  border-color: rgba(255, 255, 255, .1);
  border-radius: 12px;
  color: var(--accent-2);
  background: rgba(0, 0, 0, .28);
  font: 500 13px/1.4 ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace;
  font-variant-numeric: tabular-nums;
}

.service-address-value:focus {
  border-color: rgba(94, 224, 198, .5);
  box-shadow: 0 0 0 3px rgba(94, 224, 198, .12);
}

.service-address-copy {
  min-width: 92px;
  min-height: 44px;
  white-space: nowrap;
}

.service-address-copy-error {
  grid-column: 1 / -1;
  margin: 0;
  color: #ffb9c1;
  font-size: 12px;
  line-height: 1.45;
}

.service-address-inline-state,
.service-address-empty {
  margin: 0;
  border: 1px solid rgba(255, 184, 77, .22);
  border-radius: var(--radius-md);
  color: #f8dfb8;
  background: rgba(255, 184, 77, .08);
  line-height: 1.55;
}

.service-address-inline-state {
  min-height: 56px;
  padding: 7px 8px 7px 14px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}

.service-address-inline-state .ghost-btn {
  min-height: 44px;
  flex: 0 0 auto;
}

.service-address-empty {
  padding: 13px 15px;
}

.service-listener-diagnostics {
  padding: 16px;
  display: grid;
  gap: 11px;
  border: 1px dashed rgba(255, 255, 255, .14);
  border-radius: 20px;
  background: rgba(3, 6, 14, .16);
  box-shadow: inset 0 1px 0 rgba(255, 255, 255, .025);
}

.service-listener-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
}

.service-listener-kicker {
  margin: 0 0 3px;
  color: var(--muted);
  font-size: 11px;
  font-weight: 800;
  letter-spacing: .11em;
  text-transform: uppercase;
}

.service-listener-head h3 {
  margin: 0;
  font: 700 17px/1.35 "Songti SC", "Noto Serif CJK SC", serif;
}

.service-listener-head > span {
  min-height: 28px;
  display: inline-flex;
  align-items: center;
  padding: 5px 9px;
  border-radius: 999px;
  color: var(--muted);
  background: rgba(255, 255, 255, .055);
  font-size: 11px;
  white-space: nowrap;
}

.service-listener-description {
  max-width: 820px;
  margin: 0;
  color: var(--muted);
  font-size: 13px;
  line-height: 1.6;
  text-wrap: pretty;
}

.service-listener-list {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 320px), 1fr));
  gap: 8px;
}

.service-listener-item {
  min-width: 0;
  padding: 11px 12px;
  display: grid;
  gap: 9px;
  border: 1px solid rgba(255, 255, 255, .08);
  border-radius: 13px;
  background: rgba(255, 255, 255, .032);
}

.service-listener-address {
  min-width: 0;
  display: grid;
  grid-template-columns: auto minmax(0, 1fr);
  gap: 10px;
  align-items: center;
}

.service-listener-address > span {
  color: var(--muted);
  font-size: 12px;
  white-space: nowrap;
}

.service-listener-address code {
  min-width: 0;
  overflow-wrap: anywhere;
  padding: 0;
  color: var(--text);
  background: transparent;
  font-size: 13px;
}

.service-listener-tags {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}

.service-listener-tags span {
  display: inline-flex;
  align-items: center;
  min-height: 24px;
  padding: 4px 8px;
  border-radius: 999px;
  color: var(--muted);
  background: rgba(255, 255, 255, .05);
  font-size: 11px;
  line-height: 1;
}

.service-address-caution {
  padding-top: 15px;
  display: grid;
  grid-template-columns: 36px minmax(0, 1fr);
  gap: 11px;
  align-items: start;
  border-top: 1px solid rgba(255, 255, 255, .09);
}

.service-address-caution-icon {
  width: 36px;
  height: 36px;
  display: grid;
  place-items: center;
  border-radius: 12px;
  color: var(--accent);
  background: rgba(255, 184, 77, .1);
}

.service-address-caution strong {
  color: #f8dfb8;
  font-size: 13px;
}

.service-address-caution p {
  margin: 3px 0 0;
  color: var(--muted);
  font-size: 13px;
  line-height: 1.55;
  text-wrap: pretty;
}

@media (max-width: 720px) {
  .service-address-header {
    align-items: stretch;
    flex-direction: column;
    gap: 16px;
  }

  .service-address-summary {
    align-self: flex-start;
  }

  .service-address-groups {
    grid-template-columns: 1fr;
  }

  .service-address-group[data-group="current"] {
    grid-column: auto;
  }
}

@media (max-width: 520px) {
  .service-address-card {
    gap: 15px;
  }

  .service-address-title-wrap {
    grid-template-columns: 48px minmax(0, 1fr);
    gap: 12px;
  }

  .service-address-icon {
    width: 48px;
    height: 48px;
    border-radius: 17px;
  }

  .service-address-item-head,
  .service-address-inline-state {
    align-items: stretch;
    flex-direction: column;
  }

  .service-address-tags {
    justify-content: flex-start;
  }

  .service-listener-head {
    align-items: stretch;
    flex-direction: column;
  }

  .service-listener-head > span {
    align-self: flex-start;
  }

  .service-listener-address {
    grid-template-columns: 1fr;
    gap: 5px;
  }

  .service-address-action {
    grid-template-columns: 1fr;
  }

  .service-address-copy,
  .service-address-inline-state .ghost-btn {
    width: 100%;
  }

  .service-address-copy-error {
    grid-column: auto;
  }
}
</style>
