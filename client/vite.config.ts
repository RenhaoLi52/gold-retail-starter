import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// Vite 配置：
// - plugin-vue 让 Vite 认识 .vue 单文件组件
// - server.proxy 把前端发出的 /api 请求转发到本机 8080 的 Go 后端，
//   这样开发时前端(5173)和后端(8080)虽然是两个端口，代码里只写 /api/... 即可
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
