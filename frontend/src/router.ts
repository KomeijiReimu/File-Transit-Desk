import { createRouter, createWebHistory } from 'vue-router'
import { authState, restoreSession } from '@/auth'

const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('@/views/LoginView.vue'), meta: { public: true } },
    {
      path: '/share/:token',
      name: 'share',
      component: () => import('@/views/ShareView.vue'),
      meta: { public: true },
      props: true,
    },
    { path: '/', redirect: '/files' },
    { path: '/files', name: 'files', component: () => import('@/views/FilesView.vue') },
    { path: '/upload', name: 'upload', component: () => import('@/views/UploadView.vue') },
    { path: '/tokens', name: 'tokens', component: () => import('@/views/TokensView.vue'), meta: { adminOnly: true } },
    { path: '/transfers', name: 'transfers', component: () => import('@/views/TransfersView.vue'), meta: { adminOnly: true } },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue'), meta: { adminOnly: true } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { adminOnly: true } },
    { path: '/:pathMatch(.*)*', redirect: '/files' },
  ],
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
