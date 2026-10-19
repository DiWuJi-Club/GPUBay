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
        <li data-testid="help-migrated-web">如遇换机，SSH 密码会变化，Web 访问地址也会变为新实例 ID，请在详情页重新查看。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 data-testid="help-ssh-keys">用公钥登录、关闭密码登录</h2>
      <ol>
        <li>在「我的 → SSH 公钥」粘贴您电脑的公钥（<code>~/.ssh/id_ed25519.pub</code> 的内容，一行），之后 <code>ssh -i ~/.ssh/id_ed25519 ins-xxxxxxxx@…</code> 免密登录。</li>
        <li>添加公钥后可在实例详情关闭密码登录；在容器里执行 <code>ssh-copy-id</code> 或修改 <code>~/.ssh/authorized_keys</code> 不会生效，公钥只在控制台管理。</li>
        <li>首次连接会提示主机密钥指纹，请与本页公布的网关指纹核对：<code data-testid="help-hostkey">{{ brand.sshHostKeyFingerprint || '（部署后由运维在此填写）' }}</code>。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 data-testid="help-forward">端口转发与文件传输</h2>
      <ul>
        <li><code>scp</code> / <code>sftp</code> / <code>rsync</code> 都可用（走同一条 SSH 连接）；大文件请放到 <code>/root/data</code>。</li>
        <li>本地转发 <code>ssh -L 8888:127.0.0.1:8888 ins-xxxxxxxx@…</code> 可在本机浏览器访问容器里的服务；<code>-R</code>、<code>-A</code>（agent 转发）也支持；不支持 X11。</li>
        <li>VS Code Remote-SSH：Host 填 <code>ssh.您的域名</code>，User 填实例 ID。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-web">Web 访问（端口开放）</h2>
      <ul>
        <li>在实例详情「Web 访问」添加端口后，会得到形如 <code>https://ins-xxxxxxxx-8888.…/</code> 的网址；默认<b>私有</b>：只有您在本站登录后才能打开。</li>
        <li>设为<b>公开</b>后任何人都能访问，平台不做鉴权与限流，请确认应用自带密码；平台可在收到滥用举报后将端口限制为私有。</li>
        <li>应用需监听 <code>127.0.0.1</code> 或 <code>0.0.0.0</code>（只监听容器 eth0 地址不通）。Jupyter 请用 <code>jupyter lab --ip=127.0.0.1 --port=8888 --ServerApp.allow_remote_access=True</code>，否则会提示 Host not allowed。</li>
        <li>网址下的 <code>/.spark/</code> 路径由平台保留；应用自身的 CSRF 等安全防护由应用负责。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-booking">档期预订</h2>
      <ul>
        <li>可提前 2 小时～60 天预订，开始时间为北京时间整点；开始前 1 小时系统自动开通，开通后即可登录，开始时刻起计费。</li>
        <li>预订下单时扣款，价格按下单时锁定，之后改价不影响已付订单。</li>
        <li>取消规则：下单 15 分钟内全额退；之后距开始 24 小时以上全额退，不足 24 小时退 50%；开通（开始前 1 小时）后不能取消，只能释放（不退款）。</li>
        <li>预订的是机组，指定的机器只是优先：机器故障或仍被占用时会在同机组为您更换机器，详情页会提示。</li>
        <li>可续至：机器之后已有别人的预订时，续费最多到「下一预订开始前 8 小时」；详情页会显示可续至时间。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-batch">一次多台</h2>
      <ul>
        <li>一单最多 8 台，整单要么全部成功要么全部失败；成功后逐台管理，批次页可一键取消未开通的预订。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-auto-renew">自动续费</h2>
      <ul>
        <li>到期前 1 小时自动扣款续 1 个单位；余额不足会每分钟重试到到期，首次失败发短信提醒。</li>
        <li>被后续预订挡住时按能放下的档位办理（1 月 → 1 周 → 1 天），都放不下则失败并提示。</li>
        <li>到期停机后不再自动续费，手动续费恢复后继续生效。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-retention">数据保留规则</h2>
      <ul>
        <li>到期停机后数据保留 6 小时，期间续费可恢复运行并保留数据。</li>
        <li>超过保留期或主动释放，数据立即清除且已付费用不退。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 data-testid="help-referral">邀请返利</h2>
      <ul>
        <li>在「我的 → 邀请返利」获取您的邀请码与链接；好友通过链接注册（首次登录）即绑定，老用户不能再绑定。</li>
        <li>绑定后的返利期内（默认 12 个月），好友每笔实付租用金额（新购、续费、预订）的一定比例（默认 10%，以邀请页显示为准）返到您的余额，可用于租用，<b>不可提现、不可转让</b>。</li>
        <li>返利在扣款 7 天后到账；预订单在预订开始后到账。您完成实名认证后才能收到返利。</li>
        <li>好友退款时按比例扣回已发放的返利，余额不足的部分记为欠账，从之后的返利里抵扣。</li>
        <li>单个好友累计返利与每 30 天的发放都有上限（以邀请页显示为准），超出部分不计。</li>
        <li>禁止自刷：同一设备、同一身份证 / 营业执照注册的账号互相邀请会被搁置或撤销；平台有权撤销异常返利并冻结邀请资格。</li>
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
code { word-break: break-all; }
</style>
