<script setup lang="ts">
import { fmtTime, instanceStateLabel, instanceStateType, labelOf, useMobile } from '@spark/shared'
import { netStatusText, type ConsoleClusterMember } from '../../logic/cluster'

/** 集群成员表：rank、实例（链接）、集群 IP、实例状态、集群网络状态、机器；手机为卡片。 */
defineProps<{ members: ConsoleClusterMember[]; size: number; showExpires?: boolean }>()
const mobile = useMobile()
const role = (rank: number) => (rank === 0 ? 'head' : 'worker')
</script>

<template>
  <div class="members" data-testid="cluster-members">
    <div v-if="mobile" class="cards">
      <div v-for="m in members" :key="m.rank" class="member" data-testid="cluster-member" :data-rank="m.rank" :data-instance="m.instance_id">
        <div class="line">
          <b>rank {{ m.rank }}</b><span class="muted">{{ role(m.rank) }}</span>
          <router-link :to="`/instances/${m.instance_id}`" class="mono" data-testid="cluster-member-instance">{{ m.instance_id }}</router-link>
        </div>
        <div class="line">
          <span class="muted">集群 IP</span><code class="mono" data-testid="cluster-member-ip">{{ m.ip || '—' }}</code>
        </div>
        <div class="line">
          <el-tag size="small" :type="labelOf(instanceStateType, m.state) as 'primary'" data-testid="cluster-member-state">{{ labelOf(instanceStateLabel, m.state) }}</el-tag>
          <el-tag size="small" :type="netStatusText(m.net_status, m.net_reason).type as 'primary'" data-testid="cluster-member-net">网络{{ netStatusText(m.net_status, m.net_reason).text }}</el-tag>
          <el-tag v-if="m.suspended" size="small" type="warning" data-testid="cluster-member-suspended">计时暂停</el-tag>
        </div>
        <div class="line muted"><span data-testid="cluster-member-machine">{{ m.machine_label || '—' }}</span><template v-if="showExpires && m.expires_at"> · 到期 {{ fmtTime(m.expires_at) }}</template></div>
      </div>
    </div>
    <el-table v-else :data="members" row-key="rank" size="small">
      <el-table-column label="rank" width="90">
        <template #default="{ row }"><span data-testid="cluster-member" :data-rank="row.rank" :data-instance="row.instance_id">{{ row.rank }} <span class="muted">{{ role(row.rank) }}</span></span></template>
      </el-table-column>
      <el-table-column label="实例" min-width="140">
        <template #default="{ row }"><router-link :to="`/instances/${row.instance_id}`" class="mono" data-testid="cluster-member-instance">{{ row.instance_id }}</router-link></template>
      </el-table-column>
      <el-table-column label="集群 IP" min-width="120">
        <template #default="{ row }"><code class="mono" data-testid="cluster-member-ip">{{ row.ip || '—' }}</code></template>
      </el-table-column>
      <el-table-column label="状态" width="120">
        <template #default="{ row }">
          <el-tag size="small" :type="labelOf(instanceStateType, row.state) as 'primary'" data-testid="cluster-member-state">{{ labelOf(instanceStateLabel, row.state) }}</el-tag>
          <el-tag v-if="row.suspended" size="small" type="warning" data-testid="cluster-member-suspended">计时暂停</el-tag>
        </template>
      </el-table-column>
      <el-table-column label="集群网络" min-width="130">
        <template #default="{ row }"><el-tag size="small" :type="netStatusText(row.net_status, row.net_reason).type as 'primary'" data-testid="cluster-member-net">{{ netStatusText(row.net_status, row.net_reason).text }}</el-tag></template>
      </el-table-column>
      <el-table-column label="机器" min-width="110">
        <template #default="{ row }">
          <span data-testid="cluster-member-machine">{{ row.machine_label || '—' }}</span>
          <div v-if="showExpires && row.expires_at" class="muted small">到期 {{ fmtTime(row.expires_at) }}</div>
        </template>
      </el-table-column>
    </el-table>
    <p v-if="members.length < size" class="warn small" data-testid="cluster-members-missing">在位成员 {{ members.length }} / {{ size }} 台：有成员更换机器中或已退款释放</p>
  </div>
</template>

<style scoped>
.cards { display: grid; gap: 8px; }
.member { border: 1px solid var(--el-border-color-lighter); border-radius: 6px; padding: 8px 10px; }
.line { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin: 2px 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.muted { color: #909399; font-size: 12px; }
.small { font-size: 12px; margin: 4px 0; }
.warn { color: var(--el-color-warning); }
</style>
