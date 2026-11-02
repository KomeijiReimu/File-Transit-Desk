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

    expect((wrapper.get('[data-testid="service-address-item"] input').element as HTMLInputElement).value).toBe(currentOrigin)

    resolveRequest([])
    await flushPromises()
    wrapper.unmount()
  })

  it('normalizes the frontend-configured public origin and sorts it between current and API interface candidates', async () => {
    configuredPublicShareOrigin.mockReturnValue('HTTPS://Files.Example:443')
    shareOrigins.mockResolvedValue([...candidates, candidates[0]])
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

    expect(shareOrigins).toHaveBeenCalledWith(currentOrigin)
    const rows = wrapper.findAll('[data-testid="service-address-item"]')
    expect(rows.map((row) => row.get('input').element.value)).toEqual([
      currentOrigin,
      'https://files.example',
      'http://192.168.20.8:3000',
    ])
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
    expect(wrapper.findAll('.service-address-value').map((input) => (input.element as HTMLInputElement).value)).toEqual([
      currentOrigin,
      'https://files.example',
      'http://192.168.20.8:3000',
    ])

    await wrapper.get('button[aria-label="复制公开访问地址"]').trigger('click')
    await flushPromises()
    expect(copyToClipboard).toHaveBeenCalledWith('https://files.example')
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
    expect(wrapper.get('button[aria-label="复制当前访问地址"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.get('button[aria-label="复制公开访问地址"]').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('announces a successful copy and updates the visible button state', async () => {
    shareOrigins.mockResolvedValue([])
    copyToClipboard.mockResolvedValue(true)
    const wrapper = mount(ServiceAddressesCard)
    await flushPromises()

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

})
