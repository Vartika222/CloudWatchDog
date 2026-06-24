# Cloud Observability Engine

> Self-hosted cloud cost intelligence for engineering teams. Detects cost anomalies, attributes spend to individual Kubernetes pods, and delivers fixes as Slack alerts and GitHub PRs — not dashboard notifications nobody checks.

---

## What this actually is

Most cloud cost tools show you the problem. This one fixes it.

When a cost spike happens, your `#cloud-costs` Slack channel gets a message with the z-score, the dollar impact, and a button that opens a GitHub PR with the Terraform/Helm/Bicep patch already written. When a resource has no owner tag, Slack asks who owns it. When a PR touches infrastructure files, a GitHub Action comments with the estimated cost delta using your actual utilization history — not just a price book.

The dashboard exists as a detail view. Slack is the product.

**Currently running on demo data.** Everything is wired correctly. Swap the data sources to go live.

---

## Start it

```bash
docker compose up --build
```

Wait ~60 seconds for seeding. Then:

| Surface | URL |
|---|---|
| Dashboard | http://localhost:3000 |
| API | http://localhost:8080/api/v1/overview |
| VictoriaMetrics | http://localhost:8428 |

---

## What you'll see

**Overview** — 30-day spend across AWS + Azure (~$400K/month demo), daily cost chart, week-over-week movers, provider split.

**Anomalies** — 5 pre-seeded. The data transfer spike (day 60, z-score 4.82) was caught by the rolling 14-day z-score engine. The memory leak in `recommendations/rec-model-01` was caught by the Pearson correlation detector — it uses PostgreSQL's `CORR()` across 12 hours of hourly memory readings.

**Recommendations** — 6 recommendations totalling ~$1,350/month. Each has a working IaC patch. The rightsizing rec explains why m5.large is safe (NIC count validated, IOPS checked). The zone routing rec explains the HPA conflict risk. The VPA comparison shows where Kubernetes VPA would give dangerous advice.

**Kubernetes** — Namespace cost map, pod-level breakdown, egress flows by type. The payments namespace shows 650GB/day of cross-AZ traffic at $0.01/GB — the zone routing recommendation comes from this.

**Resources** — Full inventory. Orphan filter shows the unattached EBS volume ($40/mo) and unused Public IP ($3.65/mo).

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                  DATA SOURCES                        │
│  AWS Cost Explorer  │  Azure Cost Management        │
│  Pixie eBPF         │  CloudTrail  │  GitHub API    │
└──────────────┬──────────────────────────────────────┘
               │  normalized to FOCUS schema
┌──────────────▼──────────────────────────────────────┐
│               INTELLIGENCE ENGINE                    │
│  collector (Go)  — billing, orphans, z-score        │
│  attributor (Py) — pod attribution, leak detection, │
│                    tag inference, VPA comparison,    │
│                    commitment optimizer, eff. score  │
│  PostgreSQL      — single source of truth           │
│  VictoriaMetrics — time-series metrics              │
└──────────────┬──────────────────────────────────────┘
               │
┌──────────────▼──────────────────────────────────────┐
│           DELIVERY SURFACE  ← this is the product   │
│  notifier (Phase 3)  — Slack anomaly alerts,        │
│                         tag ownership prompts,       │
│                         weekly digest               │
│  pr-generator (Phase 3) — GitHub PRs from patches   │
│  github-action (Phase 3) — PR cost comment          │
│  api (Go) — 22 REST endpoints                       │
└──────────────┬──────────────────────────────────────┘
               │  detail view (opened from Slack link)
