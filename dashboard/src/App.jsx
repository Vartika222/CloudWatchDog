import { useState, useEffect, useCallback } from 'react'
import { AreaChart, Area, BarChart, Bar, XAxis, YAxis, CartesianGrid, Tooltip,
         ResponsiveContainer, PieChart, Pie, Cell, LineChart, Line } from 'recharts'
import { AlertTriangle, TrendingUp, TrendingDown, Zap, Server, Cloud,
         GitPullRequest, Shield, Eye, ChevronRight, RefreshCw,
         BarChart2, Activity, Package, ArrowUpRight, ArrowDownRight,
         X, Check, Clock, Layers } from 'lucide-react'
import * as api from './lib/api.js'

// ── Design tokens ─────────────────────────────────────────────────────────────
const C = {
  bg:      '#0a0e1a',
  surface: '#111827',
  border:  '#1e2a3a',
  muted:   '#2a3a4a',
  text:    '#e2e8f0',
  dim:     '#7a8a9a',
  accent:  '#3b82f6',
  green:   '#10b981',
  red:     '#ef4444',
  yellow:  '#f59e0b',
  purple:  '#8b5cf6',
  cyan:    '#06b6d4',
  aws:     '#ff9900',
  azure:   '#0078d4',
}

const fmt = {
  usd:  v => v >= 1000 ? `$${(v/1000).toFixed(1)}k` : `$${(v||0).toFixed(0)}`,
  usdFull: v => `$${(v||0).toLocaleString('en-US', {minimumFractionDigits:2,maximumFractionDigits:2})}`,
  pct:  v => `${v >= 0 ? '+' : ''}${(v||0).toFixed(1)}%`,
  bytes: v => {
    if (!v) return '0 B'
    const units = ['B','KB','MB','GB','TB']
    let i = 0; let n = v
    while (n >= 1024 && i < units.length-1) { n /= 1024; i++ }
    return `${n.toFixed(1)} ${units[i]}`
  },
  date: d => new Date(d).toLocaleDateString('en-US',{month:'short',day:'numeric'}),
}

const SEVERITY_COLOR = { critical:'#ef4444', high:'#f59e0b', medium:'#3b82f6', low:'#10b981' }
const FLOW_COLOR = { internet:'#ef4444', inter_az:'#f59e0b', intra_az:'#10b981', intra_pod:'#3b82f6' }

// ── Shared components ─────────────────────────────────────────────────────────
const Badge = ({ label, color }) => (
  <span style={{
    background: `${color}22`, color, border: `1px solid ${color}44`,
    borderRadius: 4, padding: '2px 8px', fontSize: 11, fontWeight: 600,
    fontFamily: 'IBM Plex Mono, monospace', letterSpacing: '0.04em', textTransform: 'uppercase',
  }}>{label}</span>
)

const Card = ({ children, style={} }) => (
  <div style={{
    background: C.surface, border: `1px solid ${C.border}`,
    borderRadius: 10, padding: 20, ...style
  }}>{children}</div>
)

const CardTitle = ({ children, right }) => (
  <div style={{ display:'flex', justifyContent:'space-between', alignItems:'center', marginBottom: 16 }}>
    <span style={{ fontSize: 13, fontWeight: 600, color: C.dim, letterSpacing:'0.08em', textTransform:'uppercase' }}>
      {children}
    </span>
    {right}
  </div>
)

const Spinner = () => (
  <div style={{ display:'flex', alignItems:'center', justifyContent:'center', height: 120 }}>
    <div style={{
      width: 24, height: 24, borderRadius: '50%',
      border: `2px solid ${C.border}`, borderTopColor: C.accent,
      animation: 'spin 0.8s linear infinite',
    }}/>
  </div>
)

const Stat = ({ label, value, delta, color = C.text, mono = false }) => (
  <div>
    <div style={{ fontSize: 11, color: C.dim, marginBottom: 4, letterSpacing:'0.06em', textTransform:'uppercase' }}>{label}</div>
    <div style={{ fontSize: 28, fontWeight: 700, color, fontFamily: mono ? 'IBM Plex Mono, monospace' : 'inherit' }}>{value}</div>
    {delta != null && (
      <div style={{ fontSize: 12, color: delta >= 0 ? C.red : C.green, marginTop: 4, display:'flex', alignItems:'center', gap: 3 }}>
        {delta >= 0 ? <ArrowUpRight size={12}/> : <ArrowDownRight size={12}/>}
        {fmt.pct(delta)} vs prior week
      </div>
    )}
  </div>
)

