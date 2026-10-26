<script setup lang="ts">
import { computed } from 'vue'
import { copyText as copy } from '../../logic/clipboard'
import { connectSnippets, type ConsoleClusterView } from '../../logic/cluster'

/** 集群「连接信息」：head 地址、集群网段、cluster.env / cluster.json 说明与 torchrun / Ray 示例（每段可复制）。 */
const props = defineProps<{ view: ConsoleClusterView }>()
const snippets = computed(() => connectSnippets(props.view))
const blocks = computed(() => [
  { key: 'env', title: '集群环境变量', text: snippets.value.env },
  { key: 'torchrun', title: 'PyTorch 分布式（torchrun）', text: snippets.value.torchrun },
  { key: 'ray', title: 'Ray', text: snippets.value.ray },
])
</script>

<template>
  <div class="connect" data-testid="cluster-connect">
    <div class="row">
      <span class="muted">head（rank 0）集群地址：</span><code class="mono" data-testid="cluster-connect-head">{{ view.head_ip || '—' }}</code>
      <el-button v-if="view.head_ip" link type="primary" size="small" data-testid="cluster-connect-head-copy" @click="copy(view.head_ip)">复制</el-button>
    </div>
    <div class="row"><span class="muted">集群网段：</span><code class="mono" data-testid="cluster-connect-subnet">{{ view.subnet || '—' }}</code><span class="muted small">（容器内网卡 spc0）</span></div>
    <ul class="notes">
      <li>每个成员各有 SSH 连接（在成员实例详情里查看）；成员之间<b>没有</b>预配的 SSH 互信，分布式程序请用集群 IP 直接通信。</li>
      <li>拓扑信息在 <code>/etc/spark-cluster.json</code>；变量在 <code>/.spark/cluster.env</code>（每行 <code>export</code>，登录 shell 自动加载），<code>ssh 实例 命令</code> 这类非登录会话请先 <code>. /.spark/cluster.env</code>。</li>
    </ul>
    <div v-for="b in blocks" :key="b.key" class="block">
      <div class="block-head">
        <b>{{ b.title }}</b>
        <el-button link type="primary" size="small" :data-testid="`cluster-connect-${b.key}-copy`" @click="copy(b.text)">复制</el-button>
      </div>
      <pre class="code" :data-testid="`cluster-connect-${b.key}`">{{ b.text }}</pre>
    </div>
  </div>
</template>

<style scoped>
.row { display: flex; align-items: center; gap: 4px; flex-wrap: wrap; margin: 4px 0; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; word-break: break-all; }
.muted { color: #909399; }
.small { font-size: 12px; }
.notes { margin: 8px 0; padding-left: 18px; font-size: 13px; color: #606266; }
.notes code { word-break: break-all; }
.block { margin-top: 8px; }
.block-head { display: flex; align-items: center; justify-content: space-between; gap: 8px; font-size: 13px; }
.code { margin: 4px 0 0; padding: 8px; background: var(--el-fill-color-lighter); border-radius: 4px; white-space: pre-wrap; word-break: break-all; font-size: 12px; }
</style>
