async function fetchJSON(url, options) {
  const resp = await fetch(url, options)
  if (!resp.ok) {
    throw new Error(await resp.text())
  }
  return resp.json()
}

export function queryString(obj) {
  const params = new URLSearchParams()
  for (const [key, value] of Object.entries(obj)) {
    if (value !== undefined && value !== null && value !== '') {
      params.set(key, value)
    }
  }
  return params.toString()
}

export async function fetchStats(filters) {
  return fetchJSON(`/_proxy/stats?${queryString(filters)}`)
}

export async function fetchRequests(limit, offset, filters) {
  return fetchJSON(`/_proxy/requests?${queryString({ limit, offset, ...filters })}`)
}

export async function fetchRequest(id) {
  return fetchJSON(`/_proxy/request/${encodeURIComponent(id)}`)
}

export async function deleteRequest(id) {
  return fetchJSON(`/_proxy/request/${encodeURIComponent(id)}`, { method: 'DELETE' })
}

export async function fetchRaw(id, part) {
  const resp = await fetch(`/_proxy/raw/${encodeURIComponent(id)}/${part}`)
  if (!resp.ok) {
    return null
  }
  const text = await resp.text()
  try {
    return JSON.stringify(JSON.parse(text), null, 2)
  } catch {
    return text
  }
}

export async function fetchModels() {
  const data = await fetchJSON('/_proxy/models')
  return Array.isArray(data.items) ? data.items : []
}

export async function fetchBackends() {
  const data = await fetchJSON('/_proxy/backends')
  return Array.isArray(data.items) ? data.items : []
}

export async function fetchStatsByBackend(filters = {}) {
  const data = await fetchJSON(`/_proxy/stats-by-backend?${queryString(filters)}`)
  return Array.isArray(data.items) ? data.items : []
}

export async function fetchDailyStats(days, filters = {}) {
  const data = await fetchJSON(`/_proxy/daily-stats?${queryString({ days, ...filters })}`)
  return {
    items: Array.isArray(data.items) ? data.items : [],
    days: data.days || days,
  }
}

export async function fetchScheduler() {
  return fetchJSON('/_proxy/scheduler')
}

// 手动触发调度器切换到指定模型（scheduling 中配置的模型 ID）。
// 返回 {status:"switching"} 或 {status:"skipped"}（coding 租约活跃时跳过）。
export async function switchSchedulerModel(model) {
  return fetchJSON('/_proxy/scheduler/switch', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ model }),
  })
}

export async function fetchInflight() {
  const data = await fetchJSON('/_proxy/inflight')
  return Array.isArray(data.items) ? data.items : []
}