┌──────────────▼──────────────────────────────────────┐
│              DASHBOARD (React + Recharts)            │
└─────────────────────────────────────────────────────┘
```

### Services

| Service | Stack | Port | Role |
|---|---|---|---|
| `postgres` | PostgreSQL 16 | 5432 | Single source of truth |
| `victoriametrics` | VictoriaMetrics | 8428 | Time-series from Pixie |
| `redis` | Redis 7 | 6379 | Dedup cache for Slack alerts |
| `seeder` | Python | — | Loads 90 days of demo data, runs once |
| `collector` | Go | — | Billing ingestion + anomaly detection |
| `attributor` | Python | — | Pod attribution + all analysis |
| `api` | Go + chi | 8080 | 22 REST endpoints |
| `dashboard` | React + Vite | 3000 | Detail view |

---

## Connecting real data

### Kubernetes metrics via Pixie (free, no credit card)

```bash
# Local cluster
brew install kind kubectl
kind create cluster --name obs-demo

# Real microservices app with genuine traffic
kubectl apply -f https://raw.githubusercontent.com/GoogleCloudPlatform/microservices-demo/main/release/kubernetes-manifests.yaml

# Install Pixie free tier (sign up at px.dev)
bash -c "$(curl -fsSL https://withpixie.ai/install.sh)"
px deploy

# Run these — their output is what PixieClient should return
px run px/pod_lifetime_resource_usage
px run px/net_flow_graph
```

In `docker-compose.yml`:
```yaml
attributor:
  environment:
    PIXIE_ENABLED: "true"
    PIXIE_ADDR: "your-cluster-addr:443"
    DEMO_MODE: "false"
```

Then implement `PixieClient.get_pod_resource_usage()` in `attributor/main.py`. The stub is there with a `NotImplementedError` and comments explaining the PxL queries to use.

### AWS billing (Free Tier — $0)

```yaml
collector:
  environment:
    AWS_ENABLED: "true"
    DEMO_MODE: "false"
    AWS_ACCESS_KEY_ID: "your-key"
    AWS_SECRET_ACCESS_KEY: "your-secret"
    AWS_DEFAULT_REGION: "us-east-1"
```

Required IAM:
```json
{
  "Action": ["ce:GetCostAndUsage", "ce:GetReservationCoverage",
             "ec2:DescribeInstances", "ec2:DescribeVolumes", "ec2:DescribeAddresses"]
}
```

Implement `collectAWS()` in `collector/main.go` — the stub is there with `// TODO` comments.

### Azure billing (Free Tier)

```yaml
collector:
  environment:
    AZURE_ENABLED: "true"
    DEMO_MODE: "false"
    AZURE_SUBSCRIPTION_ID: "your-sub-id"
    AZURE_TENANT_ID: "your-tenant-id"
    AZURE_CLIENT_ID: "your-client-id"
    AZURE_CLIENT_SECRET: "your-secret"
```

Required RBAC: `Cost Management Reader` + `Reader` on the subscription.

### GitHub PR generation (works now, no extra service needed)

```yaml
api:
  environment:
    GITHUB_TOKEN: "ghp_your_token"
    GITHUB_REPO: "your-org/your-infra-repo"
```

Test it immediately:
```bash
# Get a recommendation ID from the API
curl localhost:8080/api/v1/recommendations | jq '.[0].id'

# Open a real PR
curl -X POST localhost:8080/api/v1/recommendations/{id}/open-pr
```

### Slack alerts (notifier — next to build)

```yaml
notifier:
  environment:
    SLACK_WEBHOOK_URL: "https://hooks.slack.com/services/..."
    SLACK_CHANNEL: "#cloud-costs"
```

The notifier service doesn't exist yet. It's the next thing to build — ~150 lines of Go that polls `anomalies WHERE notified_at IS NULL` and posts Block Kit messages with action buttons to Slack.

---

## API reference

