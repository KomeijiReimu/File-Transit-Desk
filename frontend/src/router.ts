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
    { path: '/tokens', name: 'tokens', component: () => import('@/views/TokensView.vue'), meta: { adminOnly: true } },
    { path: '/audit', name: 'audit', component: () => import('@/views/AuditView.vue'), meta: { adminOnly: true } },
    { path: '/config', name: 'config', component: () => import('@/views/ConfigView.vue'), meta: { adminOnly: true } },
    { path: '/:pathMatch(.*)*', redirect: '/files' },
  ],
})

router.beforeEach(async (to) => {
  if ((to.name === 'login' || !to.meta.public) && !authState.ready) await restoreSession()
  if (!to.meta.public && !authState.authenticated) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  if (to.name === 'login' && authState.authenticated) return { name: 'files' }
  if (to.meta.adminOnly && authState.role !== 'admin') return { name: 'files' }
  return true
})

export default router
