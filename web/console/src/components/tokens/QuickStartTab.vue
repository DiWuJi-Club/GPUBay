<script setup lang="ts">
import { formatTokens, type LLMModelPublic } from '@spark/shared'
import { computed } from 'vue'
import { copyText } from '../../logic/clipboard'
import { curlExample, DEFAULT_LLM_LIMITS, exampleModel, KEY_PLACEHOLDER, llmErrorHelp, llmErrorStatus, pythonExample } from '../../logic/tokens'

const props = defineProps<{
  baseUrl: string
  models: LLMModelPublic[]
  /** 已登录时传入账户的实际限额；未传用默认值 */
  limits?: { rpm: number; tpm: number; concurrency: number }
}>()

const model = computed(() => exampleModel(props.models))
const curl = computed(() => curlExample(props.baseUrl, model.value))
const python = computed(() => pythonExample(props.baseUrl, model.value))
const limits = computed(() => props.limits ?? DEFAULT_LLM_LIMITS)
const errors = computed(() =>
  Object.keys(llmErrorHelp)
    .map((code) => ({ code, status: llmErrorStatus[code], help: llmErrorHelp[code] }))
    .sort((a, b) => a.status - b.status),
)
</script>

<template>
  <div class="quickstart">
    <section>
      <h3>接口地址</h3>
      <p class="muted">兼容 OpenAI API：支持 <code>/chat/completions</code>、<code>/completions</code>、<code>/embeddings</code>、<code>/models</code>。</p>
      <div class="line">
        <code class="base" data-testid="tokens-quickstart-base-url">{{ baseUrl }}</code>
        <el-button size="small" data-testid="tokens-quickstart-copy-base-url" @click="copyText(baseUrl)">复制</el-button>
      </div>
      <p class="muted">
        下面的示例使用模型 <code data-testid="tokens-quickstart-model">{{ model }}</code>，把 <code>{{ KEY_PLACEHOLDER }}</code> 换成在「API Keys」里创建的 Key。
      </p>
    </section>

    <section>
      <div class="code-head">
        <h3>curl</h3>
        <el-button size="small" data-testid="tokens-quickstart-copy-curl" @click="copyText(curl)">复制</el-button>
      </div>
      <pre class="code" data-testid="tokens-quickstart-curl">{{ curl }}</pre>
    </section>

    <section>
      <div class="code-head">
        <h3>Python（openai SDK，流式）</h3>
        <el-button size="small" data-testid="tokens-quickstart-copy-python" @click="copyText(python)">复制</el-button>
      </div>
      <pre class="code" data-testid="tokens-quickstart-python">{{ python }}</pre>
    </section>

    <section>
      <h3>说明</h3>
      <ul class="notes" data-testid="tokens-quickstart-notes">
        <li data-testid="tokens-quickstart-note-usage">流式请求总会在最后附带一个 <code>usage</code> 块（choices 为空），无需自己设置 <code>stream_options</code>。</li>
        <li data-testid="tokens-quickstart-note-limits">
          限额（按账户）：每分钟 {{ limits.rpm }} 次请求、每分钟 {{ formatTokens(limits.tpm) }} token、同时 {{ limits.concurrency }} 个并发请求；超出返回 429，按响应头
          <code>Retry-After</code> 重试。单个请求体不超过 2 MiB。
        </li>
        <li data-testid="tokens-quickstart-note-billing">
          按实际 token 计费：费用累计到 ¥1 或最早一笔满 10 分钟时，从钱包结算一次，每次一条「Token 消费」流水。
        </li>
        <li data-testid="tokens-quickstart-note-debt">
          每次调用前会按余额预检；并发请求可能让余额被扣成欠费，有欠费时所有调用返回 402，充值后下一次结算自动补扣，补齐后恢复调用。
        </li>
        <li>请妥善保管 Key：明文只在创建时显示一次，泄露后请立即撤销。</li>
      </ul>
    </section>

    <section>
      <h3>错误码</h3>
      <p class="muted">错误响应体格式：<code>{"error": {"message": "…", "type": "…", "code": "…"}}</code></p>
      <el-table :data="errors" size="small" data-testid="tokens-quickstart-errors">
        <el-table-column label="HTTP" prop="status" width="72" />
        <el-table-column label="code" min-width="160">
          <template #default="{ row }"><code>{{ row.code }}</code></template>
        </el-table-column>
        <el-table-column label="说明" prop="help" min-width="240" />
      </el-table>
    </section>
  </div>
</template>

<style scoped>
.quickstart { display: grid; gap: 20px; max-width: 960px; }
h3 { margin: 0 0 8px; font-size: 15px; }
.muted { color: #606266; font-size: 13px; margin: 0 0 8px; }
.line { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-bottom: 8px; }
.base { font-size: 14px; background: #f4f4f5; padding: 4px 8px; border-radius: 4px; word-break: break-all; }
.code-head { display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px; }
.code-head h3 { margin: 0; }
.code { margin: 0; padding: 12px; background: #1f2329; color: #e6e6e6; border-radius: 6px; font-size: 13px; line-height: 1.5; overflow-x: auto; white-space: pre; }
.notes { margin: 0; padding-left: 20px; display: grid; gap: 6px; font-size: 14px; color: #303133; }
code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
</style>
