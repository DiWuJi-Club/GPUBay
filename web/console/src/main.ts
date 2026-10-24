import { brand } from '@spark/shared'
import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
// ElMessage / ElMessageBox 是显式 import 的函数式组件，自动引入插件不会带上它们的样式，在入口统一引入
import 'element-plus/es/components/message/style/css'
import 'element-plus/es/components/message-box/style/css'
import './styles.css'

document.documentElement.style.setProperty('--el-color-primary', brand.primaryColor)
createApp(App).use(router).mount('#app')
