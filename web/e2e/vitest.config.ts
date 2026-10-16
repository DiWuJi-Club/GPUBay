import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: { name: 'e2e-helpers', environment: 'node', include: ['__tests__/*.spec.ts'] },
})
