<template>
  <div class="prov-panel">
    <div class="prov-tools">
      <template v-if="view !== 'tokens'">
        <input v-model="search" class="prov-search" :placeholder="t('provSearch')" />
        <button class="chip" :class="{ active: filter === 'all' }" @click="filter = 'all'">{{ t('any') }}</button>
        <button class="chip" :class="{ active: filter === 'ok' }" @click="filter = 'ok'">{{ t('provFilterOk') }}</button>
        <button class="chip" :class="{ active: filter === 'err' }" @click="filter = 'err'">{{ t('provFilterErr') }}</button>
      </template>
      <span class="spacer"></span>
      <button class="chip" :class="{ active: view === 'rows' }" @click="setView('rows')">{{ t('provViewRows') }}</button>
      <button class="chip" :class="{ active: view === 'tokens' }" @click="setView('tokens')">{{ t('provViewTokens') }}</button>
    </div>

    <!-- Token 构成视图 -->
    <div v-if="view === 'tokens'" class="tb">
      <div class="tb-title">
        <span class="dot" style="background: var(--green); box-shadow: 0 0 8px var(--green)"></span>
        {{ t('tokenBreakdownTitle') }}
      </div>

      <div class="tb-overview">
        <div class="tb-total">
          <span class="mono">{{ fmtCompact(tbTotal.total) }}</span>
          <span class="tb-total-label">{{ t('metricTotalTokens') }}</span>
        </div>
        <div class="tb-bar" :title="tbBarTitle(tbTotal)">
          <div class="tb-seg seg-input" :style="{ width: tbPct(tbTotal.input, tbTotal.total) }"></div>
          <div class="tb-seg seg-cached" :style="{ width: tbPct(tbTotal.cached, tbTotal.total) }"></div>
          <div class="tb-seg seg-output" :style="{ width: tbPct(tbTotal.output, tbTotal.total) }"></div>
        </div>
        <div class="tb-legend">
          <span class="tb-legend-item" :title="t('tokenLegendInput', { n: fmtNum(tbTotal.input) })">
            <i class="tb-dot seg-input"></i>{{ t('tokenInput') }} <strong class="mono">{{ fmtCompact(tbTotal.input) }}</strong>
          </span>
          <span class="tb-legend-item" :title="t('tokenLegendCached', { n: fmtNum(tbTotal.cached) })">
            <i class="tb-dot seg-cached"></i>{{ t('tokenCached') }} <strong class="mono">{{ fmtCompact(tbTotal.cached) }}</strong>
          </span>
          <span class="tb-legend-item" :title="t('tokenLegendOutput', { n: fmtNum(tbTotal.output) })">
            <i class="tb-dot seg-output"></i>{{ t('tokenOutput') }} <strong class="mono">{{ fmtCompact(tbTotal.output) }}</strong>
          </span>
          <span class="tb-hit mono" :title="t('tokenHitRateTip', { pct: fmtPctNum(tbTotal.hit) })">
            {{ t('tokenHitRate') }} <strong :class="hitTone(tbTotal.hit)">{{ fmtPctNum(tbTotal.hit) }}</strong>
          </span>
        </div>
      </div>

      <div class="tb-models" v-if="tbModels.length">
        <div
          class="tb-model-row"
          v-for="m in tbModels"
          :key="m.model"
          :title="t('clickToFilter')"
          @click="emit('select-model', m.model)"
        >
          <span class="tb-model-name" :title="m.model">{{ m.model }}</span>
          <div class="tb-bar tb-bar-sm" :title="tbBarTitle(m)">
            <div class="tb-seg seg-input" :style="{ width: tbPct(m.input, m.total) }"></div>
            <div class="tb-seg seg-cached" :style="{ width: tbPct(m.cached, m.total) }"></div>
            <div class="tb-seg seg-output" :style="{ width: tbPct(m.output, m.total) }"></div>
          </div>
          <span class="tb-model-total mono" :title="fmtNum(m.total)">{{ fmtCompact(m.total) }}</span>
          <span class="tb-model-hit mono" :class="hitTone(m.hit)">{{ fmtPctNum(m.hit) }}</span>
        </div>
      </div>
    </div>

    <!-- 列表视图：el-table 固定高度，表头吸附，表体自动滚动 -->
    <el-table
      v-else-if="all.length"
      class="prov-el"
      :data="rows"
      height="320"
      size="small"
      :default-sort="{ prop: 'requests', order: 'descending' }"
      @row-click="onRowClick"
    >
      <el-table-column :label="t('colBackend')" min-width="230">
        <template #default="{ row }">
          <div class="prov-name" :title="t('clickToFilter')">
            <span class="prov-dot" :class="row.dot"></span>
            <div style="min-width: 0">
              <div class="prov-url" :title="row.url">{{ row.name }}
                <span v-for="tagv in row.tags" :key="tagv" class="tag-chip" :class="tagClass(tagv)">{{ tagv }}</span>
              </div>
              <div class="prov-models" v-if="row.model" :title="row.model">
                {{ row.model }}
                <span v-if="row.modelCount > 1"> · {{ t('provModelsN', { n: row.modelCount }) }}</span>
              </div>
            </div>
          </div>
        </template>
      </el-table-column>

      <el-table-column :label="t('provShare')" min-width="180">
        <template #default="{ row }">
          <div class="share-line" :style="{ '--pc': row.color }">
            <div class="share-bar">
              <div class="share-fill" :style="{ width: row.sharePct + '%' }"></div>
            </div>
            <span class="share-pct">{{ row.sharePct.toFixed(1) }}%</span>
          </div>
        </template>
      </el-table-column>

      <el-table-column prop="requests" :label="t('provRequests')" width="88" align="right" sortable>
        <template #default="{ row }"><span class="cell-num">{{ fmtNum(row.requests) }}</span></template>
      </el-table-column>
      <el-table-column prop="avgFirstByte" :label="t('colTtft')" width="92" align="right" sortable>
        <template #default="{ row }">
          <span class="cell-num" :class="latencyTone(row.avgFirstByte)">{{ fmtDuration(row.avgFirstByte) }}</span>
        </template>
      </el-table-column>
      <el-table-column prop="tps" :label="t('provTps')" width="92" align="right" sortable>
        <template #default="{ row }">
          <span class="cell-num" :class="{ dim: row.tps === 0 }">{{ row.tps >= 100 ? fmtNum(Math.round(row.tps)) : row.tps.toFixed(1) }}</span>
        </template>
      </el-table-column>
      <el-table-column prop="totalTokens" :label="t('provTok')" width="104" align="right" sortable>
        <template #default="{ row }"><span class="cell-num" :title="fmtNum(row.totalTokens)">{{ fmtCompact(row.totalTokens) }}</span></template>
      </el-table-column>
      <el-table-column prop="errorRate" :label="t('metricErrorRate')" width="88" align="right" sortable>
        <template #default="{ row }"><span class="cell-num" :class="errRateTone(row.errorRate)">{{ (row.errorRate * 100).toFixed(2) }}%</span></template>
      </el-table-column>
    </el-table>

    <div v-if="all.length" class="prov-total">
      <span class="tt-tag">{{ t('provTotal') }}</span>
      <span>{{ t('provRequests') }} <b>{{ fmtNum(totalReq) }}</b></span>
      <span>{{ t('provWeightedTtft') }} <b>{{ fmtDuration(weightedTtft) }}</b></span>
      <span>{{ t('provTok') }} <b>{{ fmtCompact(totalTok) }}</b></span>
      <span>{{ t('provTotalInput') }} <b>{{ fmtCompact(totalInput) }}</b></span>
      <span>{{ t('provTotalCached') }} <b>{{ fmtCompact(totalCached) }}</b></span>
      <span>{{ t('provTotalOutput') }} <b>{{ fmtCompact(totalOutput) }}</b></span>
    </div>
    <div v-else-if="view !== 'tokens'" class="empty-backend">{{ t('noBackendData') }}</div>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import { t } from '../i18n'
