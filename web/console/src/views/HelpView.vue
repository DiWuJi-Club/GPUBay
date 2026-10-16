<script setup lang="ts">
import { brand } from '@spark/shared'
</script>

<template>
  <div class="help" data-testid="help-page">
    <el-card>
      <h2 data-testid="help-ssh">怎么用 SSH 连接</h2>
      <ol>
        <li>实例状态变为「运行中」后，在实例详情页复制 SSH 命令（形如 <code>ssh ins-xxxxxxxx@ssh.您的域名</code>）。</li>
        <li>点击「显示」查看 SSH 密码并复制；密码只在您点击时才会显示，离开页面后需要重新查看。</li>
        <li>如遇换机，SSH 密码会变化，请在详情页重新查看。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 data-testid="help-retention">数据保留规则</h2>
      <ul>
        <li>到期停机后数据保留 6 小时，期间续费可恢复运行并保留数据。</li>
        <li>超过保留期或主动释放，数据立即清除且已付费用不退。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-billing">计费规则</h2>
      <ul>
        <li>预付包时长：1 天 / 1 周 / 1 月（按 30 天计），从开通成功开始计时。</li>
        <li>机器故障期间暂停计时，恢复后自动补回暂停的时长。</li>
        <li>停机期间续费，时长在重新启动成功后开始计算。</li>
      </ul>
      <p class="support">{{ brand.supportText }}</p>
    </el-card>
  </div>
</template>

<style scoped>
.help { display: grid; gap: 16px; max-width: 760px; }
h2 { font-size: 16px; margin: 0 0 8px; }
.support { color: #909399; font-size: 12px; }
</style>
