import { currentLang } from './i18n'

// ---- UI 状态持久化（localStorage，刷新后恢复用户点过的选择）----
const UI_PREFIX = 'llmproxy.ui.'
export function loadUI(key, fallback) {
  try {
    const raw = localStorage.getItem(UI_PREFIX + key)
    if (raw == null) return fallback
    const v = JSON.parse(raw)
    return v == null ? fallback : v
  } catch {
    return fallback
  }
}
export function saveUI(key, value) {
  try {
    localStorage.setItem(UI_PREFIX + key, JSON.stringify(value))
  } catch {
    /* 忽略配额/隐私模式异常 */
  }
}

export async function copyText(text) {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      /* fall through to legacy copy */
    }
  }
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.setAttribute('readonly', '')
    ta.style.position = 'fixed'
    ta.style.top = '-9999px'
    document.body.appendChild(ta)
    ta.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    return ok
  } catch {
    return false
  }
}

export function fmtMs(value) {
  if (!Number.isFinite(value) || value <= 0) return '-'
  return `${Math.round(value)} ms`
}

export function fmtMsCompact(value) {
  if (!Number.isFinite(value) || value <= 0) return '-'
  return `${Math.round(value)}ms`
}

export function fmtLatency(ttft, total, liveText) {
  const t = fmtDuration(ttft)
  let r = fmtDuration(total)
  if (r === '-' && liveText) r = String(liveText)
  if (t === '-' && r === '-') return '-'
  return `${t} / ${r}`
}

export function fmtTokens(prompt, completion) {
  return `${fmtNum(prompt)} / ${fmtNum(completion)}`
}

export function shortQuery(query, max = 40) {
  const q = String(query || '').trim()
  if (!q) return ''
  return q.length > max ? q.slice(0, max) + '…' : q
}

export function fmtRate(value) {
  if (!Number.isFinite(value) || value <= 0) return '-'
  return value.toFixed(value >= 100 ? 0 : 1)
}

export function fmtNum(value) {
  if (!Number.isFinite(value)) return '0'
  return new Intl.NumberFormat('en-US').format(value)
}

function trimTrailingZeros(n) {
  // 整数部分越长，小数位越少，避免大数字符串过长导致卡片内换行
  const intDigits = Math.abs(Math.floor(n)).toString().length
  const decimals = intDigits >= 5 ? 0 : intDigits >= 4 ? 1 : 2
  let s = n.toFixed(decimals)
  if (decimals > 0 && s.includes('.')) s = s.replace(/\.?0+$/, '')
  return s
}

// 人性化大数显示：中文用 万/亿，英文用 K/M/B；阈值以下保留千分位完整数字
export function fmtCompact(value, lang) {
  const v = Number.isFinite(value) ? Math.round(value) : 0
  if (v < 0) return fmtNum(v)
  const l = lang ?? currentLang.value
  if (l === 'zh') {
    if (v >= 1e8) return trimTrailingZeros(v / 1e8) + '亿'
    if (v >= 1e4) return trimTrailingZeros(v / 1e4) + '万'
    return fmtNum(v)
  }
  if (v >= 1e9) return trimTrailingZeros(v / 1e9) + 'B'
  if (v >= 1e6) return trimTrailingZeros(v / 1e6) + 'M'
  if (v >= 1e3) return trimTrailingZeros(v / 1e3) + 'K'
  return fmtNum(v)
}

export function fmtPercent(value) {
  if (!Number.isFinite(value)) return '0%'
  return `${(value * 100).toFixed(2)}%`
}

export function fmtPctNum(value) {
  if (!Number.isFinite(value)) return '0%'
  const n = Math.round(value * 100) / 100
  if (Number.isInteger(n)) return `${n}%`
  return `${n.toFixed(2)}%`
}

export function fmtBytes(value) {
  if (!Number.isFinite(value) || value <= 0) return '0 B'
  if (value < 1024) return `${value} B`
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`
  return `${(value / (1024 * 1024)).toFixed(2)} MB`
}

export function fmtTime(value) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value || '-'
  return d.toLocaleTimeString([], {
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  })
}

export function fmtDate(value) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value || '-'
  return d.toLocaleTimeString([], {
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  })
}

export function fmtDateTime(value) {
  const d = new Date(value)
  if (Number.isNaN(d.getTime())) return value || '-'
  const p = (x) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function todayRange() {
  const now = new Date()
  return [
    new Date(now.getFullYear(), now.getMonth(), now.getDate(), 0, 0, 0, 0),
    new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59, 999),
  ]
}

export function fmtWindowLabel(from, to) {
  const f = new Date(from)
  const t = new Date(to)
  if (Number.isNaN(f.getTime()) || Number.isNaN(t.getTime())) return '-'
  const p = (x) => String(x).padStart(2, '0')
  const time = (d) => `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
  const date = (d) => `${d.getFullYear()}/${p(d.getMonth() + 1)}/${p(d.getDate())}`
  if (f.toDateString() === t.toDateString()) {
    return `${date(f)} ${time(f)} ~ ${time(t)}`
  }
  return `${date(f)} ${time(f)} ~ ${date(t)} ${time(t)}`
}