import {
  fmtNum, fmtCompact, fmtDuration, fmtPctNum, errRateTone, latencyTone,
  shortenBackendUrl as shorten, loadUI, saveUI,
} from '../utils'

const props = defineProps({
  items: { type: Array, default: [] },
  // 全量历史数据（无时间窗），仅供合计行使用；缺省时退回 items
  allItems: { type: Array, default: () => [] },
  windowSec: { type: Number, default: 3600 },
  // Token 构成视图数据（/api/stats）
  stats: { type: Object, default: () => ({}) },
})
const emit = defineEmits(['select', 'select-model'])

// 搜索词 / 筛选持久化：刷新后恢复（view 已单独持久化）
const _savedProv = loadUI('prov', {})
const search = ref(typeof _savedProv.search === 'string' ? _savedProv.search : '')
const filter = ref(['all', 'ok', 'err'].includes(_savedProv.filter) ? _savedProv.filter : 'all')
watch([search, filter], () => {
  saveUI('prov', { search: search.value, filter: filter.value })
})

// 视图切换（rows=自定义行 / tokens=Token 构成），持久化
const VIEW_KEY = 'provView'
const view = ref(['rows', 'tokens'].includes(localStorage.getItem(VIEW_KEY)) ? localStorage.getItem(VIEW_KEY) : 'rows')
function setView(v) {
  view.value = v
  localStorage.setItem(VIEW_KEY, v)
}

