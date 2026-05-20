import { createApp } from 'vue'
import App from '@/App.vue'
import router from '@/router'
import '@/styles.css'

// 前端只创建一个 Vue 应用实例，路由守卫会在进入受保护页面前恢复登录态。
createApp(App).use(router).mount('#app')