export function fmtDuration(value) {
  if (!Number.isFinite(value) || value <= 0) return '-'
  if (value < 1000) return `${Math.round(value)}ms`
  const totalSec = value / 1000
  if (totalSec < 60) return `${totalSec.toFixed(1)}s`
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = totalSec % 60
  if (h > 0) return `${h}h${m}m`
  return `${m}m${s.toFixed(0)}s`
}

const STATUS_TEXT = {
  200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content',
  301: 'Moved Permanently', 302: 'Found', 304: 'Not Modified',
  400: 'Bad Request', 401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found',
  405: 'Method Not Allowed', 408: 'Request Timeout', 429: 'Too Many Requests',
  500: 'Internal Server Error', 501: 'Not Implemented', 502: 'Bad Gateway',
  503: 'Service Unavailable', 504: 'Gateway Timeout',
}

export function statusLabel(code) {
  return STATUS_TEXT[code] || ''
}

export function shortenId(id) {
  const s = String(id || '')
  if (s.length <= 8) return s
  return `${s.slice(0, 8)}…`
}

export function escapeHtml(value) {
  return String(value).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;')
}

export function highlightJSON(text) {
  const src = String(text ?? '')
  if (!src) return ''
  let out = ''
  let i = 0
  const n = src.length
  while (i < n) {
    const ch = src[i]
    if (ch === '"') {
      let j = i + 1
      while (j < n && src[j] !== '"') {
        if (src[j] === '\\') j++
        j++
      }
      let k = j + 1
      while (k < n && /\s/.test(src[k])) k++
      const isKey = src[k] === ':'
      out += `<span class="${isKey ? 'jk' : 'js'}">${escapeHtml(src.slice(i, j + 1))}</span>`
      i = j + 1
    } else if (/[0-9]/.test(ch) || (ch === '-' && i + 1 < n && /[0-9]/.test(src[i + 1]))) {
      let j = i
      while (j < n && /[0-9.eE+-]/.test(src[j])) j++
      out += `<span class="jn">${escapeHtml(src.slice(i, j))}</span>`
      i = j
    } else if (/[a-zA-Z_]/.test(ch)) {
      let j = i
      while (j < n && /[a-zA-Z_]/.test(src[j])) j++
      const word = src.slice(i, j)
      if (word === 'true' || word === 'false' || word === 'null') {
        out += `<span class="jn">${word}</span>`
      } else {
        out += `<span class="jb">${word}</span>`
      }
      i = j
    } else {
      out += escapeHtml(ch)
      i++
    }
  }
  return out
    .split('\n')
    .map((line, idx) => `<span class="ln">${String(idx + 1).padStart(3, ' ')}</span>${line}`)
    .join('\n')
}

export function isCompleted(item) {
  return (item.status_code || 0) > 0 || !!item.response_raw_path || !!item.error_text
}

export function statusText(item) {
  return isCompleted(item) ? String(item.status_code || 0) : 'LIVE'
}

export function statusClass(code) {
  if (code === 0) return 'status-live'
  if (code >= 500) return 'status-err'
  if (code >= 400) return 'status-mid'
  return 'status-ok'
}

export function statusBucket(code) {
  if (code === 0) return 'live'
  if (code >= 500) return 'err5xx'
  if (code >= 400) return 'err4xx'
  return 'ok'
}

export function methodClass(method) {
  const m = String(method || '').toUpperCase()
  if (m === 'GET') return 'method-get'
  if (m === 'POST') return 'method-post'
  return 'method-other'
}

// 错误率分级：<1% 绿（正常）/ 1%–5% 琥珀（警告）/ >5% 红（异常）
export function errRateTone(rate) {
  if (!Number.isFinite(rate) || rate < 0.01) return 'tone-good'
  if (rate <= 0.05) return 'tone-warm'
  return 'tone-critical'
}

export function latencyTone(ms) {
  if (!Number.isFinite(ms) || ms <= 0) return ''
  if (ms >= 20000) return 'tone-critical'
  if (ms >= 8000) return 'tone-hot'
  if (ms >= 2500) return 'tone-warm'
  return 'tone-cool'
}

export function rateTone(rate) {
  if (!Number.isFinite(rate) || rate <= 0) return ''
  if (rate < 10) return 'tone-critical'
  if (rate < 25) return 'tone-hot'
  if (rate < 45) return 'tone-warm'
  return 'tone-good'
}

export function cacheTone(pct) {
  if (!Number.isFinite(pct) || pct <= 0) return ''
  if (pct >= 50) return 'tone-good'
  if (pct >= 20) return 'tone-warm'
  return 'tone-hot'
}

export function shortenBackendUrl(url) {
  return String(url || '').replace(/^https?:\/\//, '').replace(/\/+$/, '')
}

export function buildStructuredResponse(raw, rec) {
  if (!raw) return ''
  try {
    const parsed = JSON.parse(raw)
    if (!rec?.is_streaming) {
      return JSON.stringify(parsed, null, 2)
    }
    return raw
  } catch {
    return raw
  }
}
