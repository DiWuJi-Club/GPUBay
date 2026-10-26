<script setup lang="ts">
import { instanceStateLabel, instanceStateType, labelOf, type ClusterStatus } from '@spark/shared'
import { useRouter } from 'vue-router'
import { clusterStatusBadge, type ClusterInstance } from '../../logic/cluster'

/** 实例列表里折叠的一张集群卡：「集群 clu-xxxx · N 台 · 状态」+ 成员行；点击卡片进集群页（成员 ID 链接进实例详情）。 */
type MemberRow = ClusterInstance & { timeText: string; remaining: number }
const props = defineProps<{ cluster: { id: string; size: number; status: ClusterStatus }; members: MemberRow[] }>()
const router = useRouter()
const go = () => void router.push(`/clusters/${props.cluster.id}`)
</script>

<template>
  <el-card class="cluster-card" shadow="hover" data-testid="instances-cluster-card" :data-id="cluster.id" :data-status="cluster.status" @click="go">
    <div class="head">
      <router-link :to="`/clusters/${cluster.id}`" class="title" data-testid="instances-cluster-link" @click.stop>集群 <span class="mono">{{ cluster.id }}</span> · {{ cluster.size }} 台</router-link>
      <el-tag :type="clusterStatusBadge(cluster).type as 'primary'" data-testid="instances-cluster-status">{{ clusterStatusBadge(cluster).text }}</el-tag>
    </div>
    <div v-for="m in members" :key="m.id" class="member" data-testid="instances-cluster-member" :data-id="m.id" :data-rank="m.cluster?.rank">
      <span class="rank">rank {{ m.cluster?.rank }}</span>
      <router-link :to="`/instances/${m.id}`" class="mono" data-testid="instances-cluster-member-id" @click.stop>{{ m.id }}</router-link>
      <el-tag size="small" :type="labelOf(instanceStateType, m.state) as 'primary'" data-testid="instances-cluster-member-state">{{ labelOf(instanceStateLabel, m.state) }}</el-tag>
      <span class="muted" data-testid="instances-cluster-member-time" :data-remaining="m.remaining">{{ m.timeText }}</span>
      <span v-if="m.suspended" class="warn">计时已暂停</span>
    </div>
  </el-card>
</template>

<style scoped>
.cluster-card { cursor: pointer; }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; margin-bottom: 6px; flex-wrap: wrap; }
.title { font-weight: 600; color: inherit; text-decoration: none; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.member { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; padding: 4px 0; border-top: 1px solid var(--el-border-color-lighter); font-size: 13px; }
.rank { color: #909399; min-width: 48px; }
.muted { color: #909399; }
.warn { color: var(--el-color-warning); font-size: 12px; }
</style>