// ── Overview Page ─────────────────────────────────────────────────────────────
function OverviewPage() {
  const [overview, setOverview] = useState(null)
  const [dailyCosts, setDailyCosts] = useState([])
  const [movers, setMovers] = useState([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    Promise.all([api.getOverview(), api.getDailyCosts(60), api.getCostMovers()])
      .then(([ov, dc, mv]) => {
        setOverview(ov)
        // Pivot daily costs for chart
        const byDate = {}
        dc.forEach(d => {
          if (!byDate[d.date]) byDate[d.date] = { date: d.date, aws: 0, azure: 0 }
          byDate[d.date][d.provider] = d.total
        })
        setDailyCosts(Object.values(byDate).sort((a,b) => a.date.localeCompare(b.date)))
        setMovers(mv)
      })
      .finally(() => setLoading(false))
  }, [])

  if (loading) return <Spinner/>
  if (!overview) return <div style={{color: C.dim}}>No data</div>

  const totalSavings = overview.total_monthly_savings || 0

  return (
    <div style={{ display:'grid', gap: 20 }}>
      {/* KPI row */}
      <div style={{ display:'grid', gridTemplateColumns:'repeat(4,1fr)', gap: 16 }}>
        {[
          { label:'30-Day Spend', value: fmt.usd(overview.total_spend_30d), delta: overview.change_vs_prior_7d, color: C.text },
          { label:'Monthly Savings Available', value: fmt.usd(totalSavings), color: C.green },
          { label:'Open Anomalies', value: overview.open_anomalies, color: overview.critical_anomalies > 0 ? C.red : C.yellow },
          { label:'Orphan Resources', value: `${overview.orphan_resources} (${fmt.usd(overview.orphan_monthly_cost)}/mo)`, color: C.yellow },
        ].map(s => (
          <Card key={s.label}>
            <Stat {...s}/>
          </Card>
        ))}
      </div>

      {/* Provider split + daily chart */}
      <div style={{ display:'grid', gridTemplateColumns:'200px 1fr', gap: 16 }}>
        <Card>
          <CardTitle>By Provider</CardTitle>
          {(overview.provider_breakdown||[]).map(p => (
            <div key={p.provider} style={{ marginBottom: 14 }}>
              <div style={{ display:'flex', justifyContent:'space-between', marginBottom: 4 }}>
                <span style={{ color: p.provider === 'aws' ? C.aws : C.azure, fontSize: 13, fontWeight:600 }}>
                  {p.provider.toUpperCase()}
                </span>
                <span style={{ color: C.text, fontSize: 13 }}>{fmt.usd(p.spend_30d)}</span>
              </div>
              <div style={{ background: C.muted, borderRadius: 3, height: 4, overflow:'hidden' }}>
                <div style={{
                  background: p.provider === 'aws' ? C.aws : C.azure,
                  width: `${Math.min((p.spend_30d / (overview.total_spend_30d || 1)) * 100, 100)}%`,
                  height: '100%', borderRadius: 3,
                }}/>
              </div>
            </div>
          ))}
        </Card>

        <Card>
          <CardTitle>Daily Spend (60 Days)</CardTitle>
          <ResponsiveContainer width="100%" height={200}>
            <AreaChart data={dailyCosts} margin={{top:0,right:0,bottom:0,left:0}}>
              <defs>
                <linearGradient id="awsGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={C.aws} stopOpacity={0.3}/>
                  <stop offset="95%" stopColor={C.aws} stopOpacity={0}/>
                </linearGradient>
                <linearGradient id="azureGrad" x1="0" y1="0" x2="0" y2="1">
                  <stop offset="5%" stopColor={C.azure} stopOpacity={0.3}/>
                  <stop offset="95%" stopColor={C.azure} stopOpacity={0}/>
                </linearGradient>
              </defs>
              <CartesianGrid strokeDasharray="3 3" stroke={C.border} vertical={false}/>
              <XAxis dataKey="date" tickFormatter={fmt.date} tick={{fill:C.dim,fontSize:11}} axisLine={false} tickLine={false}/>
              <YAxis tickFormatter={v=>`$${(v/1000).toFixed(0)}k`} tick={{fill:C.dim,fontSize:11}} axisLine={false} tickLine={false}/>
              <Tooltip
                contentStyle={{background:C.surface,border:`1px solid ${C.border}`,borderRadius:6,fontSize:12}}
                formatter={(v,n) => [fmt.usd(v), n.toUpperCase()]}
                labelFormatter={fmt.date}
              />
              <Area type="monotone" dataKey="aws"   stroke={C.aws}   fill="url(#awsGrad)"   strokeWidth={2} dot={false}/>
              <Area type="monotone" dataKey="azure" stroke={C.azure} fill="url(#azureGrad)" strokeWidth={2} dot={false}/>
            </AreaChart>
          </ResponsiveContainer>
        </Card>
      </div>

      {/* Cost movers */}
      <Card>
        <CardTitle>Top Cost Movers (7d vs prior 7d)</CardTitle>
        <div style={{ display:'grid', gridTemplateColumns:'repeat(auto-fill,minmax(260px,1fr))', gap: 12 }}>
          {movers.slice(0,8).map(m => {
            const up = (m.change_pct||0) >= 0
            return (
              <div key={`${m.provider}-${m.service}`} style={{
                background: C.bg, borderRadius: 8, padding: '12px 16px',
                border: `1px solid ${C.border}`, display:'flex', justifyContent:'space-between', alignItems:'center'
              }}>
                <div>
                  <div style={{ fontSize: 11, color: m.provider === 'aws' ? C.aws : C.azure, marginBottom: 2, fontWeight:600 }}>
                    {m.provider.toUpperCase()}
                  </div>
                  <div style={{ fontSize: 13, color: C.text }}>{m.service}</div>
                  <div style={{ fontSize: 12, color: C.dim }}>{fmt.usd(m.cost_7d)} this week</div>
                </div>
                <div style={{ textAlign:'right' }}>
                  <div style={{ fontSize: 16, fontWeight:700, color: up ? C.red : C.green, fontFamily:'IBM Plex Mono,monospace' }}>
                    {up ? '+' : ''}{(m.change_pct||0).toFixed(1)}%
                  </div>
                  <div style={{ fontSize: 11, color: C.dim }}>{up ? <ArrowUpRight size={12} style={{display:'inline'}}/> : <ArrowDownRight size={12} style={{display:'inline'}}/>} {fmt.usd(Math.abs(m.delta||0))}</div>
                </div>
              </div>
            )
          })}
        </div>
      </Card>
    </div>
  )
}

