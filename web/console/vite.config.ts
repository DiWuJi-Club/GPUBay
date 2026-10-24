import { fileURLToPath, URL } from 'node:url'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import { defineConfig } from 'vite'

// 开发时预列用到的 Element Plus 样式，避免首次打开多刷新一次
const epComponents = [
  'button', 'input', 'input-number', 'form', 'form-item', 'dialog', 'message', 'table', 'table-column', 'tag', 'card', 'select', 'option',
  'radio-group', 'radio-button', 'tabs', 'tab-pane', 'pagination', 'upload', 'alert', 'skeleton', 'empty', 'descriptions', 'descriptions-item',
  'divider', 'link', 'result', 'checkbox', 'icon', 'drawer', 'dropdown', 'dropdown-menu', 'dropdown-item', 'progress', 'tooltip',
]

export default defineConfig({
  base: '/',
  plugins: [
    vue(),
    // 生成的 d.ts 入库，vue-tsc --noEmit 在 vite build 之前运行时也能识别自动引入的组件与 ElMessage
    AutoImport({ resolvers: [ElementPlusResolver()], dts: 'src/auto-imports.d.ts' }),
    Components({ resolvers: [ElementPlusResolver()], dts: 'src/components.d.ts' }),
  ],
  resolve: {
    alias: {
      '@spark/shared': fileURLToPath(new URL('../shared/src/index.ts', import.meta.url)),
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: { port: 5173, proxy: { '/api': 'http://127.0.0.1:8080' } },
  optimizeDeps: { include: ['element-plus/es', ...epComponents.map((c) => `element-plus/es/components/${c}/style/css`)] },
  build: { outDir: fileURLToPath(new URL('../../internal/web/dist/console', import.meta.url)), emptyOutDir: true },
})
