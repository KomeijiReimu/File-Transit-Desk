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

const groupDefinitions: Array<{ key: AddressGroupKey; title: string }> = [
  { key: 'current', title: '当前地址' },
  { key: 'configured', title: '公开地址' },
  { key: 'interface', title: '本机地址' },
  { key: 'other', title: '其他地址' },
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
  <section class="service-address-card" aria-labelledby="service-addresses-title">
    <header class="service-address-header">
      <div class="service-address-title-wrap">
        <span class="service-address-icon" aria-hidden="true">
          <AppIcon name="link" :size="26" />
        </span>
        <div>
          <h2 id="service-addresses-title">访问地址</h2>
        </div>
      </div>
      <div class="service-address-summary" role="status" aria-live="polite">
        <span v-if="loading" class="service-address-loader" aria-hidden="true" />
        <span>{{ loading ? '加载中…' : `${displayCandidates.length} 个地址` }}</span>
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
          <h3 :id="`service-address-group-${group.key}`">{{ group.title }}</h3>
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
              <details class="service-address-details"><summary>详情</summary><div class="service-address-tags" :aria-label="`${candidateTitle(candidate)}的地址信息`">
                <span
                  v-for="tag in candidateTags(candidate)"
                  :key="`${tag.label}:${tag.matchStatus || ''}`"
                  :data-tone="tag.tone"
                  :data-match-status="tag.matchStatus"
                >{{ tag.label }}</span>
              </div></details>
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
      <span>{{ currentOrigin ? (hasConfiguredCandidate ? '其他地址加载失败，仍可使用当前地址和公开地址。' : '其他地址加载失败，仍可使用当前地址。') : '地址加载失败，请重试。' }}</span>
      <button class="ghost-btn" type="button" :disabled="loading" @click="load">重新获取</button>
    </div>
    <p v-else-if="showEmptyNotice" class="service-address-empty" role="status">
      没有其他地址。
    </p>

    <details v-if="listenDiagnostics.length" class="service-listener-diagnostics">
      <summary>服务器监听信息</summary>
      <p class="service-listener-description">监听地址可能仅供服务器内部使用，不一定能从浏览器访问。</p>
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
    </details>

    <footer class="service-address-caution">
      <span class="service-address-caution-icon" aria-hidden="true">
        <AppIcon name="alert-triangle" :size="20" />
      </span>
      <div>
        <p>这些地址尚未测试，请确认接收方能够访问后再分享。</p>
      </div>
    </footer>
  </section>
</template>
