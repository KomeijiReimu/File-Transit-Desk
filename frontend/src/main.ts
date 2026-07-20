import { createApp } from 'vue'
import App from '@/App.vue'
import router, { installAuthenticationRouteSync } from '@/router'
import '@/styles.css'

// 前端只创建一个 Vue 应用实例，路由守卫会在进入受保护页面前恢复登录态。
export const app = createApp(App)
app.use(router)
export const stopAuthenticationRouteSync = installAuthenticationRouteSync()
app.mount('#app')

if (import.meta.hot) {
  import.meta.hot.dispose(() => stopAuthenticationRouteSync())
}
