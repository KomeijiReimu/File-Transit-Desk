import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import type { ProxyOptions } from 'vite'
import vue from '@vitejs/plugin-vue'

const backendOrigin = process.env.VITE_BACKEND_ORIGIN || process.env.BACKEND_ORIGIN || 'http://127.0.0.1:17878'
const backendProxyOrigin = new URL(backendOrigin).origin

const backendProxy = (target: string): ProxyOptions => ({
  target,
  changeOrigin: true,
  configure(proxy) {
    proxy.on('proxyReq', (proxyReq) => {
      // 局域网访问 Vite 时浏览器 Origin 会是 http://局域网IP:5173；开发代理转发到后端前改成后端同源，避免触发后端 CSRF Origin 防护。
      proxyReq.setHeader('Origin', backendProxyOrigin)
    })
  },
})

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    proxy: {
      '/api': backendProxy(backendOrigin),
      '^/t/': backendProxy(backendOrigin),
    },
  },
})
