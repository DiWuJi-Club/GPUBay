<script setup lang="ts">
/** 批次号：能看订单的角色（财务 / 超管）点击跳到按该批次筛选的订单页；运维没有订单页权限，只显示批次号。testid 由调用方传入。 */
const props = defineProps<{ id: string | null | undefined; disabled?: boolean }>()
</script>

<template>
  <span v-if="!props.id">—</span>
  <span v-else-if="props.disabled" class="mono">{{ props.id }}</span>
  <router-link v-else :to="{ path: '/orders', query: { batch_id: props.id } }" class="mono">{{ props.id }}</router-link>
</template>

<style scoped>
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
