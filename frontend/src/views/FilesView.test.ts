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

describe('FilesView file actions', () => {
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

  it.each([false, true])('downloads a listed file with administrator=%s and gates sharing by role', async (admin) => {
    viewMocks.authState!.admin = admin
    if (admin) viewMocks.dirs.mockResolvedValue([{ id: 'shared', name: 'Shared', type: 'file', canDownload: true, canUpload: false }])
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

    expect(wrapper.text()).toContain(longName)
    const share = wrapper.findAllComponents(RouterLinkStub).find((link) => link.props('to')?.name === 'tokens')
    expect(Boolean(share)).toBe(admin)
    if (admin) expect(share!.props('to')).toEqual({ name: 'tokens', query: { type: 'download', dirId: 'shared', path: longName } })
    const download = wrapper.findAll('button').find((button) => button.text() === '下载')!
    const assign = vi.spyOn(window.location, 'assign').mockImplementation(() => {})
    await download.trigger('click')
    await flushPromises()
    expect(viewMocks.createDownloadLease).toHaveBeenCalledWith('shared', longName)
    expect(assign).toHaveBeenCalledWith(expect.stringContaining('/t/download'))
    assign.mockRestore()
    wrapper.unmount()
  })
})
