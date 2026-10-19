<script setup lang="ts">
import { computed } from 'vue'

/** 机器 / 实例 / 用户 ID：可点击跳详情；disabled（如财务看机器，没有机器详情权限）只显示 ID。testid 由调用方传入。 */
const props = defineProps<{ kind: 'machine' | 'instance' | 'user'; id: string | number | null | undefined; disabled?: boolean }>()
const to = computed(() => ({ machine: '/machines/', instance: '/instances/', user: '/users/' })[props.kind] + String(props.id))
</script>

<template>
  <span v-if="props.id == null || props.id === ''">—</span>
  <span v-else-if="props.disabled" class="mono">{{ props.id }}</span>
  <router-link v-else :to="to" class="mono">{{ props.id }}</router-link>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
