import { defineConfig } from 'vitest/config'

// 每个工作区一个 Vitest 项目；console / admin / e2e 的配置在各自任务中加入。
export default defineConfig({
  test: {
    projects: ['shared/vitest.config.ts'],
  },
})
