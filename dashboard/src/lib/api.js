const API = import.meta.env.VITE_API_URL || ''

async function apiFetch(path) {
  const res = await fetch(`${API}/api/v1${path}`)
  if (!res.ok) throw new Error(`API ${path}: ${res.status}`)
  return res.json()
}

export async function getOverview()         { return apiFetch('/overview') }
export async function getDailyCosts(days=30, provider='') {
  return apiFetch(`/costs/daily?days=${days}${provider ? `&provider=${provider}` : ''}`)
}
export async function getServiceBreakdown(provider='') {
  return apiFetch(`/costs/services${provider ? `?provider=${provider}` : ''}`)
}
export async function getCostMovers()       { return apiFetch('/costs/movers') }
export async function getForecasts(provider='') {
  return apiFetch(`/costs/forecast${provider ? `?provider=${provider}` : ''}`)
}
export async function getK8sCostMap()       { return apiFetch('/kubernetes/costmap') }
export async function getTopPods(ns='')     { return apiFetch(`/kubernetes/pods${ns ? `?namespace=${ns}` : ''}`) }
export async function getEgressFlows()      { return apiFetch('/kubernetes/egress') }
export async function getAnomalies(status='open') { return apiFetch(`/anomalies?status=${status}`) }
export async function getRecommendations(type='') {
  return apiFetch(`/recommendations${type ? `?type=${type}` : ''}`)
}
export async function getResources(orphanOnly=false) {
  return apiFetch(`/resources${orphanOnly ? '?orphan=true' : ''}`)
}

export async function updateAnomaly(id, status, note='') {
  const res = await fetch(`${API}/api/v1/anomalies/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ status, note }),
  })
  return res.json()
}

export async function updateRecommendation(id, status, reason='') {
  const res = await fetch(`${API}/api/v1/recommendations/${id}`, {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ status, reason }),
  })
  return res.json()
}
