import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

const viewMocks = vi.hoisted(() => ({
  route: { name: 'files', query: {} as Record<string, string> },
  push: vi.fn(),
  dirs: vi.fn(),
  listFiles: vi.fn(),
  createDownloadLease: vi.fn(),
  authState: undefined as { admin: boolean } | undefined,
}))

vi.mock('@/api', () => {
  class MockApiError extends Error {
    status: number

    constructor(message: string, status: number) {
      super(message)
      this.name = 'ApiError'
      this.status = status
    }
  }

  return {
    ApiError: MockApiError,
    api: {
      dirs: viewMocks.dirs,
      listFiles: viewMocks.listFiles,
      createDownloadLease: viewMocks.createDownloadLease,
    },
  }
})

vi.mock('@/auth', async () => {
  const { computed, reactive } = await vi.importActual<typeof import('vue')>('vue')
  const state = reactive({ admin: false })
  viewMocks.authState = state
  return { isAdmin: computed(() => state.admin) }
})

vi.mock('vue-router', () => ({
  useRoute: () => viewMocks.route,
  useRouter: () => ({ push: viewMocks.push }),
}))

vi.mock('@/useGsapEntrance', () => ({ useGsapEntrance: () => {} }))

import FilesView from '@/views/FilesView.vue'

const RouterLinkStub = {
  props: ['to'],
  template: '<a href="#"><slot /></a>',
}

async function mountView() {
  const wrapper = mount(FilesView, {
    global: { stubs: { RouterLink: RouterLinkStub } },
  })
  await flushPromises()
  return wrapper
}

describe('FilesView responsive file actions', () => {
  beforeEach(() => {
    viewMocks.route.name = 'files'
    viewMocks.route.query = {}
    viewMocks.push.mockReset().mockResolvedValue(undefined)
    viewMocks.authState!.admin = false
    viewMocks.dirs.mockReset().mockResolvedValue([{
      id: 'shared',
      name: 'Shared',
      label: '共享文件',
      type: 'directory',
      canDownload: true,
      canUpload: true,
    }])
    viewMocks.listFiles.mockReset().mockResolvedValue({
      entries: [],
      canDownload: true,
      canUpload: true,
      page: 1,
      pageSize: 100,
      hasMore: false,
      totalKnown: true,
      total: 0,
    })
    viewMocks.createDownloadLease.mockReset().mockResolvedValue({ url: '/t/download' })
  })

  it('keeps a long file name and the ordinary-user download action inside the responsive table structure', async () => {
    const longName = `${'季度归档与跨区域同步记录-'.repeat(10)}最终版本.tar.zst`
    viewMocks.listFiles.mockResolvedValue({
      entries: [{
        name: longName,
        path: longName,
        type: 'file',
        size: 8_192,
        modifiedAt: '2026-07-21T10:00:00Z',
        metadataKnown: true,
        downloadable: true,
      }],
      canDownload: true,
      canUpload: false,
      totalKnown: true,
      total: 1,
    })

    const wrapper = await mountView()

    expect(wrapper.find('.files-table-scroll').exists()).toBe(true)
    expect(wrapper.get('#files-list-table').classes()).toContain('files-table')
    expect(wrapper.get('.file-name > span:last-child').text()).toBe(longName)
    const actionCell = wrapper.get('td[data-label="操作"]')
    expect(actionCell.classes()).toEqual(expect.arrayContaining(['actions', 'files-actions-cell']))
    const actions = actionCell.findAll('.mini-btn')
    expect(actions.map((action) => action.text())).toEqual(['下载'])
    expect(actions[0].attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('renders the administrator download and share controls together in the stable action cell', async () => {
    viewMocks.authState!.admin = true
    viewMocks.listFiles.mockResolvedValue({
      entries: [{
        name: 'release.iso',
        path: 'release.iso',
        type: 'file',
        size: 4_096,
        modifiedAt: '2026-07-21T10:00:00Z',
        metadataKnown: true,
        downloadable: true,
      }],
      canDownload: true,
      canUpload: true,
      totalKnown: true,
      total: 1,
    })

    const wrapper = await mountView()
    const actionCell = wrapper.get('td.files-actions-cell.actions')

    expect(actionCell.findAll('.mini-btn').map((action) => action.text())).toEqual(['下载', '创建分享'])
    expect(actionCell.get('button').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('keeps a single-file resource downloadable through the same responsive action layout', async () => {
    viewMocks.dirs.mockResolvedValue([{
      id: 'single-file',
      name: 'single-file',
      label: '单文件资源',
      type: 'file',
      canDownload: true,
      canUpload: false,
    }])
    viewMocks.listFiles.mockResolvedValue({
      entries: [{
        name: 'single-file.bin',
        path: 'single-file.bin',
        type: 'file',
        size: 2_048,
        metadataKnown: true,
        downloadable: true,
      }],
      canDownload: true,
      canUpload: false,
      totalKnown: true,
      total: 1,
    })

    const wrapper = await mountView()

    expect(wrapper.get('.dir-summary .pill').text()).toBe('单文件')
    expect(wrapper.get('td.files-actions-cell').text()).toContain('下载')
    expect(wrapper.get('td.files-actions-cell .mini-btn').attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})
