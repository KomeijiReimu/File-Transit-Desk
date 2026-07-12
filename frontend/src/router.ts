import { nextTick } from 'vue'
import { createRouter, createWebHistory } from 'vue-router'
import { authState, restoreSession } from '@/auth'

const router = createRouter({
  history: createWebHistory(),
  scrollBehavior(to, from, savedPosition) {
    if (savedPosition) return savedPosition
    if (to.path === from.path) return false
    return { top: 0 }
  },
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/LoginView.vue'), meta: { public: true, title: '登录' } },
    {
      path: '/share/:token',
      name: 'share',
      component: () => import('@/views/ShareView.vue'),
      meta: { public: true, title: '临时分享' },
      props: true,
    },
    { path: '/', redirect: '/files' },
    { path: '/files', name: 'files', component: () => import('@/views/FilesView.vue'), meta: { title: '文件浏览' } },
    { path: '/upload', name: 'upload', component: () => import('@/views/UploadView.vue'), meta: { title: '文件上传' } },
    { path: '/tokens', name: 'tokens', component: () => import('@/views/TokensView.vue'), meta: { adminOnly: true, title: '令牌管理' } },
    { path: '/transfers', name: 'transfers', component: () => import('@/views/TransfersView.vue'), meta: { adminOnly: true, title: '正在传输' } },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue'), meta: { adminOnly: true, title: '访问记录' } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { adminOnly: true, title: '配置管理' } },
    { path: '/:pathMatch(.*)*', redirect: '/files' },
  ],
})

router.afterEach(async (to, from) => {
  const title = typeof to.meta.title === 'string' ? to.meta.title : '文件传输台'
  document.title = title === '文件传输台' ? title : `${title} · 文件传输台`
  if (to.name === from.name) return
  await nextTick()
  window.requestAnimationFrame(() => {
    document.querySelector<HTMLElement>('[data-route-focus]')?.focus({ preventScroll: true })
  })
})

router.beforeEach(async (to) => {
  // 首次进入受保护页面前先恢复会话，避免刷新后短暂闪到登录页。
  if ((to.name === 'login' || !to.meta.public) && !authState.ready) await restoreSession()
  if (!to.meta.public && !authState.authenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && authState.authenticated) return { name: 'files' }
  // 管理页只允许 admin；普通用户被温和带回文件浏览页。
  if (to.meta.adminOnly && authState.role !== 'admin') return { name: 'files' }
  return true
})

export default router
