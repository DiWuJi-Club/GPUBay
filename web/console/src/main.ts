import { brand } from '@spark/shared'
import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import './styles.css'

document.documentElement.style.setProperty('--el-color-primary', brand.primaryColor)
createApp(App).use(router).mount('#app')
