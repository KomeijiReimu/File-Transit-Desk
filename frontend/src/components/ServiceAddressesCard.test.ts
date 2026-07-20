import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const shareOrigins = vi.hoisted(() => vi.fn())
const copyToClipboard = vi.hoisted(() => vi.fn())
const configuredPublicShareOrigin = vi.hoisted(() => vi.fn())

vi.mock('@/api', () => ({ api: { shareOrigins } }))
vi.mock('@/utils', () => ({ configuredPublicShareOrigin, copyToClipboard }))

import ServiceAddressesCard from '@/components/ServiceAddressesCard.vue'
import type { ShareListenDiagnostic, ShareOriginCandidate } from '@/types'

const currentOrigin = 'http://localhost:3000'
const loopbackListener: ShareListenDiagnostic = {
  source: 'listen',
  network: 'tcp4',
  family: 'ipv4',
  mode: 'specific',
  host: '127.0.0.1',
  port: 17878,
  address: '127.0.0.1:17878',
  reachable: 'unknown',
}

const candidates: ShareOriginCandidate[] = [
  {
    origin: 'http://192.168.20.8:3000',
    label: '局域网 192.168.20.8',
    source: 'interface',
    sources: ['interface'],
    scope: 'private',
    interface: 'eth0',
    listenMatch: true,
    reachable: 'unknown',
  },
  {
    origin: currentOrigin,
    label: '当前访问地址',
    source: 'current',
    sources: ['current'],
    scope: 'loopback',
    listenMatch: false,
    reachable: 'unknown',
  },
]