// ── Kubernetes Cost Map ───────────────────────────────────────────────────────
function KubernetesPage() {
  const [costMap, setCostMap] = useState([])
  const [pods, setPods] = useState([])
  const [egress, setEgress] = useState([])
  const [selectedNS, setSelectedNS] = useState(null)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    Promise.all([api.getK8sCostMap(), api.getTopPods(), api.getEgressFlows()])
      .then(([cm, p, e]) => { setCostMap(cm||[]); setPods(p||[]); setEgress(e||[]) })
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    if (selectedNS) api.getTopPods(selectedNS).then(setPods)
    else api.getTopPods().then(setPods)
  }, [selectedNS])

  if (loading) return <Spinner/>

  const nsData = costMap.reduce((acc, row) => {
    const key = row.namespace
    if (!acc[key]) acc[key] = { namespace: key, total: 0, egress: 0, compute: 0, pods: 0, cluster: row.cluster }
    acc[key].total   += row.cost_30d || 0
    acc[key].egress  += row.egress_cost_30d || 0
    acc[key].compute += row.compute_cost_30d || 0
    acc[key].pods    = Math.max(acc[key].pods, row.pod_count || 0)
    return acc
  }, {})
  const nsList = Object.values(nsData).sort((a,b) => b.total - a.total)
  const totalK8s = nsList.reduce((s,n) => s+n.total, 0)

  const egressByCost = [...(egress||[])].sort((a,b) => (b.egress_cost||0) - (a.egress_cost||0))

  return (
    <div style={{ display:'grid', gap: 20 }}>
      {/* Namespace treemap-style bars */}
      <Card>
        <CardTitle right={
          selectedNS && <button onClick={()=>setSelectedNS(null)} style={{
            background:'none', border:`1px solid ${C.border}`, color: C.dim,
            padding:'3px 10px', borderRadius:4, cursor:'pointer', fontSize:11
          }}>Clear filter</button>
        }>
          Namespace Cost Map (30d) — click to filter pods
        </CardTitle>
        <div style={{ display:'grid', gridTemplateColumns:'repeat(auto-fill,minmax(220px,1fr))', gap: 10 }}>
          {nsList.map(ns => {
            const pct = totalK8s > 0 ? (ns.total / totalK8s) * 100 : 0
            const egressPct = ns.total > 0 ? (ns.egress / ns.total) * 100 : 0
            const isSelected = selectedNS === ns.namespace
            return (
              <div key={ns.namespace}
                onClick={() => setSelectedNS(isSelected ? null : ns.namespace)}
                style={{
                  background: isSelected ? `${C.accent}18` : C.bg,
                  border: `1px solid ${isSelected ? C.accent : C.border}`,
                  borderRadius: 8, padding: '12px 14px', cursor: 'pointer',
                  transition: 'all 0.15s',
                }}>
                <div style={{ display:'flex', justifyContent:'space-between', marginBottom: 8 }}>
                  <span style={{ fontSize: 13, fontWeight:600, color: C.text }}>{ns.namespace}</span>
                  <span style={{ fontSize: 13, color: C.accent, fontFamily:'IBM Plex Mono,monospace' }}>{fmt.usd(ns.total)}</span>
                </div>
                <div style={{ background: C.muted, borderRadius: 3, height: 5, overflow:'hidden', marginBottom: 6 }}>
                  <div style={{ background: C.accent, width:`${pct}%`, height:'100%', borderRadius:3 }}/>
                </div>
                <div style={{ display:'flex', justifyContent:'space-between', fontSize:11, color: C.dim }}>
                  <span>{pct.toFixed(1)}% of cluster</span>
                  {egressPct > 5 && (
                    <span style={{ color: egressPct > 20 ? C.red : C.yellow }}>
                      {egressPct.toFixed(1)}% egress ⚠
                    </span>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      </Card>

      <div style={{ display:'grid', gridTemplateColumns:'1fr 1fr', gap: 16 }}>
        {/* Top pods */}
        <Card>
          <CardTitle>{selectedNS ? `Pods in ${selectedNS}` : 'Top Pods by Cost (7d)'}</CardTitle>
          <div style={{ overflowX:'auto' }}>
            <table style={{ width:'100%', borderCollapse:'collapse', fontSize:12 }}>
              <thead>
                <tr style={{ color: C.dim, borderBottom:`1px solid ${C.border}` }}>
                  {['Pod','Cost 7d','CPU%','Mem%','Egress'].map(h => (
                    <th key={h} style={{ textAlign:'left', padding:'4px 8px', fontWeight:500 }}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {(pods||[]).slice(0,10).map((p,i) => (
                  <tr key={i} style={{ borderBottom:`1px solid ${C.border}22` }}>
                    <td style={{ padding:'6px 8px', color: C.text }}>
                      <div style={{ fontSize:12 }}>{p.pod}</div>
                      <div style={{ fontSize:10, color:C.dim }}>{p.namespace}</div>
                    </td>
                    <td style={{ padding:'6px 8px', color:C.accent, fontFamily:'IBM Plex Mono,monospace' }}>{fmt.usd(p.cost_7d)}</td>
                    <td style={{ padding:'6px 8px', color: (p.cpu_util_pct||0) < 20 ? C.yellow : C.green }}>
                      {p.cpu_util_pct ? `${p.cpu_util_pct.toFixed(0)}%` : '-'}
                    </td>
                    <td style={{ padding:'6px 8px', color: (p.mem_util_pct||0) > 85 ? C.red : C.text }}>
                      {p.mem_util_pct ? `${p.mem_util_pct.toFixed(0)}%` : '-'}
                    </td>
                    <td style={{ padding:'6px 8px', color: p.egress_cost > 1 ? C.yellow : C.dim }}>
                      {fmt.usd(p.egress_cost||0)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Card>

        {/* Egress flows */}
        <Card>
          <CardTitle>Egress Flows (7d)</CardTitle>
          <div style={{ marginBottom: 12, display:'flex', gap: 8, flexWrap:'wrap' }}>
            {Object.entries(FLOW_COLOR).map(([type, color]) => (
              <div key={type} style={{ display:'flex', alignItems:'center', gap:4, fontSize:11 }}>
                <div style={{ width:8, height:8, borderRadius:'50%', background:color }}/>
                <span style={{ color: C.dim }}>{type.replace('_',' ')}</span>
              </div>
            ))}
          </div>
          {egressByCost.slice(0,8).map((f,i) => (
            <div key={i} style={{
              background: C.bg, borderRadius:6, padding:'8px 12px', marginBottom:8,
              border:`1px solid ${C.border}`, display:'flex', justifyContent:'space-between', alignItems:'center'
            }}>
              <div style={{ fontSize:12 }}>
                <span style={{ color: C.text }}>{f.src_namespace || '?'}</span>
                <span style={{ color: C.dim, margin:'0 6px' }}>→</span>
                <span style={{ color: f.flow_type === 'internet' ? C.red : C.text }}>
                  {f.dst_namespace || f.dst_ip || 'external'}
                </span>
                <div style={{ marginTop:2 }}>
                  <Badge label={f.flow_type} color={FLOW_COLOR[f.flow_type]||C.dim}/>
                  <span style={{ color:C.dim, fontSize:11, marginLeft:8 }}>{fmt.bytes(f.bytes_total)}</span>
                </div>
              </div>
              <div style={{ color: f.egress_cost > 0 ? C.yellow : C.dim, fontFamily:'IBM Plex Mono,monospace', fontSize:13, fontWeight:600 }}>
                {fmt.usdFull(f.egress_cost||0)}
              </div>
            </div>
          ))}
        </Card>
      </div>
    </div>
  )
}

// ── Anomalies Page ─────────────────────────────────────────────────────────────
function AnomaliesPage() {
  const [anomalies, setAnomalies] = useState([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState(null)

  const load = useCallback(() => {
    setLoading(true)
    api.getAnomalies('open').then(setAnomalies).finally(() => setLoading(false))
  }, [])

  useEffect(() => { load() }, [])

  const handleUpdate = async (id, status) => {
    await api.updateAnomaly(id, status)
    load()
  }

  if (loading) return <Spinner/>

  return (
    <div style={{ display:'grid', gap: 12 }}>
      <div style={{ display:'flex', justifyContent:'space-between', alignItems:'center' }}>
        <span style={{ color:C.dim, fontSize:13 }}>{anomalies.length} open anomalies</span>
        <button onClick={load} style={{ background:'none', border:`1px solid ${C.border}`,
          color:C.dim, padding:'4px 12px', borderRadius:6, cursor:'pointer', fontSize:12,
          display:'flex', alignItems:'center', gap:6 }}>
          <RefreshCw size={12}/> Refresh
        </button>
      </div>

      {anomalies.map(a => (
        <div key={a.id} style={{
          background: C.surface, border: `1px solid ${a.severity === 'high' || a.severity === 'critical' ? SEVERITY_COLOR[a.severity]+'44' : C.border}`,
          borderRadius: 10, overflow:'hidden',
        }}>
          <div style={{
            padding:'14px 20px', display:'flex', alignItems:'flex-start',
            justifyContent:'space-between', cursor:'pointer',
            borderLeft: `3px solid ${SEVERITY_COLOR[a.severity]||C.dim}`
          }}
            onClick={() => setExpanded(expanded === a.id ? null : a.id)}>
            <div style={{ flex:1 }}>
              <div style={{ display:'flex', gap:8, alignItems:'center', marginBottom:6, flexWrap:'wrap' }}>
                <Badge label={a.severity} color={SEVERITY_COLOR[a.severity]}/>
                <Badge label={a.type?.replace('_',' ')} color={C.accent}/>
                {a.provider && <Badge label={a.provider} color={a.provider==='aws'?C.aws:C.azure}/>}
                {a.impact_usd > 0 && (
                  <span style={{ fontSize:12, color:C.red, fontFamily:'IBM Plex Mono,monospace' }}>
                    ${a.impact_usd.toFixed(0)} impact
                  </span>
                )}
              </div>
              <div style={{ fontSize:13, color:C.text, lineHeight:1.5 }}>
                {a.description.slice(0, expanded === a.id ? 9999 : 120)}{expanded !== a.id && a.description.length > 120 && '...'}
              </div>
              {(a.namespace || a.resource) && (
                <div style={{ fontSize:11, color:C.dim, marginTop:4, fontFamily:'IBM Plex Mono,monospace' }}>
                  {[a.namespace, a.resource, a.pod].filter(Boolean).join(' / ')}
                </div>
              )}
            </div>
            <div style={{ display:'flex', gap:8, marginLeft:16, alignItems:'center' }}>
              <div style={{ textAlign:'right', marginRight:8 }}>
                <div style={{ fontSize:11, color:C.dim }}>Z-score</div>
                <div style={{ fontSize:16, fontWeight:700, fontFamily:'IBM Plex Mono,monospace',
                  color: Math.abs(a.zscore||0) > 3 ? C.red : C.yellow }}>
                  {(a.zscore||0).toFixed(2)}
                </div>
              </div>
              <ChevronRight size={16} style={{ color:C.dim, transform: expanded===a.id ? 'rotate(90deg)' : 'none', transition:'0.2s' }}/>
            </div>
          </div>

          {expanded === a.id && (
            <div style={{ padding:'0 20px 16px', borderTop:`1px solid ${C.border}` }}>
              <div style={{ display:'grid', gridTemplateColumns:'repeat(3,1fr)', gap:12, marginTop:12, marginBottom:12 }}>
                {[
                  { label:'Metric Value', value: a.metric_value?.toFixed(2) },
                  { label:'Baseline',     value: a.baseline?.toFixed(2) },
                  { label:'Deviation',    value: a.deviation_pct ? `${a.deviation_pct.toFixed(1)}%` : '-' },
                ].map(s => (
                  <div key={s.label} style={{ background:C.bg, borderRadius:6, padding:10 }}>
                    <div style={{ fontSize:10, color:C.dim, marginBottom:2 }}>{s.label}</div>
                    <div style={{ fontSize:16, fontFamily:'IBM Plex Mono,monospace', color:C.text }}>{s.value}</div>
                  </div>
                ))}
              </div>
              <div style={{ display:'flex', gap:8 }}>
                <button onClick={() => handleUpdate(a.id,'acknowledged')} style={{
                  background:`${C.yellow}22`, border:`1px solid ${C.yellow}44`, color:C.yellow,
                  padding:'6px 14px', borderRadius:6, cursor:'pointer', fontSize:12, display:'flex', alignItems:'center', gap:4
                }}><Clock size={12}/> Acknowledge</button>
                <button onClick={() => handleUpdate(a.id,'resolved')} style={{
                  background:`${C.green}22`, border:`1px solid ${C.green}44`, color:C.green,
                  padding:'6px 14px', borderRadius:6, cursor:'pointer', fontSize:12, display:'flex', alignItems:'center', gap:4
                }}><Check size={12}/> Resolve</button>
                <button onClick={() => handleUpdate(a.id,'false_positive')} style={{
                  background:`${C.dim}22`, border:`1px solid ${C.dim}44`, color:C.dim,
                  padding:'6px 14px', borderRadius:6, cursor:'pointer', fontSize:12, display:'flex', alignItems:'center', gap:4
                }}><X size={12}/> False Positive</button>
              </div>
            </div>
          )}
        </div>
      ))}

      {anomalies.length === 0 && (
        <Card style={{ textAlign:'center', color: C.green, padding: 40 }}>
          <Check size={32} style={{ marginBottom:8 }}/><br/>
          No open anomalies. All systems normal.
        </Card>
      )}
    </div>
  )
}

// ── Recommendations Page ──────────────────────────────────────────────────────
function RecommendationsPage() {
  const [recs, setRecs] = useState([])
  const [loading, setLoading] = useState(true)
  const [expanded, setExpanded] = useState(null)
  const [filter, setFilter] = useState('')

  const load = useCallback(() => {
    setLoading(true)
    api.getRecommendations().then(d => { setRecs(d||[]); setLoading(false) })
  }, [])

  useEffect(() => { load() }, [])

  const handleUpdate = async (id, status) => {
    await api.updateRecommendation(id, status)
    load()
    setExpanded(null)
  }

  if (loading) return <Spinner/>

  const filtered = filter ? recs.filter(r => r.type === filter) : recs
  const totalSavings = filtered.reduce((s,r) => s + (r.monthly_savings||0), 0)
  const types = [...new Set(recs.map(r=>r.type))]

  const REC_ICON = {
    rightsize:    <Server size={14}/>,
    ri_purchase:  <TrendingDown size={14}/>,
    orphan_delete:<Package size={14}/>,
    zone_routing: <Zap size={14}/>,
    ahb_activate: <Shield size={14}/>,
  }
  const REC_COLOR = {
    rightsize:    C.accent,
    ri_purchase:  C.green,
    orphan_delete:C.yellow,
    zone_routing: C.cyan,
    ahb_activate: C.purple,
  }

  return (
    <div style={{ display:'grid', gap: 16 }}>
      {/* Summary bar */}
      <Card style={{ display:'flex', justifyContent:'space-between', alignItems:'center' }}>
        <div>
          <div style={{ fontSize:11, color:C.dim, marginBottom:2 }}>TOTAL MONTHLY SAVINGS AVAILABLE</div>
          <div style={{ fontSize:32, fontWeight:700, color:C.green, fontFamily:'IBM Plex Mono,monospace' }}>
            {fmt.usdFull(totalSavings)}/mo
          </div>
          <div style={{ fontSize:12, color:C.dim }}>{fmt.usdFull(totalSavings*12)} annually</div>
        </div>
        <div style={{ display:'flex', gap:8, flexWrap:'wrap' }}>
          <button onClick={()=>setFilter('')} style={{
            background: !filter ? `${C.accent}22` : 'none',
            border:`1px solid ${!filter ? C.accent : C.border}`,
            color: !filter ? C.accent : C.dim,
            padding:'4px 12px', borderRadius:6, cursor:'pointer', fontSize:11
          }}>All ({recs.length})</button>
          {types.map(t => (
            <button key={t} onClick={()=>setFilter(t)} style={{
              background: filter===t ? `${REC_COLOR[t]||C.dim}22` : 'none',
              border:`1px solid ${filter===t ? REC_COLOR[t]||C.dim : C.border}`,
              color: filter===t ? REC_COLOR[t]||C.dim : C.dim,
              padding:'4px 12px', borderRadius:6, cursor:'pointer', fontSize:11,
              display:'flex', alignItems:'center', gap:4
            }}>
              {REC_ICON[t]} {t.replace('_',' ')}
            </button>
          ))}
        </div>
      </Card>

      {filtered.map(r => (
        <div key={r.id} style={{
          background: C.surface, border:`1px solid ${C.border}`,
          borderLeft:`3px solid ${REC_COLOR[r.type]||C.dim}`,
          borderRadius:10, overflow:'hidden'
        }}>
          <div style={{ padding:'14px 20px', cursor:'pointer', display:'flex', justifyContent:'space-between', alignItems:'flex-start' }}
            onClick={() => setExpanded(expanded===r.id ? null : r.id)}>
            <div style={{ flex:1 }}>
              <div style={{ display:'flex', gap:8, marginBottom:6, flexWrap:'wrap', alignItems:'center' }}>
                <span style={{ color:REC_COLOR[r.type]||C.dim, display:'flex', alignItems:'center', gap:4, fontSize:12, fontWeight:600 }}>
                  {REC_ICON[r.type]} {r.type?.replace(/_/g,' ')}
                </span>
                <Badge label={r.priority} color={SEVERITY_COLOR[r.priority]||C.dim}/>
                <Badge label={r.provider} color={r.provider==='aws'?C.aws:C.azure}/>
                {r.iac_type && <Badge label={r.iac_type} color={C.purple}/>}
              </div>
              <div style={{ fontSize:14, fontWeight:500, color:C.text, marginBottom:4 }}>{r.title}</div>
              <div style={{ fontSize:12, color:C.dim, lineHeight:1.5 }}>
                {r.description?.slice(0, expanded===r.id ? 9999 : 100)}
                {expanded!==r.id && (r.description||'').length > 100 && '...'}
              </div>
            </div>
            <div style={{ display:'flex', gap:12, marginLeft:20, alignItems:'center' }}>
              <div style={{ textAlign:'right' }}>
                <div style={{ fontSize:11, color:C.dim }}>Monthly saving</div>
                <div style={{ fontSize:18, fontWeight:700, color:C.green, fontFamily:'IBM Plex Mono,monospace' }}>
                  {fmt.usd(r.monthly_savings)}
                </div>
                <div style={{ fontSize:11, color:C.dim }}>{r.confidence_pct?.toFixed(0)}% confidence</div>
              </div>
              <ChevronRight size={16} style={{ color:C.dim, transform:expanded===r.id?'rotate(90deg)':'none', transition:'0.2s' }}/>
            </div>
          </div>

          {expanded === r.id && (
            <div style={{ padding:'0 20px 16px', borderTop:`1px solid ${C.border}` }}>
              {r.iac_patch && (
                <div style={{ marginTop:12, marginBottom:12 }}>
                  <div style={{ fontSize:11, color:C.dim, marginBottom:6, display:'flex', alignItems:'center', gap:6 }}>
                    <GitPullRequest size={12}/> IaC PATCH ({r.iac_type})
                  </div>
                  <pre style={{
                    background:C.bg, border:`1px solid ${C.border}`, borderRadius:6,
                    padding:12, fontSize:12, color:C.cyan, overflow:'auto', maxHeight:200,
                    fontFamily:'IBM Plex Mono,monospace', margin:0, lineHeight:1.6
                  }}>{r.iac_patch}</pre>
                </div>
              )}
              <div style={{ display:'flex', gap:8 }}>
                <button onClick={()=>handleUpdate(r.id,'applied')} style={{
                  background:`${C.green}22`, border:`1px solid ${C.green}44`, color:C.green,
                  padding:'6px 14px', borderRadius:6, cursor:'pointer', fontSize:12, display:'flex', alignItems:'center', gap:4
                }}><Check size={12}/> Mark Applied</button>
                <button onClick={()=>handleUpdate(r.id,'dismissed')} style={{
                  background:`${C.dim}22`, border:`1px solid ${C.dim}44`, color:C.dim,
                  padding:'6px 14px', borderRadius:6, cursor:'pointer', fontSize:12, display:'flex', alignItems:'center', gap:4
                }}><X size={12}/> Dismiss</button>
              </div>
            </div>
          )}
        </div>
      ))}
    </div>
  )
}

// ── Forecast Page ─────────────────────────────────────────────────────────────
function ForecastPage() {
  const [forecasts, setForecasts] = useState([])
  const [historical, setHistorical] = useState([])
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    Promise.all([api.getForecasts(), api.getDailyCosts(30)])
      .then(([f, h]) => {
        setForecasts(f||[])
        const byDate = {}
        ;(h||[]).forEach(d => {
          if (!byDate[d.date]) byDate[d.date] = { date:d.date, total:0 }
          byDate[d.date].total += d.total
        })
        setHistorical(Object.values(byDate).sort((a,b)=>a.date.localeCompare(b.date)))
      })
      .finally(() => setLoading(false))
  }, [])

  if (loading) return <Spinner/>

  // Merge historical + forecast for chart
  const byDate = {}
  historical.forEach(h => { byDate[h.date] = { date:h.date, actual:h.total } })
  const fByDate = {}
  forecasts.forEach(f => {
    if (!fByDate[f.date]) fByDate[f.date] = { date:f.date, aws:0, azure:0, lower:0, upper:0 }
    fByDate[f.date][f.provider] = f.forecast
    fByDate[f.date].lower  += f.lower || 0
    fByDate[f.date].upper  += f.upper || 0
  })
  Object.values(fByDate).forEach(f => {
    byDate[f.date] = { ...byDate[f.date], ...f, forecast: f.aws + f.azure }
  })
  const chartData = Object.values(byDate).sort((a,b) => a.date.localeCompare(b.date))

  const nextMonth = forecasts.filter(f => {
    const d = new Date(f.date); const now = new Date()
    return d.getMonth() === now.getMonth() && d.getFullYear() === now.getFullYear()
  })
  const forecastTotal = Object.values(fByDate).reduce((s,f) => s + f.aws + f.azure, 0)
  const avgDaily = forecastTotal / (forecasts.length / 2 || 1)

  return (
    <div style={{ display:'grid', gap:20 }}>
      <div style={{ display:'grid', gridTemplateColumns:'repeat(3,1fr)', gap:16 }}>
        {[
          { label:'30-Day Forecast Total', value:fmt.usd(forecastTotal), color:C.accent },
          { label:'Avg Daily (forecast)',  value:fmt.usd(avgDaily),      color:C.text },
          { label:'Model',                value:'Linear Trend + Seasonality', color:C.purple },
        ].map(s => <Card key={s.label}><Stat {...s}/></Card>)}
      </div>

      <Card>
        <CardTitle>Historical vs Forecast (60d)</CardTitle>
        <div style={{ fontSize:11, color:C.dim, marginBottom:12, display:'flex', gap:16 }}>
          <span><span style={{ color:C.text }}>━</span> Actual</span>
          <span><span style={{ color:C.purple }}>━</span> Forecast</span>
          <span><span style={{ color:C.purple, opacity:0.3 }}>▓</span> Confidence band</span>
        </div>
        <ResponsiveContainer width="100%" height={280}>
          <AreaChart data={chartData} margin={{top:0,right:0,bottom:0,left:0}}>
            <defs>
              <linearGradient id="fcGrad" x1="0" y1="0" x2="0" y2="1">
                <stop offset="5%"  stopColor={C.purple} stopOpacity={0.25}/>
                <stop offset="95%" stopColor={C.purple} stopOpacity={0.02}/>
              </linearGradient>
            </defs>
            <CartesianGrid strokeDasharray="3 3" stroke={C.border} vertical={false}/>
            <XAxis dataKey="date" tickFormatter={fmt.date} tick={{fill:C.dim,fontSize:11}} axisLine={false} tickLine={false}/>
            <YAxis tickFormatter={v=>`$${(v/1000).toFixed(0)}k`} tick={{fill:C.dim,fontSize:11}} axisLine={false} tickLine={false}/>
            <Tooltip
              contentStyle={{background:C.surface,border:`1px solid ${C.border}`,borderRadius:6,fontSize:12}}
              formatter={(v,n) => [fmt.usd(v), n]}
              labelFormatter={fmt.date}
            />
            <Area type="monotone" dataKey="upper"    fill="url(#fcGrad)" stroke="none" />
            <Area type="monotone" dataKey="lower"    fill={C.bg}         stroke="none" />
            <Line type="monotone" dataKey="actual"   stroke={C.text}   strokeWidth={2} dot={false} connectNulls/>
            <Line type="monotone" dataKey="forecast" stroke={C.purple} strokeWidth={2} strokeDasharray="5 4" dot={false} connectNulls/>
          </AreaChart>
        </ResponsiveContainer>
      </Card>
    </div>
  )
}

// ── Resources Page ─────────────────────────────────────────────────────────────
function ResourcesPage() {
  const [resources, setResources] = useState([])
  const [orphanOnly, setOrphanOnly] = useState(false)
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    setLoading(true)
    api.getResources(orphanOnly).then(d => setResources(d||[])).finally(()=>setLoading(false))
  }, [orphanOnly])

  if (loading) return <Spinner/>

  const orphans = resources.filter(r => r.is_orphan)
  const orphanCost = orphans.reduce((s,r) => s + (r.cost_30d||0), 0)

  return (
    <div style={{ display:'grid', gap:16 }}>
      <div style={{ display:'grid', gridTemplateColumns:'repeat(3,1fr)', gap:16 }}>
        <Card><Stat label="Total Resources" value={resources.length}/></Card>
        <Card><Stat label="Orphan Resources" value={orphans.length} color={C.yellow}/></Card>
        <Card><Stat label="Orphan Monthly Cost" value={fmt.usd(orphanCost)} color={C.red}/></Card>
      </div>

      <Card>
        <CardTitle right={
          <button onClick={()=>setOrphanOnly(!orphanOnly)} style={{
            background: orphanOnly ? `${C.yellow}22` : 'none',
            border:`1px solid ${orphanOnly ? C.yellow : C.border}`,
            color: orphanOnly ? C.yellow : C.dim,
            padding:'4px 12px', borderRadius:6, cursor:'pointer', fontSize:11
          }}>
            {orphanOnly ? '✓ ' : ''}Orphans only
          </button>
        }>
          Resource Inventory ({resources.length})
        </CardTitle>
        <div style={{ overflowX:'auto' }}>
          <table style={{ width:'100%', borderCollapse:'collapse', fontSize:12 }}>
            <thead>
              <tr style={{ color:C.dim, borderBottom:`1px solid ${C.border}` }}>
                {['Provider','Name','Type','Region','Cost 30d','Commitment','Status'].map(h => (
                  <th key={h} style={{ textAlign:'left', padding:'6px 10px', fontWeight:500 }}>{h}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {resources.map((r,i) => (
                <tr key={i} style={{ borderBottom:`1px solid ${C.border}22`, background: r.is_orphan ? `${C.yellow}08` : 'transparent' }}>
                  <td style={{ padding:'8px 10px' }}>
                    <Badge label={r.provider} color={r.provider==='aws'?C.aws:C.azure}/>
                  </td>
                  <td style={{ padding:'8px 10px', color:C.text, maxWidth:200 }}>
                    <div style={{ fontSize:12, fontFamily:'IBM Plex Mono,monospace', overflow:'hidden', textOverflow:'ellipsis', whiteSpace:'nowrap' }}>
                      {r.name}
                    </div>
                    {r.is_orphan && <div style={{ fontSize:10, color:C.yellow }}>⚠ {r.orphan_reason}</div>}
                  </td>
                  <td style={{ padding:'8px 10px', color:C.dim }}>{r.type?.replace('_',' ')}</td>
                  <td style={{ padding:'8px 10px', color:C.dim, fontFamily:'IBM Plex Mono,monospace', fontSize:11 }}>{r.region}</td>
                  <td style={{ padding:'8px 10px', color:C.accent, fontFamily:'IBM Plex Mono,monospace' }}>{fmt.usd(r.cost_30d)}</td>
                  <td style={{ padding:'8px 10px' }}>
                    <Badge label={(r.commitment_type||'on_demand').replace('_',' ')}
                      color={r.commitment_type?.includes('reserved') ? C.green : C.dim}/>
                  </td>
                  <td style={{ padding:'8px 10px' }}>
                    <Badge label={r.status} color={r.status==='active'?C.green:C.yellow}/>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  )
}

// ── Nav ───────────────────────────────────────────────────────────────────────
const NAV = [
  { id:'overview',      label:'Overview',        icon:<BarChart2 size={16}/> },
  { id:'kubernetes',    label:'Kubernetes',      icon:<Layers size={16}/> },
  { id:'anomalies',     label:'Anomalies',       icon:<AlertTriangle size={16}/> },
  { id:'recommendations',label:'Recommendations',icon:<GitPullRequest size={16}/> },
  { id:'forecast',      label:'Forecast',        icon:<TrendingUp size={16}/> },
  { id:'resources',     label:'Resources',       icon:<Server size={16}/> },
]

// ── Root App ──────────────────────────────────────────────────────────────────
export default function App() {
  const [page, setPage] = useState('overview')
  const [badge, setBadge] = useState({})

  useEffect(() => {
    api.getOverview().then(o => {
      setBadge({
        anomalies:       o.open_anomalies,
        recommendations: o.open_recommendations,
      })
    }).catch(()=>{})
  }, [])

  const PAGES = {
    overview:        <OverviewPage/>,
    kubernetes:      <KubernetesPage/>,
    anomalies:       <AnomaliesPage/>,
    recommendations: <RecommendationsPage/>,
    forecast:        <ForecastPage/>,
    resources:       <ResourcesPage/>,
  }

  return (
    <div style={{ background:C.bg, minHeight:'100vh', color:C.text, fontFamily:"'IBM Plex Sans', sans-serif", display:'flex' }}>
      <style>{`
        * { box-sizing: border-box; margin: 0; padding: 0; }
        @keyframes spin { to { transform: rotate(360deg); } }
        ::-webkit-scrollbar { width: 6px; height: 6px; }
        ::-webkit-scrollbar-track { background: ${C.bg}; }
        ::-webkit-scrollbar-thumb { background: ${C.muted}; border-radius: 3px; }
        button { font-family: inherit; }
      `}</style>

      {/* Sidebar */}
      <div style={{
        width: 220, background: C.surface, borderRight:`1px solid ${C.border}`,
        display:'flex', flexDirection:'column', flexShrink:0, position:'sticky', top:0, height:'100vh'
      }}>
        {/* Logo */}
        <div style={{ padding:'20px 20px 16px', borderBottom:`1px solid ${C.border}` }}>
          <div style={{ display:'flex', alignItems:'center', gap:8 }}>
            <div style={{
              width:28, height:28, borderRadius:6,
              background:`linear-gradient(135deg, ${C.accent}, ${C.cyan})`,
              display:'flex', alignItems:'center', justifyContent:'center',
            }}>
              <Eye size={14} color="#fff"/>
            </div>
            <div>
              <div style={{ fontSize:14, fontWeight:700, color:C.text, lineHeight:1 }}>ObsEngine</div>
              <div style={{ fontSize:10, color:C.dim }}>Cloud Observability</div>
            </div>
          </div>
        </div>

        {/* Nav items */}
        <nav style={{ padding:'12px 10px', flex:1 }}>
          {NAV.map(n => {
            const active = page === n.id
            const b = badge[n.id]
            return (
              <button key={n.id} onClick={() => setPage(n.id)} style={{
                width:'100%', display:'flex', alignItems:'center', justifyContent:'space-between',
                padding:'9px 12px', borderRadius:7, marginBottom:2, cursor:'pointer',
                background: active ? `${C.accent}22` : 'transparent',
                border: `1px solid ${active ? C.accent+'44' : 'transparent'}`,
                color: active ? C.accent : C.dim,
                fontWeight: active ? 600 : 400, fontSize: 13,
                transition:'all 0.12s',
              }}>
                <span style={{ display:'flex', alignItems:'center', gap:8 }}>{n.icon}{n.label}</span>
                {b > 0 && (
                  <span style={{
                    background: b > 0 ? C.red : C.dim, color:'#fff',
                    borderRadius:10, padding:'1px 6px', fontSize:10, fontWeight:700,
                  }}>{b}</span>
                )}
              </button>
            )
          })}
        </nav>

        {/* Status indicator */}
        <div style={{ padding:'12px 20px', borderTop:`1px solid ${C.border}` }}>
          <div style={{ display:'flex', alignItems:'center', gap:6, fontSize:11, color:C.dim }}>
            <div style={{ width:6, height:6, borderRadius:'50%', background:C.green,
              boxShadow:`0 0 6px ${C.green}` }}/>
            Demo Mode Active
          </div>
          <div style={{ fontSize:10, color:C.muted, marginTop:2 }}>
            Set DEMO_MODE=false + credentials for live data
          </div>
        </div>
      </div>

      {/* Main content */}
      <div style={{ flex:1, overflow:'auto' }}>
        <div style={{ padding:'28px 32px', maxWidth:1400 }}>
          <div style={{ marginBottom:24 }}>
            <h1 style={{ fontSize:22, fontWeight:700, color:C.text, marginBottom:4 }}>
              {NAV.find(n=>n.id===page)?.label}
            </h1>
            <div style={{ height:2, width:40, background:`linear-gradient(90deg,${C.accent},${C.cyan})`, borderRadius:2 }}/>
          </div>
          {PAGES[page]}
        </div>
      </div>
    </div>
  )
}
