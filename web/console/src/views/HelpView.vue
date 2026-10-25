<script setup lang="ts">
import { brand, formatTokens } from '@spark/shared'
import { DEFAULT_LLM_LIMITS, llmErrorHelp, llmErrorStatus } from '../logic/tokens'

// Token 商城的常见错误码：与快速开始同一份说明（llmErrorHelp），按 HTTP 状态码排序
const llmErrors = Object.keys(llmErrorHelp)
  .map((code) => ({ code, status: llmErrorStatus[code], help: llmErrorHelp[code] }))
  .sort((a, b) => a.status - b.status)
</script>

<template>
  <div class="help" data-testid="help-page">
    <el-card>
      <h2 id="ssh" data-testid="help-ssh">怎么用 SSH 连接</h2>
      <ol>
        <li>实例状态变为「运行中」后，在实例详情页复制 SSH 命令（形如 <code>ssh ins-xxxxxxxx@ssh.您的域名</code>）。</li>
        <li>点击「显示」查看 SSH 密码并复制；密码只在您点击时才会显示，离开页面后需要重新查看。</li>
        <li data-testid="help-migrated-web">如遇换机，SSH 密码会变化，Web 访问地址也会变为新实例 ID，请在详情页重新查看。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 id="ssh-keys" data-testid="help-ssh-keys">用公钥登录、关闭密码登录</h2>
      <ol>
        <li>在「我的 → SSH 公钥」粘贴您电脑的公钥（<code>~/.ssh/id_ed25519.pub</code> 的内容，一行），之后 <code>ssh -i ~/.ssh/id_ed25519 ins-xxxxxxxx@…</code> 免密登录。</li>
        <li>添加公钥后可在实例详情关闭密码登录；在容器里执行 <code>ssh-copy-id</code> 或修改 <code>~/.ssh/authorized_keys</code> 不会生效，公钥只在控制台管理。</li>
        <li>首次连接会提示主机密钥指纹，请与本页公布的网关指纹核对：<code data-testid="help-hostkey">{{ brand.sshHostKeyFingerprint || '（部署后由运维在此填写）' }}</code>。</li>
      </ol>
    </el-card>
    <el-card>
      <h2 id="forward" data-testid="help-forward">端口转发与文件传输</h2>
      <ul>
        <li><code>scp</code> / <code>sftp</code> / <code>rsync</code> 都可用（走同一条 SSH 连接）；大文件请放到 <code>/root/data</code>。</li>
        <li>本地转发 <code>ssh -L 8888:127.0.0.1:8888 ins-xxxxxxxx@…</code> 可在本机浏览器访问容器里的服务；<code>-R</code>、<code>-A</code>（agent 转发）也支持；不支持 X11。</li>
        <li>VS Code Remote-SSH：Host 填 <code>ssh.您的域名</code>，User 填实例 ID。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="web" data-testid="help-web">Web 访问（端口开放）</h2>
      <ul>
        <li>在实例详情「Web 访问」添加端口后，会得到形如 <code>https://ins-xxxxxxxx-8888.…/</code> 的网址；默认<b>私有</b>：只有您在本站登录后才能打开。</li>
        <li>设为<b>公开</b>后任何人都能访问，平台不做鉴权与限流，请确认应用自带密码；平台可在收到滥用举报后将端口限制为私有。</li>
        <li>应用需监听 <code>127.0.0.1</code> 或 <code>0.0.0.0</code>（只监听容器 eth0 地址不通）。Jupyter 请用 <code>jupyter lab --ip=127.0.0.1 --port=8888 --ServerApp.allow_remote_access=True</code>，否则会提示 Host not allowed。</li>
        <li>网址下的 <code>/.spark/</code> 路径由平台保留；应用自身的 CSRF 等安全防护由应用负责。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="apps" data-testid="help-apps">一键部署</h2>
      <ul>
        <li>在算力市场的「一键部署」或实例详情的「应用」卡片里选择应用（vLLM、Open WebUI、ComfyUI、JupyterLab），平台会在您的实例里把它装好并启动。应用以进程形式运行在实例里，不另收费。</li>
        <li>首次部署需要下载模型，速度受实例出网限速影响（16GB 约 15 分钟）；进度在「应用」卡片里显示，可随时「查看日志」。</li>
        <li>模型缓存在 <code>/root/data/models</code>，卸载应用不会删除，重新部署同一模型无需再下载；不需要时可 SSH 进去用 <code>rm -rf /root/data/models/…</code> 清理以腾出数据盘空间。</li>
        <li>应用端口会自动开放为<b>私有</b>网址：点「打开」即可访问，只有您登录控制台后能打开；如需设为公开，请先确认应用自带密码。</li>
        <li>Open WebUI：<b>第一个访问者会成为管理员</b>，部署完成后请先自己打开一次并注册。</li>
        <li>vLLM：<code>API_KEY</code> 留空时接口不鉴权（私有网址仍受控制台登录保护）；设为公开前请先设置接口密钥，调用时带上 <code>Authorization: Bearer &lt;密钥&gt;</code>。</li>
        <li>JupyterLab：访问令牌必填，可点「生成」得到 32 位随机值；令牌只在填写时显示一次，请复制保存。</li>
        <li>实例到期停机后应用随之停止，续费恢复运行后会自动重新启动；停机期间不能重新部署、停止或启动。</li>
        <li>如遇换机，应用会在新实例上自动重新部署，但模型需要重新下载。</li>
        <li>想自己改配置可以 SSH 进去查看 <code>/root/data/.spark-apps/&lt;应用&gt;/</code>（<code>app.sh</code>、<code>env</code>、<code>log</code>），但「重新部署」会覆盖 <code>app.sh</code> 与 <code>env</code>。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="workflows" data-testid="help-workflows">工作流市场</h2>
      <ul>
        <li>在「工作流市场」挑选 ComfyUI 工作流，点「一键安装」装到您实例上的 ComfyUI；还没有 ComfyUI 时可「新开一台实例」，开通时一并预装 ComfyUI 与所选工作流（每个 ComfyUI 最多 8 个）。</li>
        <li>安装 = 在实例里下载该工作流需要的模型与自定义节点，再<b>重启 ComfyUI</b>：正在生成的任务会中断；下载量见工作流卡片（「需下载约 X GB」），速度受实例出网限速影响。</li>
        <li>文件位置：模型在 <code>/root/data/comfyui/models</code>，自定义节点在 <code>/root/data/comfyui/custom_nodes</code>，工作流文件在 <code>/root/data/comfyui/user</code>（ComfyUI 左侧「工作流」里以 <code>spark-</code> 开头）；一键部署时选的预置模型缓存在 <code>/root/data/models</code>（软链进 ComfyUI 模型目录）。</li>
        <li>移除工作流只删工作流文件，<b>不删除模型与节点</b>（其它工作流可能也在用）；不需要时可 SSH 进去手动清理以腾出数据盘空间。</li>
        <li>支持「快速出图」的工作流可在实例详情直接填参数出图；ComfyUI 的出图结果（含快速出图）保存在 <code>/root/data/comfyui-output</code>。</li>
        <li>想自己装节点：SSH 进去放到 <code>/root/data/comfyui/custom_nodes/</code>，重新部署不会删除它，但平台不负责其兼容性。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="booking" data-testid="help-booking">档期预订</h2>
      <ul>
        <li>可提前 2 小时～60 天预订，开始时间为北京时间整点；开始前 1 小时系统自动开通，开通后即可登录，开始时刻起计费。</li>
        <li>预订下单时扣款，价格按下单时锁定，之后改价不影响已付订单。</li>
        <li>取消规则：下单 15 分钟内全额退；之后距开始 24 小时以上全额退，不足 24 小时退 50%；开通（开始前 1 小时）后不能取消，只能释放（不退款）。</li>
        <li>预订的是机组，指定的机器只是优先：机器故障或仍被占用时会在同机组为您更换机器，详情页会提示。</li>
        <li>可续至：机器之后已有别人的预订时，续费最多到「下一预订开始前 8 小时」；详情页会显示可续至时间。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="batch" data-testid="help-batch">一次多台</h2>
      <ul>
        <li>一单最多 8 台，整单要么全部成功要么全部失败；成功后逐台管理，批次页可一键取消未开通的预订。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="auto-renew" data-testid="help-auto-renew">自动续费</h2>
      <ul>
        <li>到期前 1 小时自动扣款续 1 个单位；余额不足会每分钟重试到到期，首次失败发短信提醒。</li>
        <li>被后续预订挡住时按能放下的档位办理（1 月 → 1 周 → 1 天），都放不下则失败并提示。</li>
        <li>到期停机后不再自动续费，手动续费恢复后继续生效。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="retention" data-testid="help-retention">数据保留规则</h2>
      <ul>
        <li>到期停机后数据保留 6 小时，期间续费可恢复运行并保留数据。</li>
        <li>超过保留期或主动释放，数据立即清除且已付费用不退。</li>
      </ul>
    </el-card>
    <el-card>
      <h2 id="tokens" data-testid="help-tokens">Token 商城</h2>
      <ul>
        <li data-testid="help-tokens-billing">计费：按实际 token 数计费，价格以「元 / 百万 token」标示，输入（提示词）与输出（生成内容）分别计价，向量模型只计输入；请求未指定 max_tokens 时按模型的最大输出长度处理。上游错误、上游超时（未输出内容时）、无可用节点、服务重启中断的请求不收费；流式输出已返回部分内容后因 10 分钟总时长上限被截断的，按已返回的内容计费。</li>
        <li data-testid="help-tokens-settle">结算：费用先累计，累计到 ¥1 或最早一笔满 10 分钟时从钱包余额扣款一次，每次一条「Token 消费」流水；在「Token 商城 → 用量」可查看结算记录并跳转到对应流水。</li>
        <li data-testid="help-tokens-limits">
          限额（按账户，所有 Key 共享）：默认每分钟 {{ DEFAULT_LLM_LIMITS.rpm }} 次请求、每分钟 {{ formatTokens(DEFAULT_LLM_LIMITS.tpm) }} token、同时 {{ DEFAULT_LLM_LIMITS.concurrency }} 个并发请求，超出返回 429；需要更高限额请联系客服。每个 Key 还可以设置月消费上限。
        </li>
        <li data-testid="help-tokens-key">Key 安全：明文 Key 只在创建时显示一次，平台只保存哈希、无法找回；请勿写进前端代码或公开仓库，一旦泄露请立即在「API Keys」撤销并重新创建。</li>
        <li data-testid="help-tokens-debt">欠费：每次调用前按余额预检，但并发请求可能让实际费用超过余额，差额记为欠费；有欠费时所有调用返回 402，充值后下一次结算自动补扣，补齐后恢复调用。</li>
      </ul>
      <h3>常见错误码</h3>
      <el-table :data="llmErrors" size="small" data-testid="help-tokens-errors">
        <el-table-column label="HTTP" prop="status" width="64" />
        <el-table-column label="code" min-width="150">
          <template #default="{ row }"><code>{{ row.code }}</code></template>
        </el-table-column>
        <el-table-column label="说明" prop="help" min-width="220" />
      </el-table>
    </el-card>
    <el-card>
      <h2 id="referral" data-testid="help-referral">邀请返利</h2>
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
      <h2 id="billing" data-testid="help-billing">计费规则</h2>
      <ul>
        <li>预付包时长：1 天 / 1 周 / 1 月（按 30 天计），从开通成功开始计时。</li>
        <li>机器故障期间暂停计时，恢复后自动补回暂停的时长。</li>
        <li>停机期间续费，时长在重新启动成功后开始计算。</li>
      </ul>
      <p class="support">{{ brand.supportText }}</p>
    </el-card>
    <el-card>
      <!-- 与 internal/butler/help/butler.md（get_help 的语料）同文，两边手工保持一致 -->
      <h2 id="butler" data-testid="help-butler">AI 管家</h2>
      <ul>
        <li><b>能做什么</b>：帮您挑选套餐和可租时段、租用或续费实例、取消预订、释放实例；在实例上部署 / 重新部署 / 停止 / 启动 / 卸载应用；读取部署日志与事件帮您排查问题；查询余额、订单与计费规则，回答帮助页里的问题。</li>
        <li><b>不能做什么</b>：查看或修改 SSH 密码、管理 SSH 公钥、开放 / 公开 / 删除端口、已有实例的自动续费开关、充值、实名认证、邀请返利相关操作，以及创建或查看 Token 商城的 API Key。遇到这些需求，管家会告诉您在控制台的哪个页面自己操作。</li>
        <li><b>任何花钱或删除的操作都会先出确认卡</b>：卡片上的内容与金额由平台根据真实参数生成，您点「确认」才会执行，不点就什么都不会发生；释放实例、卸载应用、取消预订等危险操作还需要勾选确认。确认卡 10 分钟内有效。</li>
        <li><b>对话保存 90 天</b>：客服在排障时可能查看对话内容；管家生成的密钥与应用的敏感变量不会以明文保存。</li>
        <li><b>每日额度</b>：每个账号每天有一定的 AI 管家使用额度，用完后次日恢复。</li>
        <li><b>价格以下单页为准</b>：管家回答里的价格来自实时查询，但最终扣款以确认卡与下单页显示的金额为准。</li>
      </ul>
    </el-card>
  </div>
</template>

<style scoped>
.help { display: grid; gap: 16px; max-width: 760px; }
h2 { font-size: 16px; margin: 0 0 8px; }
h3 { font-size: 14px; margin: 12px 0 8px; }
.support { color: #909399; font-size: 12px; }
code { word-break: break-all; }
</style>
