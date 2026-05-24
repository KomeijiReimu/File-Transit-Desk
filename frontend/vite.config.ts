import { fileURLToPath, URL } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

const backendOrigin = process.env.VITE_BACKEND_ORIGIN || process.env.BACKEND_ORIGIN || 'http://localhost:17878'

export default defineConfig({
  plugins: [vue()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    proxy: {
      '/api': {
        target: backendOrigin,
        changeOrigin: true,
      },
      '^/t/': {
        target: backendOrigin,
        changeOrigin: true,
      },
    },
  },
})