describe('ServiceAddressesCard', () => {
  beforeEach(() => {
    window.history.replaceState({}, '', '/config')
    shareOrigins.mockReset()
    copyToClipboard.mockReset()
    configuredPublicShareOrigin.mockReset().mockReturnValue('')
  })

  it('shows the current origin immediately while additional candidates are loading', async () => {
    let resolveRequest!: (value: ShareOriginCandidate[]) => void
    shareOrigins.mockReturnValue(new Promise<ShareOriginCandidate[]>((resolve) => { resolveRequest = resolve }))
    const wrapper = mount(ServiceAddressesCard)

    expect(wrapper.text()).toContain('正在获取其他候选')
    expect((wrapper.get('[data-testid="service-address-item"] input').element as HTMLInputElement).value).toBe(currentOrigin)

    resolveRequest([])
    await flushPromises()
    expect(wrapper.text()).toContain('暂未发现其他候选')
    wrapper.unmount()
  })

  it('normalizes the frontend-configured public origin and sorts it between current and API interface candidates', async () => {
    configuredPublicShareOrigin.mockReturnValue('HTTPS://Files.Example:443')
    shareOrigins.mockResolvedValue(candidates)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(shareOrigins).toHaveBeenCalledWith(currentOrigin)
    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows.map((row) => row.get('input').element.value)).toEqual([
      currentOrigin,
      'https://files.example',
      'http://192.168.20.8:3000',
    ])
    expect(rows.map((row) => row.attributes('data-group'))).toEqual(['current', 'configured', 'interface'])
    expect(wrapper.text()).toContain('当前访问地址')
    expect(wrapper.text()).toContain('公开访问地址')
    expect(wrapper.text()).not.toContain('显式配置的对外访问地址')
    expect(wrapper.text()).toContain('网络接口')
    expect(wrapper.text()).toContain('局域网地址')
    expect(wrapper.text()).toContain('IPv4')
    expect(wrapper.text()).toContain('私网')
    expect(wrapper.find('a').exists()).toBe(false)
    wrapper.unmount()
  })

  it.each([
    ['', '空配置'],
    ['javascript:alert(1)', '非 HTTP 协议'],
    ['https://user:secret@public.example', '带凭据地址'],
    ['https://public.example/path', '带路径地址'],
    ['https://public.example?source=config', '带查询地址'],
    ['//public.example', '缺少协议地址'],
  ])('safely ignores %s (%s)', async (configured) => {
    configuredPublicShareOrigin.mockReturnValue(configured)
    shareOrigins.mockResolvedValue([])
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows).toHaveLength(1)
    expect((rows[0].get('input').element as HTMLInputElement).value).toBe(currentOrigin)
    expect(wrapper.text()).not.toContain('公开访问地址')
    wrapper.unmount()
  })

  it('merges a configured origin equal to the current origin and preserves current as the highest-priority meaning', async () => {
    configuredPublicShareOrigin.mockReturnValue(`${currentOrigin}/`)
    shareOrigins.mockResolvedValue([
      {
        origin: currentOrigin,
        label: '当前访问地址',
        source: 'current',
        sources: ['current'],
        scope: 'loopback',
        reachable: 'unknown',
      },
    ] satisfies ShareOriginCandidate[])
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows).toHaveLength(1)
    expect(rows[0].attributes('data-group')).toBe('current')
    expect(rows[0].text()).toContain('当前访问地址')
    expect(rows[0].text()).toContain('公开配置')
    expect(wrapper.find('[aria-labelledby="service-address-group-configured"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('merges a configured origin equal to an API interface candidate without losing interface or listener metadata', async () => {
    const sharedOrigin = 'http://192.168.20.8:3000'
    configuredPublicShareOrigin.mockReturnValue(sharedOrigin)
    shareOrigins.mockResolvedValue([
      {
        origin: sharedOrigin,
        label: '局域网 192.168.20.8',
        source: 'interface',
        sources: ['interface'],
        scope: 'private',
        interface: 'eth0',
        listen: loopbackListener,
        listenMatchStatus: 'match',
        listenMatch: true,
        reachable: 'unknown',
      },
    ] satisfies ShareOriginCandidate[])
    copyToClipboard.mockResolvedValue(true)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows).toHaveLength(2)
    expect(rows.map((row) => row.attributes('data-group'))).toEqual(['current', 'configured'])
    const sharedRow = rows[1]
    expect((sharedRow.get('input').element as HTMLInputElement).value).toBe(sharedOrigin)
    expect(sharedRow.text()).toContain('公开访问地址')
    expect(sharedRow.text()).toContain('公开配置')
    expect(sharedRow.text()).toContain('本机网卡')
    expect(sharedRow.text()).toContain('私网')
    expect(sharedRow.text()).toContain('监听匹配')
    expect(wrapper.findAll('[data-testid="listen-diagnostic"]')).toHaveLength(1)

    await sharedRow.get('button').trigger('click')
    await flushPromises()
    expect(copyToClipboard).toHaveBeenCalledWith(sharedOrigin)
    wrapper.unmount()
  })

  it('deduplicates backend listener diagnostics and never turns a loopback socket into a copy candidate', async () => {
    configuredPublicShareOrigin.mockReturnValue('https://files.example')
    shareOrigins.mockResolvedValue([
      {
        origin: currentOrigin,
        label: '当前访问地址',
        source: 'current',
        scope: 'loopback',
        listen: loopbackListener,
        listenMatchStatus: 'match',
        listenMatch: true,
        reachable: 'unknown',
      },
      {
        origin: 'http://192.168.20.8:3000',
        label: '局域网 192.168.20.8',
        source: 'interface',
        scope: 'private',
        listen: loopbackListener,
        listenMatchStatus: 'match',
        listenMatch: true,
        reachable: 'unknown',
      },
    ] satisfies ShareOriginCandidate[])
    copyToClipboard.mockResolvedValue(true)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    const diagnostics = wrapper.findAll('[data-testid="listen-diagnostic"]')
    expect(diagnostics).toHaveLength(1)
    expect(diagnostics[0].text()).toContain('127.0.0.1:17878')
    expect(diagnostics[0].text()).not.toContain('http://127.0.0.1:17878')
    expect(diagnostics[0].find('input').exists()).toBe(false)
    expect(diagnostics[0].find('button').exists()).toBe(false)
    expect(wrapper.findAll('.service-address-copy')).toHaveLength(3)
    expect(wrapper.findAll('.service-address-value').map((input) => (input.element as HTMLInputElement).value)).toEqual([
      currentOrigin,
      'https://files.example',
      'http://192.168.20.8:3000',
    ])
    expect(wrapper.text()).toContain('反向代理')
    expect(wrapper.text()).toContain('不会加入上方复制列表')

    await wrapper.get('button[aria-label="复制公开访问地址"]').trigger('click')
    await flushPromises()
    expect(copyToClipboard).toHaveBeenCalledWith('https://files.example')
    wrapper.unmount()
  })

  it('presents match, mismatch and unknown listener relationships as three distinct non-blocking states', async () => {
    shareOrigins.mockResolvedValue([
      {
        origin: currentOrigin,
        label: '当前访问地址',
        source: 'current',
        scope: 'loopback',
        listen: loopbackListener,
        listenMatchStatus: 'unknown',
        reachable: 'unknown',
      },
      {
        origin: 'http://10.0.0.8:3000',
        label: '局域网 10.0.0.8',
        source: 'interface',
        scope: 'private',
        listen: loopbackListener,
        listenMatchStatus: 'mismatch',
        listenMatch: false,
        reachable: 'unknown',
      },
      {
        origin: 'http://192.168.20.8:3000',
        label: '局域网 192.168.20.8',
        source: 'interface',
        scope: 'private',
        listen: loopbackListener,
        listenMatchStatus: 'match',
        listenMatch: true,
        reachable: 'unknown',
      },
    ] satisfies ShareOriginCandidate[])
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(wrapper.get('[data-match-status="match"]').text()).toBe('监听匹配')
    expect(wrapper.get('[data-match-status="match"]').attributes('data-tone')).toBe('positive')
    expect(wrapper.get('[data-match-status="mismatch"]').text()).toBe('监听入口不同')
    expect(wrapper.get('[data-match-status="mismatch"]').attributes('data-tone')).toBe('notice')
    expect(wrapper.get('[data-match-status="unknown"]').text()).toBe('监听关系未知')
    expect(wrapper.get('[data-match-status="unknown"]').attributes('data-tone')).toBe('neutral')
    expect(wrapper.find('.alert.error').exists()).toBe(false)
    wrapper.unmount()
  })

  it('labels carrier-grade NAT and global-unicast scopes without claiming public reachability', async () => {
    shareOrigins.mockResolvedValue([
      {
        origin: 'http://100.64.1.2:3000',
        label: '网络接口 100.64.1.2',
        source: 'interface',
        scope: 'carrier-grade-nat',
      },
      {
        origin: 'http://203.0.113.8:3000',
        label: '网络接口 203.0.113.8',
        source: 'interface',
        scope: 'global-unicast',
      },
    ] satisfies ShareOriginCandidate[])
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(wrapper.text()).toContain('运营商级 NAT（CGNAT）')
    expect(wrapper.text()).toContain('全局单播/其他网络')
    expect(wrapper.text()).not.toContain('公网可达')
    wrapper.unmount()
  })

  it('keeps legacy API responses without listener metadata copyable and free of diagnostic placeholders', async () => {
    shareOrigins.mockResolvedValue([
      { origin: 'http://192.168.50.8:3000', label: '局域网 192.168.50.8', source: 'interface', scope: 'private' },
    ] satisfies ShareOriginCandidate[])
    copyToClipboard.mockResolvedValue(true)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(wrapper.find('[data-testid="listen-diagnostic"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('后端监听诊断')
    await wrapper.get('button[aria-label="复制局域网地址"]').trigger('click')
    await flushPromises()
    expect(copyToClipboard).toHaveBeenCalledWith('http://192.168.50.8:3000')
    wrapper.unmount()
  })

  it('keeps the current and configured origins available when the API fails', async () => {
    configuredPublicShareOrigin.mockReturnValue('https://public.example')
    shareOrigins.mockRejectedValue(new Error('network down'))
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows).toHaveLength(2)
    expect(rows[0].get('input').element.value).toBe(currentOrigin)
    expect(rows[1].get('input').element.value).toBe('https://public.example')
    expect(rows[0].text()).toContain('当前访问地址')
    expect(rows[1].text()).toContain('公开访问地址')
    expect(wrapper.text()).toContain('仍可复制上方的当前访问地址和公开地址')
    expect(wrapper.get('button[aria-label="复制当前访问地址"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.get('button[aria-label="复制公开访问地址"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('announces a successful copy and updates the visible button state', async () => {
    shareOrigins.mockResolvedValue([])
    copyToClipboard.mockResolvedValue(true)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(wrapper.text()).toContain('暂未发现其他候选')
    await wrapper.get('button[aria-label="复制当前访问地址"]').trigger('click')
    await flushPromises()

    expect(copyToClipboard).toHaveBeenCalledWith(currentOrigin)
    expect(wrapper.get('[aria-live="polite"][aria-atomic="true"]').text()).toBe('当前访问地址已复制到剪贴板。')
    expect(wrapper.get('button[aria-label="再次复制当前访问地址"]').text()).toBe('已复制')
    wrapper.unmount()
  })

  it('announces copy failure and tells the administrator how to copy manually', async () => {
    shareOrigins.mockResolvedValue([])
    copyToClipboard.mockResolvedValue(false)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    await wrapper.get('button[aria-label="复制当前访问地址"]').trigger('click')
    await flushPromises()

    expect(wrapper.get('[aria-live="polite"][aria-atomic="true"]').text()).toContain('复制失败，请手动选择并复制当前访问地址')
    expect(wrapper.get('.service-address-copy-error').text()).toBe('复制失败，请手动选中地址复制。')
    expect(wrapper.get('button[aria-label="复制当前访问地址"]').text()).toBe('重试复制')
    wrapper.unmount()
  })

  it('keeps the reachability warning explicit even when candidates load successfully', async () => {
    shareOrigins.mockResolvedValue(candidates)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(wrapper.text()).toContain('连通性未知')
    expect(wrapper.text()).toContain('系统未检测这些候选的连通性')
    expect(wrapper.text()).toContain('防火墙、代理、TLS 和所在网络')
    expect(wrapper.text()).not.toContain('已验证可达')
    wrapper.unmount()
  })
})
