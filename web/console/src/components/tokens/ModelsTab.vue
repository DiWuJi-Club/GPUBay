<script setup lang="ts">
import { formatPricePerM, labelOf, llmModelKindLabel, type LLMModelPublic } from '@spark/shared'
import { copyText } from '../../logic/clipboard'
import { contextLabel } from '../../logic/tokens'

defineProps<{ models: LLMModelPublic[] }>()
</script>

<template>
  <div class="models">
    <p class="intro" data-testid="tokens-models-intro">按实际 token 计费，从钱包余额扣除，每次结算一条流水。价格单位：元 / 百万 token。</p>
    <el-empty v-if="models.length === 0" description="暂无上线的模型" data-testid="tokens-models-empty" />
    <div v-else class="cards">
      <el-card v-for="m in models" :key="m.slug" class="card" :data-testid="`tokens-model-card-${m.slug}`">
        <template #header>
          <div class="head">
            <span class="name" data-testid="tokens-model-name">{{ m.name }}</span>
            <el-tag :type="m.available ? 'success' : 'info'" size="small" data-testid="tokens-model-available">{{ m.available ? '可用' : '暂无节点' }}</el-tag>
          </div>
        </template>
        <div class="slug">
          <code data-testid="tokens-model-slug">{{ m.slug }}</code>
          <el-button link type="primary" size="small" data-testid="tokens-model-copy" @click="copyText(m.slug)">复制</el-button>
        </div>
        <p v-if="m.description" class="desc">{{ m.description }}</p>
        <ul class="meta">
          <li><span class="label">类型</span>{{ labelOf(llmModelKindLabel, m.kind) }}</li>
          <li><span class="label">上下文</span><span data-testid="tokens-model-context">{{ contextLabel(m.context_len) }}</span></li>
        </ul>
        <div v-if="m.features.length" class="features">
          <el-tag v-for="f in m.features" :key="f" size="small" effect="plain" data-testid="tokens-model-feature">{{ f }}</el-tag>
        </div>
        <div class="prices">
          <div>输入 <b data-testid="tokens-model-input-price">{{ formatPricePerM(m.input_price) }}</b> / 百万 token</div>
          <!-- 向量模型只按输入计费 -->
          <div v-if="m.kind !== 'embedding'">输出 <b data-testid="tokens-model-output-price">{{ formatPricePerM(m.output_price) }}</b> / 百万 token</div>
        </div>
      </el-card>
    </div>
  </div>
</template>

<style scoped>
.intro { color: #606266; margin: 0 0 12px; font-size: 14px; }
.cards { display: grid; gap: 16px; grid-template-columns: repeat(auto-fill, minmax(280px, 1fr)); }
.head { display: flex; justify-content: space-between; align-items: center; gap: 8px; }
.name { font-weight: 600; }
.slug { display: flex; align-items: center; gap: 8px; margin-bottom: 8px; }
.slug code { background: #f4f4f5; padding: 2px 6px; border-radius: 4px; word-break: break-all; }
.desc { color: #606266; font-size: 13px; margin: 0 0 8px; }
.meta { list-style: none; padding: 0; margin: 0 0 8px; display: grid; gap: 4px; font-size: 14px; }
.meta .label { display: inline-block; width: 56px; color: #909399; }
.features { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 8px; }
.prices { display: grid; gap: 4px; font-size: 14px; }
</style>