```
GET  /health

# Spend
GET  /api/v1/overview
GET  /api/v1/costs/daily           ?days=30&provider=aws
GET  /api/v1/costs/services
GET  /api/v1/costs/movers
GET  /api/v1/costs/forecast
GET  /api/v1/costs/estimate        ?changed_files=main.tf,values.yaml

# Kubernetes
GET  /api/v1/kubernetes/costmap
GET  /api/v1/kubernetes/pods       ?namespace=payments
GET  /api/v1/kubernetes/egress
GET  /api/v1/kubernetes/vpa

# Anomalies
GET  /api/v1/anomalies             ?status=open
PATCH /api/v1/anomalies/{id}

# Recommendations
GET  /api/v1/recommendations       ?type=rightsize
PATCH /api/v1/recommendations/{id}
POST /api/v1/recommendations/{id}/open-pr   ← opens a real GitHub PR

# Resources + inventory
GET  /api/v1/resources             ?orphan=true

# Tag ownership
GET  /api/v1/tags/coverage
GET  /api/v1/tags/inferences       ← pending ownership prompts for Slack
PATCH /api/v1/tags/inferences/{id} {"accepted":true,"accepted_by":"U123"}

# Intelligence
GET  /api/v1/commitments           ← portfolio optimizer output
GET  /api/v1/efficiency            ← composite score 0-100, 30-day history
GET  /api/v1/tenants
```

---

## How the detection works

**Billing anomaly** — Rolling 14-day z-score in SQL. `z = (today − rolling_avg) / rolling_stddev`. Threshold: ±2.5 standard deviations. Cloud billing is noisier than application metrics so 3.0 would miss real spikes.

**Memory leak** — `CORR()` (Pearson correlation) between Unix timestamps and memory readings over 12 hours. Correlation > 0.85 AND growth > 50MB AND pod at >70% of memory request = leak. Trend-based, not threshold-based, so it catches slow leaks before they OOMKill.

**Egress waste** — Cross-AZ bytes as percentage of total namespace traffic. Above 40% is an architectural problem. Fix: topology-aware routing (`service.kubernetes.io/topology-mode: Auto`).

**VPA conflict** — Checks if HPA is active on the same deployment before accepting VPA's CPU request reduction. VPA + HPA causes pod thrashing: lower CPU requests → HPA threshold changes → more replicas → higher cost. VPA has no cost model and no HPA awareness. We do.

**Tag inference** — 16 regex rules against resource names. Matches written to `inferred_tags` with confidence scores. High-confidence pending inferences surfaced via Slack for confirmation. Accepted inferences write the tag back to `resources.tags`.

**Efficiency score** — Composite 0–100:
- Utilization efficiency (35%): avg `cpu_used / cpu_request` across pods, last 7 days
- Allocation coverage (25%): % of spend with `team` tag
- Commitment utilization (20%): % of spend on non-on-demand resources  
- Hygiene (20%): penalizes orphan cost % and cross-AZ traffic ratio

Tiers: Elite (90+) · Good (70–89) · Fair (50–69) · Poor (<50)

---

## Schema overview

| Table | Holds |
|---|---|
| `billing_records` | FOCUS-normalized billing, any provider |
| `daily_cost_summary` | Aggregated daily costs — what the API queries |
| `resources` | Inventory: NIC count, IOPS, commitment type, orphan flag |
| `pod_cost_attribution` | Per-pod per-hour: CPU, memory, egress, dollar cost |
| `egress_flows` | Pod-to-pod flows with AZ classification |
| `anomalies` | Detected anomalies, z-scores, Slack notification state |
| `recommendations` | Recs with IaC patches, PR URL, PR status |
| `inferred_tags` | Tag ownership suggestions, Slack confirmation state |
| `efficiency_scores` | Daily composite score history |
| `commitment_recommendations` | Portfolio optimizer output |
| `vpa_recommendations` | VPA vs ours, disagreement reasons |
| `tenants` | Multi-tenant isolation, MSP parent/child |

---

## What's being built next

1. **`PixieClient.get_pod_resource_usage()`** — real pod metrics. The stub is in `attributor/main.py`.
2. **`notifier` service** — Slack anomaly alerts + tag ownership prompts. ~150 lines Go.
3. **`collectAWS()`** — real billing. The stub is in `collector/main.go`.
4. **GitHub Action** — cost comment on PRs. The API endpoint `/costs/estimate` is ready.
5. **CloudTrail integration** — correlate cost spikes with API events (cost archaeology).
6. **Prophet forecasting** — uncomment in `requirements.txt` after 90 days of real data.