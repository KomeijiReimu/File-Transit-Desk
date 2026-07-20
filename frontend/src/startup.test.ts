import { afterEach, describe, expect, it, vi } from 'vitest'

describe('real module startup', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    document.body.innerHTML = ''
    window.history.replaceState({}, '', '/')
  })

  it('evaluates and mounts the real auth-api-router entry cycle without reading authState in its TDZ', async () => {
    vi.resetModules()
    window.history.replaceState({}, '', '/login')
    document.body.innerHTML = '<div id="app"></div>'
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response(JSON.stringify({ error: 'unauthorized' }), {
      status: 401,
      headers: { 'content-type': 'application/json' },
    })))

    const mainModule = await import('@/main')
    const routerModule = await import('@/router')
    await routerModule.default.isReady()

    expect(document.querySelector('#app')?.innerHTML).not.toBe('')
    expect(routerModule.default.currentRoute.value.name).toBe('login')

    mainModule.stopAuthenticationRouteSync()
    mainModule.app.unmount()
  })
})
