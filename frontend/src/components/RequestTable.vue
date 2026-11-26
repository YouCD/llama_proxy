<template>
  <div class="log-panel">
    <slot name="top" />

    <el-table
      v-if="rows.length"
      :data="rows"
      height="408"
      size="small"
      :row-class-name="rowClassName"
      @row-click="onRowClick"
    >
      <el-table-column :label="t('colTime')" width="92">
        <template #default="{ row }">
          <span class="lg-time" :title="fmtDateTime(row.created_at)">{{ row._time }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colRequest')" min-width="150">
        <template #default="{ row }">
          <span class="lg-path" :title="row.path + (row.query ? '?' + row.query : '') + ' · ' + t('clickToFilter')" @click.stop="onPathClick(row)">
            {{ row.path || '-' }}
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colModel')" min-width="130">
        <template #default="{ row }">
          <span class="lg-model" :title="(row.model || '-') + ' · ' + t('clickToFilter')" @click.stop="onModelClick(row)">
            <span class="lg-model-txt">{{ row.model || '-' }}</span>
            <span v-if="row.is_streaming" class="stream-dot" style="margin-left: 6px"><i></i></span>
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colBackend')" min-width="150" class-name="lg-hide-narrow">
        <template #default="{ row }">
          <span class="lg-backend" :title="(row.backend_url || '-') + ' · ' + t('clickToFilter')" @click.stop="onBackendClick(row)">
            <span class="lg-backend-txt">{{ row.backend_url ? shortenBackendUrl(row.backend_url) : '-' }}</span>
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colClient')" width="130" class-name="lg-hide-narrow">
        <template #default="{ row }">
          <span class="lg-ip" :title="(row.client_ip || '-') + ' · ' + t('clickToFilter')" @click.stop="onClientIPClick(row)">
            {{ row.client_ip || '-' }}
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colTtft')" width="84" align="center">
        <template #default="{ row }">
          <span :class="latencyTone(row.first_byte_ms)">{{ fmtDuration(row.first_byte_ms) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colStatus')" width="84" align="center">
        <template #default="{ row }">
          <span v-if="isCompleted(row)" class="st-pill" :class="statusPill(row.status_code)">{{ row.status_code }}</span>
          <span v-else class="stream-dot"><i></i>{{ t('live') }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colTokens')" width="110" align="right" class-name="lg-hide-narrow">
        <template #default="{ row }">
          <span :title="t('colTokens')">{{ fmtNum(row.prompt_tokens || 0) }}/{{ fmtNum(row.completion_tokens || 0) }}</span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colCache')" width="70" align="right" class-name="lg-hide-narrow">
        <template #default="{ row }">
          <span :title="row.cached_prompt_tokens > 0 ? t('cached', { n: fmtNum(row.cached_prompt_tokens) }) : t('noCache')">
            {{ row.cache_hit_pct ? Math.round(row.cache_hit_pct) + '%' : '·' }}
          </span>
        </template>
      </el-table-column>
      <el-table-column :label="t('colActions')" width="80" align="center">
        <template #default="{ row }">
          <div class="row-actions">
            <button class="ra-btn" :disabled="copyingId === row.id" :title="t('copyCurlTip')" @click.stop="onCopyCurl(row)"><el-icon><CopyDocument /></el-icon></button>
            <button class="ra-btn" :title="t('viewDetail')" @click.stop="onView(row)"><el-icon><View /></el-icon></button>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <div v-else class="log-empty">
      {{ t('emptyTitle') }}
      <p class="empty-idle"><span class="live-dot live"></span>{{ t('emptyIdle') }}</p>
    </div>

    <div v-if="total > 0" class="table-pager">
      <el-pagination
        background
        layout="total, sizes, prev, pager, next, jumper"
        :total="total"
        :current-page="page"
        :page-size="pageSize"
        :page-sizes="[50, 100, 200, 500]"
        @current-change="$emit('page-change', $event)"
        @size-change="$emit('size-change', $event)"
      />
    </div>
  </div>
</template>

<script setup>
import { computed, ref, watch } from 'vue'
import { CopyDocument, View } from '@element-plus/icons-vue'
import { t } from '../i18n'
import {
  fmtNum, fmtDuration, fmtDateTime, shortenBackendUrl,
  isCompleted, latencyTone, copyText,
} from '../utils'

const props = defineProps({
  items: { type: Array, default: () => [] },
  total: { type: Number, default: 0 },
  page: { type: Number, default: 1 },
  pageSize: { type: Number, default: 100 },
})
const emit = defineEmits(['page-change', 'size-change', 'select', 'select-backend', 'select-client-ip', 'select-user-agent', 'select-path', 'select-model', 'toast'])

function fmtTime(value) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return ''
  const p = (x) => String(x).padStart(2, '0')
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

const rows = computed(() =>
  props.items.map((it) => ({ ...it, _time: fmtTime(it.created_at) }))
)

function statusPill(code) {
  if (!code) return 'st-200'
  if (code >= 500) return 'st-5xx'
  if (code >= 400) return 'st-4xx'
  return 'st-200'
}

// 新行入场动画：记录上一轮已见过的 id，本轮新出现的才加 new 类
const known = ref(new Set())
const fresh = ref(new Set())
let firstWatch = true
watch(
  () => props.items,
  (list) => {
    const cur = new Set((list || []).map((it) => it.id))
    if (firstWatch) {
      firstWatch = false
      known.value = cur
      return
    }
    fresh.value = new Set([...cur].filter((id) => !known.value.has(id)))
    known.value = cur
  },
  { immediate: true },
)
function isNew(id) {
  return fresh.value.has(id)
}

// el-table 行类名：新行入场动画（与旧 .log-row.new 同一 keyframes）
function rowClassName({ row }) {
  return isNew(row.id) ? 'new' : ''
}

function onRowClick(row) {
  if (isCompleted(row)) emit('select', row)
}
function onBackendClick(row) {
  if (row.backend_url) emit('select-backend', row.backend_url)
}
function onPathClick(row) {
  if (row.path) emit('select-path', row.path)
}
function onModelClick(row) {
  if (row.model) emit('select-model', row.model)
}
function onClientIPClick(row) {
  if (row.client_ip) emit('select-client-ip', row.client_ip)
}

// ---- 操作列：复制 curl / 查看详情 ----
const copyingId = ref(null)

function buildCurl(row, body) {
  const url = window.location.origin + (row.path || '') + (row.query ? '?' + row.query : '')
  const lines = [`curl -X ${row.method || 'POST'} '${url}'`, `  -H 'Content-Type: application/json'`]
  if (row.user_agent) lines.push(`  -H 'User-Agent: ${row.user_agent}'`)
  if (body) lines.push(`  -d '${body.replace(/'/g, "'\\''")}'`)
  return lines.map((l, i) => (i < lines.length - 1 ? l + ' \\' : l)).join('\n')
}

async function onCopyCurl(row) {
  if (copyingId.value) return
  copyingId.value = row.id
  try {
    let body = ''
    try {
      const res = await fetch(`/_proxy/raw/${row.id}/request`)
      if (res.ok) body = await res.text()
    } catch { /* 原始报文不可用时仍复制不带 body 的 curl */ }
    const ok = await copyText(buildCurl(row, body))
    emit('toast', { msg: ok ? t('copied') : t('copyFailed'), icon: ok ? '✓' : '⚠', tc: ok ? 'var(--ok)' : 'var(--bad)' })
  } finally {
    copyingId.value = null
  }
}

function onView(row) {
  emit('select', row)
}
</script>
