<template>
  <div class="panel">
    <div class="section-title">{{ t('requestTape') }}</div>
    <el-table
      :data="rows"
      :highlight-current-row="true"
      size="small"
      height="560"
      :row-class-name="rowClassName"
      @row-click="onRowClick"
    >
      <el-table-column :label="t('colTime')" width="150" fixed="left">
        <template #default="{ row }">
          <div class="mono">{{ row._rel }}</div>
        </template>
      </el-table-column>
      <el-table-column :label="t('colRequest')" min-width="120">
        <template #default="{ row }">
          <div class="req-line">
            <span :class="'method-badge ' + methodClass(row.method)">{{ row.method || '-' }}</span>
            <span class="mono req-path" :title="reqTitle(row)">{{ row.path || '-' }}</span>
          </div>
        </template>
      </el-table-column>
      <el-table-column :label="t('colClient')" min-width="110">
        <template #default="{ row }">
          <span
            v-if="row.client_ip"
            class="mono client-cell"
            :title="row.client_ip + ' · ' + t('clickToFilter')"
            @click.stop="onClientIPClick(row)"
          >{{ row.client_ip }}</span>
          <span v-else class="cell-subtle">-</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colUserAgent')" min-width="150">
        <template #default="{ row }">
          <span
            v-if="row.user_agent"
            class="mono ua-cell"
            :title="row.user_agent + ' · ' + t('clickToFilter')"
            @click.stop="onUserAgentClick(row)"
          >{{ row.user_agent }}</span>
          <span v-else class="cell-subtle">-</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colBackend')" min-width="150">
        <template #default="{ row }">
          <span
            v-if="row.backend_url"
            class="mono backend-cell"
            :title="row.backend_url + ' · ' + t('clickToFilter')"
            @click.stop="onBackendClick(row)"
          >{{ shortenBackendUrl(row.backend_url) }}</span>
          <span v-else class="cell-subtle">-</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colStatus')" width="64" align="center">
        <template #default="{ row }">
          <span :class="'status-tag ' + statusClass(row.status_code || 0)">
            {{ statusText(row) }}
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colModel')" min-width="140">
        <template #default="{ row }">
          <div class="model-cell">
            <span :title="row.model || ''" class="model-name">{{ row.model || '-' }}</span>
            <span v-if="row.is_streaming" class="stream-tag">{{ t('streaming') }}</span>
          </div>
        </template>
      </el-table-column>
      <el-table-column :label="t('colLatency')" min-width="120" align="right">
        <template #default="{ row }">
          <span :class="latencyTone(row.first_byte_ms)">{{ fmtLatency(row.first_byte_ms, row.total_ms, t('live')) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colTokens')" min-width="120" align="right">
        <template #default="{ row }">
          <div class="mono">{{ fmtTokens(row.prompt_tokens, row.completion_tokens) }}</div>
        </template>
      </el-table-column>
      <el-table-column :label="t('colCache')" min-width="76" align="right">
        <template #default="{ row }">
          <el-tooltip :content="cacheTip(row)" placement="top">
            <span :class="cacheTone(row.cache_hit_pct || 0)">
              <span class="cache-dot" :class="cacheDotClass(row)"></span>
              {{ fmtCache(row.cache_hit_pct || 0) }}
            </span>
          </el-tooltip>
        </template>
      </el-table-column>
      <el-table-column :label="t('colPromptPerSec')" min-width="80" align="right">
        <template #default="{ row }">
          <span :class="rateTone(row.decode_tok_per_sec || 0)">{{ fmtRate(row.decode_tok_per_sec || 0) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colActions')" width="84" fixed="right" align="center">
        <template #default="{ row }">
          <div class="row-actions">
            <el-tooltip :content="t('copyRequest')" placement="top">
              <el-button text :icon="CopyDocument" size="small" @click.stop="onCopy(row)" />
            </el-tooltip>
            <el-tooltip :content="t('viewDetail')" placement="top">
              <el-button text :icon="View" size="small" :disabled="!completed(row)" @click.stop="onRowClick(row)" />
            </el-tooltip>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <div v-if="hasMore" style="text-align: center; margin-top: 12px">
      <el-button :loading="loadingMore" @click="$emit('loadMore')">
        {{ loadingMore ? t('loading') : t('loadMore', { n: pageSize }) }}
      </el-button>
    </div>

    <el-empty v-if="items.length === 0" :description="t('emptyTitle')">
      <p>{{ t('emptyDesc') }}</p>
      <p class="empty-idle"><span class="live-dot live"></span>{{ t('emptyIdle') }}</p>
    </el-empty>
  </div>
</template>

<script setup>
import { computed } from 'vue'
import { CopyDocument, View } from '@element-plus/icons-vue'
import { t } from '../i18n'
import { fetchRaw } from '../api'
import {
  fmtNum, fmtRate, fmtPercent, fmtLatency, fmtTokens, shortQuery, shortenBackendUrl,
  isCompleted as completed, statusClass, latencyTone, rateTone, cacheTone, methodClass, copyText,
} from '../utils'

const props = defineProps({
  items: { type: Array, default: () => [] },
  hasMore: { type: Boolean, default: false },
  loadingMore: { type: Boolean, default: false },
  pageSize: { type: Number, default: 100 },
})
const emit = defineEmits(['loadMore', 'select', 'select-backend', 'select-client-ip', 'select-user-agent'])

const BREAK_MS = 30 * 60 * 1000

const rows = computed(() => {
  let prevTs = null
  return props.items.map((it) => {
    const ts = new Date(it.created_at).getTime()
    let breakLine = false
    if (prevTs !== null && Number.isFinite(ts) && Number.isFinite(prevTs)) {
      breakLine = Math.abs(ts - prevTs) > BREAK_MS
    }
    prevTs = Number.isFinite(ts) ? ts : prevTs
    return {
      ...it,
      _rel: fmtRelative(it.created_at),
      _break: breakLine,
    }
  })
})

function statusText(row) {
  return completed(row) ? String(row.status_code || 0) : t('live')
}
function fmtCache(v) {
  return `${Math.round(v || 0)}%`
}
function cacheTip(row) {
  return row.cached_prompt_tokens > 0
    ? t('cached', { n: fmtNum(row.cached_prompt_tokens) })
    : t('noCache')
}
function cacheDotClass(row) {
  return (row.cache_hit_pct || 0) > 0 ? 'hit' : 'miss'
}
function reqTitle(row) {
  return row.query ? `${row.path || ''}?${row.query}` : (row.path || '')
}
function fmtRelative(value) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}/${p(d.getMonth() + 1)}/${p(d.getDate())} ${d.getHours()}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}
function rowClassName({ row }) {
  return row._break ? 'row-break' : ''
}
function onRowClick(row) {
  if (completed(row)) emit('select', row)
}
function onBackendClick(row) {
  if (row.backend_url) emit('select-backend', row.backend_url)
}
function onClientIPClick(row) {
  if (row.client_ip) emit('select-client-ip', row.client_ip)
}
function onUserAgentClick(row) {
  if (row.user_agent) emit('select-user-agent', row.user_agent)
}
// 复制为 curl：与详情抽屉同一语义——指向代理自身的 origin（原始请求就是发给
// 代理的），已完成的请求附上原始请求体（--data-raw），便于直接重放。
async function onCopy(row) {
  const method = (row.method || 'GET').toUpperCase()
  let url = `${window.location.origin}${row.path || ''}`
  if (row.query) url += `?${row.query}`
  const esc = (s) => s.replace(/'/g, `'\\''`)
  const lines = [`curl -X ${method} '${esc(url)}'`, `  -H 'Content-Type: application/json'`]
  if (completed(row)) {
    const body = await fetchRaw(row.id, 'request')
    if (body) lines.push(`  --data-raw '${esc(body)}'`)
  }
  try {
    const ok = await copyText(lines.join(' \\\n'))
    if (ok) ElMessage.success(t('copied'))
    else ElMessage.warning(t('copyFailed'))
  } catch {
    ElMessage.warning(t('copyFailed'))
  }
}
</script>