// ---- Token 构成 ----
// 固定全部请求（全生命周期）口径：总量用 lifetime_*，token_by_model 后端即全量
const tbTotal = computed(() => {
  const s = props.stats || {}
  const input = s.lifetime_input_tokens ?? 0
  const cached = s.lifetime_cached_tokens ?? 0
  const output = s.lifetime_completion_tokens ?? 0
  const total = s.lifetime_total_tokens || (input + cached + output)
  const hit = s.lifetime_cache_hit_pct ?? (cached + input > 0 ? (cached / (cached + input)) * 100 : 0)
  return { input, cached, output, total, hit }
})

const tbModels = computed(() => {
  const list = (props.stats || {}).token_by_model || []
  return list
    .map((m) => ({
      model: m.model,
      input: m.input_tokens || 0,
      cached: m.cached_tokens || 0,
      output: m.completion_tokens || 0,
      total: m.total_tokens || 0,
      hit: m.cache_hit_pct || 0,
    }))
    .filter((m) => m.total > 0)
})

function tbPct(part, sum) {
  if (!sum) return '0%'
  return (part / sum) * 100 + '%'
}

function tbBarTitle(v) {
  return `${t('tokenInput')} ${fmtNum(v.input)} · ${t('tokenCached')} ${fmtNum(v.cached)} · ${t('tokenOutput')} ${fmtNum(v.output)}`
}

// 命中率颜色分级：≥50% 绿 / 10–50% 琥珀 / <10% 灰
function hitTone(hit) {
  if (hit >= 50) return 'hit-good'
  if (hit >= 10) return 'hit-warn'
  return 'hit-low'
}

function onRowClick(row) {
  emit('select', row.url)
}

// 每个后端地址稳定分配一个调色盘颜色（按 URL 哈希，排序不漂移）
const PALETTE = ['var(--amber)', 'var(--violet)', 'var(--cyan)', 'var(--blue)', 'var(--green)', 'var(--red)']
function colorFor(url) {
  let h = 0
  for (let i = 0; i < url.length; i++) h = (h * 31 + url.charCodeAt(i)) >>> 0
  return PALETTE[h % PALETTE.length]
}

function tagClass(tag) {
  if (tag === 'fast') return 'green'
  if (tag === 'tool_call' || tag === 'embed' || tag === 'embeddings') return 'blue'
  return ''
}

const all = computed(() => {
  const list = props.items || []
  const secs = props.windowSec || 1
  const totalReqs = list.reduce((a, b) => a + (b.requests || 0), 0) || 1
  return list.map((b) => {
    const requests = b.requests || 0
    const errorRate = b.error_rate || 0
    const tps = secs > 0 ? (b.completion_tokens || 0) / secs : 0
    const models = Array.isArray(b.model) ? b.model : b.model ? [b.model] : []
    return {
      url: b.backend_url,
      name: shorten(b.backend_url),
      model: models.join(', '),
      modelCount: models.length,
      tags: b.tags || [],
      requests,
      errors: b.errors_count || 0,
      prompt: b.prompt_tokens || 0,
      cached: b.cached_prompt_tokens || 0,
      completion: b.completion_tokens || 0,
      totalTokens: b.total_tokens || 0,
      errorRate,
      avgFirstByte: b.avg_first_byte_ms || 0,
      tps,
      sharePct: (requests / totalReqs) * 100,
      dot: errorRate < 0.005 ? 'ok' : errorRate <= 0.05 ? 'wait' : 'bad',
      color: colorFor(b.backend_url),
    }
  })
})

// 过滤（排序交给 el-table 内置 sortable）
const rows = computed(() => {
  let list = all.value
  const q = search.value.trim().toLowerCase()
  if (q) list = list.filter((b) => b.name.toLowerCase().includes(q) || b.model.toLowerCase().includes(q))
  if (filter.value === 'ok') list = list.filter((b) => b.errors === 0)
  if (filter.value === 'err') list = list.filter((b) => b.errors > 0)
  return list
})

// 合计行：基于全量历史数据（allItems），缺省时退回窗口数据
const allTotal = computed(() => {
  const list = (props.allItems && props.allItems.length) ? props.allItems : (props.items || [])
  return list.reduce((acc, b) => {
    const req = b.requests || 0
    acc.requests += req
    acc.totalTokens += b.total_tokens || 0
    acc.prompt += b.prompt_tokens || 0
    acc.cached += b.cached_prompt_tokens || 0
    acc.completion += b.completion_tokens || 0
    acc.ttftWeighted += req * (b.avg_first_byte_ms || 0)
    return acc
  }, { requests: 0, totalTokens: 0, prompt: 0, cached: 0, completion: 0, ttftWeighted: 0 })
})

const totalReq = computed(() => allTotal.value.requests)
const totalTok = computed(() => allTotal.value.totalTokens)
const totalInput = computed(() => allTotal.value.prompt - allTotal.value.cached)
const totalCached = computed(() => allTotal.value.cached)
const totalOutput = computed(() => allTotal.value.completion)
const weightedTtft = computed(() => (totalReq.value ? allTotal.value.ttftWeighted / totalReq.value : 0))
</script>
