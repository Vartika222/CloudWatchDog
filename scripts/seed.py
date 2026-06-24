#!/usr/bin/env python3
"""
Seed script: generates 90 days of realistic cloud billing data,
Kubernetes pod attribution, egress flows, anomalies, and recommendations.
Runs once on first boot. Safe to re-run (idempotent).
"""

import os, sys, random, json, uuid, math
from datetime import datetime, timedelta, date
from decimal import Decimal
import psycopg2
from psycopg2.extras import execute_values, RealDictCursor
import urllib.request

DB_DSN  = os.getenv("DB_DSN", "postgres://oe_user:oe_pass@localhost:5432/observability")
VM_URL  = os.getenv("VM_URL", "http://localhost:8428")

random.seed(42)

# ── Helpers ──────────────────────────────────────────────────────────────────
def rnd(lo, hi, decimals=4):
    return round(random.uniform(lo, hi), decimals)

def jitter(val, pct=0.1):
    return val * (1 + random.uniform(-pct, pct))

# ── Config ───────────────────────────────────────────────────────────────────
PROVIDERS = {
    "aws": {
        "account_id": "123456789012",
        "account_name": "prod-main",
        "regions": ["us-east-1", "us-west-2", "eu-west-1"],
        "services": {
            "EC2":         {"base_cost": 4200, "unit": "Hrs",    "category": "Compute"},
            "RDS":         {"base_cost": 1800, "unit": "Hrs",    "category": "Database"},
            "S3":          {"base_cost":  620, "unit": "GB-Mo",  "category": "Storage"},
            "CloudFront":  {"base_cost":  340, "unit": "GB",     "category": "Networking"},
            "EKS":         {"base_cost": 2100, "unit": "Hrs",    "category": "Compute"},
            "Lambda":      {"base_cost":  180, "unit": "Req",    "category": "Compute"},
            "ElastiCache": {"base_cost":  420, "unit": "Hrs",    "category": "Database"},
            "NAT Gateway": {"base_cost":  890, "unit": "GB",     "category": "Networking"},
            "Data Transfer":{"base_cost": 1100,"unit": "GB",     "category": "Networking"},
            "EBS":         {"base_cost":  560, "unit": "GB-Mo",  "category": "Storage"},
        }
    },
    "azure": {
        "account_id": "sub-a1b2c3d4-prod",
        "account_name": "azure-prod-sub",
        "regions": ["eastus", "westeurope", "southeastasia"],
        "services": {
            "Virtual Machines":    {"base_cost": 3800, "unit": "Hrs",   "category": "Compute"},
            "AKS":                 {"base_cost": 1900, "unit": "Hrs",   "category": "Compute"},
            "Azure SQL":           {"base_cost": 1400, "unit": "DTUs",  "category": "Database"},
            "Blob Storage":        {"base_cost":  480, "unit": "GB-Mo", "category": "Storage"},
            "Azure CDN":           {"base_cost":  220, "unit": "GB",    "category": "Networking"},
            "Azure Functions":     {"base_cost":  140, "unit": "Req",   "category": "Compute"},
            "Cosmos DB":           {"base_cost":  760, "unit": "RU",    "category": "Database"},
            "VPN Gateway":         {"base_cost":  310, "unit": "Hrs",   "category": "Networking"},
            "Bandwidth":           {"base_cost":  640, "unit": "GB",    "category": "Networking"},
            "Managed Disks":       {"base_cost":  390, "unit": "GB-Mo", "category": "Storage"},
        }
    }
}

NAMESPACES = [
    ("checkout",       "checkout-team",    0.18),
    ("payments",       "payments-team",    0.22),
    ("catalog",        "platform-team",    0.12),
    ("recommendations","ml-team",          0.15),
    ("user-service",   "platform-team",    0.10),
    ("notification",   "platform-team",    0.06),
    ("analytics",      "data-team",        0.11),
    ("monitoring",     "sre-team",         0.06),
]

PODS_PER_NS = {
    "checkout":        ["checkout-api-{}", "checkout-worker-{}", "checkout-cache-{}"],
    "payments":        ["payments-api-{}", "payments-processor-{}", "payments-fraud-{}"],
    "catalog":         ["catalog-api-{}", "catalog-search-{}", "catalog-sync-{}"],
    "recommendations": ["rec-model-{}", "rec-api-{}", "rec-featurestore-{}"],
    "user-service":    ["user-api-{}", "user-auth-{}", "user-profile-{}"],
    "notification":    ["notif-sender-{}", "notif-scheduler-{}"],
    "analytics":       ["analytics-ingest-{}", "analytics-query-{}", "analytics-export-{}"],
    "monitoring":      ["prom-server-{}", "alertmanager-{}", "grafana-{}"],
}

# ── DB Connection ─────────────────────────────────────────────────────────────
print("Connecting to database...")
conn = psycopg2.connect(DB_DSN)
conn.autocommit = False
cur = conn.cursor(cursor_factory=RealDictCursor)

# Check if already seeded
cur.execute("SELECT COUNT(*) as c FROM billing_records")
count = cur.fetchone()["c"]
if count > 0:
    print(f"Database already seeded ({count} billing records). Skipping.")
    sys.exit(0)

print("Seeding database with 90 days of realistic data...")

today = date.today()
start_date = today - timedelta(days=90)

# ── 1. Seed billing records ───────────────────────────────────────────────────
print("  → Seeding billing records...")
billing_rows = []

for provider_name, pconf in PROVIDERS.items():
    for svc_name, svc in pconf["services"].items():
        base = svc["base_cost"]
        for day_offset in range(90):
            day = start_date + timedelta(days=day_offset)
            dow = day.weekday()  # 0=Mon, 6=Sun

            # Seasonality: weekends ~70%, gradual growth trend
            weekend_factor = 0.70 if dow >= 5 else 1.0
            growth_factor  = 1 + (day_offset / 90) * 0.12  # 12% growth over 90 days
            noise          = jitter(1.0, 0.08)

            # Inject a cost spike anomaly around day 60 for "Data Transfer"/"Bandwidth"
            spike = 1.0
            if svc_name in ("Data Transfer", "Bandwidth") and 58 <= day_offset <= 62:
                spike = 3.2

            daily_cost = base / 30 * weekend_factor * growth_factor * noise * spike
            region = random.choice(pconf["regions"])

            billing_rows.append((
                str(uuid.uuid4()),
                pconf["account_id"],
                pconf["account_name"],
                provider_name,
                svc_name,
                svc["category"],
                region,
                f"{region}a" if provider_name == "aws" else None,
                f"/{provider_name}/resource/{svc_name.lower().replace(' ','-')}-prod-01",
                f"{svc_name.lower().replace(' ','-')}-prod-01",
                svc_name,
                round(daily_cost, 6),
                round(daily_cost * 0.92, 6),  # effective after RI
                round(daily_cost * 1.15, 6),  # list price
                round(daily_cost * 0.92, 6),
                "USD",
                datetime.combine(day, datetime.min.time()),
                datetime.combine(day + timedelta(days=1), datetime.min.time()),
                datetime.combine(day, datetime.min.time()),
                datetime.combine(day + timedelta(days=1), datetime.min.time()),
                round(daily_cost / 0.05, 2),
                svc["unit"],
                json.dumps({"team": random.choice(["platform","payments","data","ml"]),
                           "env": "production", "cost-center": "engineering"}),
                None,
            ))

execute_values(cur, """
    INSERT INTO billing_records (
        id, billing_account_id, billing_account_name, provider,
        service_name, service_category, region, availability_zone,
        resource_id, resource_name, resource_type,
        billed_cost, effective_cost, list_cost, amortized_cost, currency,
        billing_period_start, billing_period_end,
        usage_start, usage_end,
        usage_quantity, usage_unit, tags, raw_record
    ) VALUES %s
""", billing_rows)
print(f"     {len(billing_rows)} billing records inserted")

# ── 2. Daily cost summary ─────────────────────────────────────────────────────
print("  → Building daily cost summaries...")
cur.execute("""
    INSERT INTO daily_cost_summary (summary_date, provider, account_id, service_name, region, total_cost, resource_count)
    SELECT
        DATE(usage_start) as summary_date,
        provider,
        billing_account_id as account_id,
        service_name,
        COALESCE(region, 'global') as region,
        SUM(billed_cost) as total_cost,
        COUNT(DISTINCT resource_id) as resource_count
    FROM billing_records
    GROUP BY DATE(usage_start), provider, billing_account_id, service_name, COALESCE(region, 'global')
    ON CONFLICT (summary_date, provider, account_id, service_name, region) DO NOTHING
""")

# ── 3. Resources ──────────────────────────────────────────────────────────────
print("  → Seeding resources...")
resource_rows = []
resource_types = {
    "aws": [
        ("ec2_instance", "m5.xlarge",  4,  16,  None,  2,  "reserved_1yr"),
        ("ec2_instance", "m5.2xlarge", 8,  32,  None,  4,  "on_demand"),
        ("ec2_instance", "r5.large",   2,  16,  None,  2,  "on_demand"),
        ("rds_instance", "db.r5.large",2,  16,  100,   1,  "reserved_1yr"),
        ("ebs_volume",   "gp3",        None, None, 500, None,"on_demand"),
        ("ebs_volume",   "gp3",        None, None, 200, None,"on_demand"),  # orphan
        ("elastic_ip",   None,         None, None, None,None,"on_demand"),  # orphan
        ("nat_gateway",  None,         None, None, None,None,"on_demand"),
        ("eks_cluster",  None,         None, None, None,None,"on_demand"),
    ],
    "azure": [
        ("virtual_machine", "Standard_D4s_v3", 4,  16,  None, 2, "reserved_1yr"),
        ("virtual_machine", "Standard_D8s_v3", 8,  32,  None, 4, "on_demand"),
        ("virtual_machine", "Standard_E4s_v3", 4,  32,  None, 2, "on_demand"),
        ("sql_database",    "GP_Gen5_4",        4,  20.4, 100, 1, "on_demand"),
        ("managed_disk",    "P30",              None,None, 1024,None,"on_demand"),
        ("managed_disk",    "P20",              None,None, 512, None,"on_demand"),  # orphan
        ("public_ip",       "static",           None,None, None,None,"on_demand"),  # orphan
        ("aks_cluster",     None,               None,None, None,None,"on_demand"),
    ]
}

for provider_name, pconf in PROVIDERS.items():
    for i, (rtype, rsku, cpu, mem, stor, nics, commit) in enumerate(resource_types[provider_name]):
        rid = f"/{provider_name}/{rtype}/{i:04d}"
        is_orphan = rtype in ("ebs_volume", "managed_disk", "elastic_ip", "public_ip") and i % 3 == 0
        last_active = (datetime.now() - timedelta(days=random.randint(45, 90))) if is_orphan else datetime.now()

        resource_rows.append((
            str(uuid.uuid4()),
            provider_name,
            pconf["account_id"],
            rid,
            f"{rtype}-prod-{i:02d}",
            rtype,
            random.choice(pconf["regions"]),
            None,
            "active" if not is_orphan else "idle",
            json.dumps({"sku": rsku} if rsku else {}),
            json.dumps({"env": "production", "team": "platform"}),
            rnd(80, 180, 2),
            rnd(18, 45, 2),
            rnd(2, 12, 2),
            cpu, mem, stor, nics,
            commit, None,
            last_active,
            is_orphan,
            "No activity in 60+ days" if is_orphan else None,
        ))

execute_values(cur, """
    INSERT INTO resources (
        id, provider, account_id, resource_id, resource_name, resource_type,
        region, availability_zone, status, configuration, tags,
        cost_30d, cost_7d, cost_yesterday,
        cpu_cores, memory_gb, storage_gb, nic_count,
        commitment_type, commitment_end,
        last_active_at, is_orphan, orphan_reason
    ) VALUES %s
    ON CONFLICT (provider, account_id, resource_id) DO NOTHING
""", resource_rows)
print(f"     {len(resource_rows)} resources inserted")

# ── 4. K8s clusters ───────────────────────────────────────────────────────────
print("  → Seeding Kubernetes clusters...")
aws_cluster_id   = str(uuid.uuid4())
azure_cluster_id = str(uuid.uuid4())

execute_values(cur, """
    INSERT INTO k8s_clusters (id, provider, account_id, cluster_name, region, node_count, total_cost_30d, pixie_enabled)
    VALUES %s ON CONFLICT (provider, account_id, cluster_name) DO NOTHING
""", [
    (aws_cluster_id,   "aws",   "123456789012",  "prod-eks-us-east-1",  "us-east-1", 12, 8400.0, True),
    (azure_cluster_id, "azure", "sub-a1b2c3d4-prod", "prod-aks-eastus", "eastus",    10, 7200.0, True),
])

# ── 5. Pod attribution ────────────────────────────────────────────────────────
print("  → Seeding pod cost attribution (30 days)...")
pod_rows = []

for cluster_id in [aws_cluster_id, azure_cluster_id]:
    cluster_base_cost = 8400.0 if cluster_id == aws_cluster_id else 7200.0
    for day_offset in range(30):
        day = today - timedelta(days=30 - day_offset)
        for ns_name, team, cost_share in NAMESPACES:
            ns_daily = cluster_base_cost / 30 * cost_share
            for pod_tmpl in PODS_PER_NS.get(ns_name, [f"{ns_name}-app-{{}}"]):
                for replica in range(1, 3):
                    pod_name = pod_tmpl.format(f"{replica:02x}")
                    pod_cost = ns_daily / (len(PODS_PER_NS.get(ns_name, ["x"])) * 2)
                    cpu_req  = rnd(0.1, 1.0)
                    cpu_used = cpu_req * rnd(0.15, 0.75)
                    mem_req  = int(rnd(128, 1024) * 1024 * 1024)
                    mem_used = int(mem_req * rnd(0.20, 0.80))

                    # Simulate memory leak in recommendations ns near end
                    if ns_name == "recommendations" and day_offset > 22:
                        mem_used = int(mem_req * (0.6 + (day_offset - 22) * 0.04))

                    # Simulate cross-AZ egress spike in payments
                    inter_az_bytes = 0
                    if ns_name == "payments":
                        inter_az_bytes = int(rnd(50, 500) * 1024 * 1024)  # 50-500 MB

                    inet_bytes  = int(rnd(1, 50) * 1024 * 1024)
                    egress_ia   = inter_az_bytes * 0.01 / (1024**3)   # $0.01/GB
                    egress_inet = inet_bytes     * 0.09 / (1024**3)   # $0.09/GB

                    pod_rows.append((
                        str(uuid.uuid4()),
                        cluster_id, ns_name, pod_name,
                        ns_name, "Deployment", f"{ns_name}-deployment",
                        f"node-{random.randint(1,12):02d}",
                        random.choice(["us-east-1a","us-east-1b","us-east-1c"]),
                        datetime.combine(day, datetime.min.time()),
                        datetime.combine(day + timedelta(days=1), datetime.min.time()),
                        round(cpu_req,6), round(cpu_req*1.5,6), round(cpu_used,6),
                        mem_req, int(mem_req*1.5), mem_used,
                        int(rnd(100,1000)*1024*1024), inter_az_bytes, inet_bytes,
                        int(rnd(50,500)*1024*1024),
                        round(pod_cost * 0.6,  6),
                        round(pod_cost * 0.3,  6),
                        round(egress_ia,  6),
                        round(egress_inet, 6),
                        round(pod_cost * 0.9 + egress_ia + egress_inet, 6),
                        "usage_ratio",
                        json.dumps({"team": team, "app": ns_name}),
                    ))

execute_values(cur, """
    INSERT INTO pod_cost_attribution (
        id, cluster_id, namespace, pod_name, service_name,
        owner_kind, owner_name, node_name, availability_zone,
        window_start, window_end,
        cpu_request_cores, cpu_limit_cores, cpu_used_cores,
        memory_request_bytes, memory_limit_bytes, memory_used_bytes,
        bytes_tx_intra_az, bytes_tx_inter_az, bytes_tx_internet, bytes_rx_total,
        compute_cost, memory_cost, egress_cost_inter_az, egress_cost_internet, total_cost,
        attribution_method, labels
    ) VALUES %s
""", pod_rows)
print(f"     {len(pod_rows)} pod attribution records inserted")

# ── 6. Namespace daily cost ───────────────────────────────────────────────────
print("  → Building namespace daily cost summaries...")
cur.execute("""
    INSERT INTO namespace_daily_cost (summary_date, cluster_id, namespace, compute_cost, memory_cost, egress_cost, total_cost, pod_count)
    SELECT
        DATE(window_start),
        cluster_id,
        namespace,
        SUM(compute_cost),
        SUM(memory_cost),
        SUM(egress_cost_inter_az + egress_cost_internet),
        SUM(total_cost),
        COUNT(DISTINCT pod_name)
    FROM pod_cost_attribution
    GROUP BY DATE(window_start), cluster_id, namespace
    ON CONFLICT (summary_date, cluster_id, namespace) DO NOTHING
""")

# ── 7. Egress flows ───────────────────────────────────────────────────────────
print("  → Seeding egress flows...")
egress_rows = []
for cluster_id in [aws_cluster_id, azure_cluster_id]:
    for day_offset in range(7):
        day = today - timedelta(days=7 - day_offset)
        # Payments → external payment processor (inter-AZ, expensive)
        egress_rows.append((
            str(uuid.uuid4()), cluster_id,
            "payments", "payments-api-01", "payments-api",
            None, None, None, "54.239.28.100",
            "inter_az",
            int(rnd(200, 800) * 1024 * 1024 * 1024),  # 200-800 GB
            int(rnd(50000, 200000)),
            round(rnd(2, 8), 4),
            datetime.combine(day, datetime.min.time()),
            datetime.combine(day + timedelta(days=1), datetime.min.time()),
        ))
        # Recommendations → S3/Blob (internet egress, large model files)
        egress_rows.append((
            str(uuid.uuid4()), cluster_id,
            "recommendations", "rec-model-01", "rec-model",
            None, None, None, "52.216.1.100",
            "internet",
            int(rnd(50, 300) * 1024 * 1024 * 1024),
            int(rnd(1000, 5000)),
            round(rnd(4, 27), 4),
            datetime.combine(day, datetime.min.time()),
            datetime.combine(day + timedelta(days=1), datetime.min.time()),
        ))
        # Intra-AZ (free)
        egress_rows.append((
            str(uuid.uuid4()), cluster_id,
            "checkout", "checkout-api-01", "checkout-api",
            "payments", "payments-api-01", "payments-api",
            None,
            "intra_az",
            int(rnd(10, 100) * 1024 * 1024 * 1024),
            int(rnd(100000, 500000)),
            0.0,
            datetime.combine(day, datetime.min.time()),
            datetime.combine(day + timedelta(days=1), datetime.min.time()),
        ))

execute_values(cur, """
    INSERT INTO egress_flows (
        id, cluster_id, src_namespace, src_pod, src_service,
        dst_namespace, dst_pod, dst_service, dst_ip,
        flow_type, bytes_total, request_count, egress_cost,
        window_start, window_end
    ) VALUES %s
""", egress_rows)
print(f"     {len(egress_rows)} egress flow records inserted")

# ── 8. Anomalies ──────────────────────────────────────────────────────────────
print("  → Seeding anomalies...")
anomaly_rows = [
    (str(uuid.uuid4()), "cost_spike",    "high",    "aws",   "123456789012",
     None, "Data Transfer", None, None,
     datetime.now() - timedelta(days=30), "billed_cost_daily",
     3240.50, 980.20, 230.6, 4.82,
     "Data transfer costs spiked 230% above 14-day baseline. Detected on day 60 of the billing period. Possible misconfigured NAT gateway or unexpected egress from EKS cluster.",
     2260.30, None, None, "open", '{}'),

    (str(uuid.uuid4()), "memory_leak",   "high",    "aws",   "123456789012",
     None, None, "recommendations", "rec-model-01",
     datetime.now() - timedelta(hours=6), "memory_used_bytes",
     892000000, 420000000, 112.4, 3.94,
     "Gradual memory growth detected in rec-model-01 (recommendations namespace). Memory usage increasing ~4% per day over last 8 days. Pattern consistent with memory leak in model serving layer.",
     140.0, None, None, "open", '{}'),

    (str(uuid.uuid4()), "egress_spike",  "medium",  "azure", "sub-a1b2c3d4-prod",
     None, None, "payments", "payments-processor-01",
     datetime.now() - timedelta(days=2), "bytes_tx_inter_az",
     650000000000, 180000000000, 261.1, 3.21,
     "Cross-availability-zone traffic from payments namespace increased 261% vs prior week baseline. Payments processor appears to be communicating with a database replica in a different AZ. Consider enabling zone-aware routing.",
     58.50, None, None, "open", '{}'),

    (str(uuid.uuid4()), "cost_spike",    "medium",  "azure", "sub-a1b2c3d4-prod",
     None, "Cosmos DB", None, None,
     datetime.now() - timedelta(days=5), "billed_cost_daily",
     142.80, 87.40, 63.4, 2.41,
     "Cosmos DB daily cost increased 63% vs 14-day baseline. Possible hot partition or missing index causing excessive RU consumption.",
     55.40, None, None, "acknowledged", '{}'),

    (str(uuid.uuid4()), "cpu_anomaly",   "low",     "aws",   "123456789012",
     None, "EC2", None, None,
     datetime.now() - timedelta(hours=18), "cpu_used_cores",
     0.04, 1.8, -97.8, -4.1,
     "EC2 instance group showing near-zero CPU utilization for 72+ hours. Instance may be a candidate for rightsizing or termination. No active connections detected.",
     None, None, None, "open", '{}'),
]

execute_values(cur, """
    INSERT INTO anomalies (
        id, anomaly_type, severity, provider, account_id,
        resource_id, resource_name, namespace, pod_name,
        detected_at, metric_name,
        metric_value, baseline_value, deviation_pct, zscore,
        description, impact_usd, resolved_at, resolution_note, status, raw_context
    ) VALUES %s
""", anomaly_rows)
print(f"     {len(anomaly_rows)} anomalies inserted")

# ── 9. Recommendations ────────────────────────────────────────────────────────
print("  → Seeding recommendations...")
rec_rows = [
    (str(uuid.uuid4()), "rightsize", "high", "aws", "123456789012",
     "/aws/ec2_instance/0001", "m5.2xlarge-prod-01", "ec2_instance",
     None,
     "Rightsize m5.2xlarge → m5.large (72h avg CPU: 8%)",
     "This EC2 instance has averaged 8% CPU and 22% memory utilization over the past 14 days. Rightsizing from m5.2xlarge to m5.large will reduce cost by 49% with no performance impact. NIC limit check: m5.large supports 3 NICs (current usage: 1). IOPS: no gp3 volume attached.",
     json.dumps({"instance_type": "m5.2xlarge", "cpu_avg_pct": 8.2, "mem_avg_pct": 22.1, "vcpus": 8, "memory_gb": 32}),
     json.dumps({"instance_type": "m5.large",   "cpu_headroom_pct": 38, "mem_headroom_pct": 22, "vcpus": 2, "memory_gb": 8}),
     680.40, 8164.80, 91.0, 14,
     json.dumps({"cpu_p95": 18.4, "cpu_avg": 8.2, "mem_p95": 28.0, "samples": 336}),
     "terraform",
     'resource "aws_instance" "m5_2xlarge_prod_01" {\n  instance_type = "m5.large"  # was m5.2xlarge\n  # estimated saving: $680/month\n}',
     None, None, "open"),

    (str(uuid.uuid4()), "zone_routing", "high", "aws", "123456789012",
     None, None, None, "payments",
     "Enable Topology Aware Routing in payments namespace (saves $58/mo egress)",
     "The payments namespace is generating significant cross-AZ traffic to payment processor backends. Enabling Kubernetes Topology Aware Hints will instruct the scheduler to prefer pods in the same AZ, reducing inter-AZ data transfer by an estimated 70-85%.",
     json.dumps({"inter_az_bytes_7d": 4550000000000, "inter_az_cost_7d": 45.5, "affected_services": ["payments-api","payments-processor"]}),
     json.dumps({"topology_hint": "Auto", "expected_reduction_pct": 78}),
     58.50, 702.0, 78.0, 7,
     json.dumps({"cross_az_gb_daily": 650, "cost_per_gb": 0.01}),
     "helm",
     'service:\n  annotations:\n    service.kubernetes.io/topology-mode: "Auto"\n  # Enables Topology Aware Routing\n  # Estimated saving: $58/month',
     None, None, "open"),

    (str(uuid.uuid4()), "orphan_delete", "medium", "aws", "123456789012",
     "/aws/ebs_volume/0005", "ebs-vol-unattached-prod-05", "ebs_volume",
     None,
     "Delete unattached EBS volume gp3/500GB (idle 67 days)",
     "This EBS gp3 volume (500GB) has not been attached to any EC2 instance for 67 days. It was previously attached to a terminated instance. Monthly cost: $40. There are no snapshots depending on this volume.",
     json.dumps({"volume_id": "/aws/ebs_volume/0005", "size_gb": 500, "type": "gp3", "days_unattached": 67}),
     json.dumps({"action": "delete", "snapshot_first": True}),
     40.0, 480.0, 99.0, 1,
     json.dumps({"last_attached": (today - timedelta(days=67)).isoformat()}),
     "terraform",
     '# Delete unattached volume\n# Run: aws ec2 delete-volume --volume-id vol-0example\n# First create snapshot: aws ec2 create-snapshot --volume-id vol-0example',
     None, None, "open"),

    (str(uuid.uuid4()), "ri_purchase", "high", "azure", "sub-a1b2c3d4-prod",
     "/azure/virtual_machine/0001", "Standard_D8s_v3-prod-01", "virtual_machine",
     None,
     "Purchase 1-yr Reserved Instance for Standard_D8s_v3 (saves $285/mo)",
     "This Azure VM has run continuously for 85+ days with >99% uptime. Purchasing a 1-year Reserved Instance will reduce cost from $580/month to $295/month. Risk assessment: stable workload, no architecture changes planned, appropriate for 1-year commitment.",
     json.dumps({"vm_size": "Standard_D8s_v3", "current_type": "on_demand", "uptime_pct_90d": 99.2, "monthly_cost_od": 580}),
     json.dumps({"commitment": "1yr_reserved", "monthly_cost_ri": 295, "break_even_months": 1.0}),
     285.0, 3420.0, 88.0, 85,
     json.dumps({"uptime_days": 85, "utilization_avg": 61.4}),
     "bicep",
     '// Azure Bicep: Reserved Instance purchase\n// Navigate to: Azure Portal > Reservations > Add\n// Scope: Shared  VM Size: Standard_D8s_v3  Term: 1 Year\n// Estimated saving: $285/month',
     None, None, "open"),

    (str(uuid.uuid4()), "orphan_delete", "medium", "azure", "sub-a1b2c3d4-prod",
     "/azure/public_ip/0006", "pip-orphaned-prod-06", "public_ip",
     None,
     "Release unassociated Public IP (idle 52 days, $3.65/mo)",
     "This static Public IP address has not been associated with any load balancer or virtual machine for 52 days. Azure charges $0.005/hour for unassociated static IPs.",
     json.dumps({"ip_address": "40.112.x.x", "type": "static", "days_unassociated": 52}),
     json.dumps({"action": "release"}),
     3.65, 43.80, 99.0, 1,
     json.dumps({"last_associated": (today - timedelta(days=52)).isoformat()}),
     "bicep",
     "// Release public IP via Azure CLI:\n// az network public-ip delete --name pip-orphaned-prod-06 --resource-group prod-rg",
     None, None, "open"),

    (str(uuid.uuid4()), "rightsize", "medium", "azure", "sub-a1b2c3d4-prod",
     "/azure/virtual_machine/0002", "Standard_D8s_v3-prod-02", "virtual_machine",
     None,
     "Rightsize Standard_D8s_v3 → Standard_D4s_v3 (14d avg CPU: 11%)",
     "VM has averaged 11% CPU and 31% memory over 14 days. Downsize to Standard_D4s_v3 saves 49%. NIC limit verified: D4s_v3 supports 4 NICs (current: 2). No proximity placement group constraints.",
     json.dumps({"vm_size": "Standard_D8s_v3", "cpu_avg_pct": 11.2, "mem_avg_pct": 30.8, "vcpus": 8, "memory_gb": 32}),
     json.dumps({"vm_size": "Standard_D4s_v3", "vcpus": 4, "memory_gb": 16, "nic_limit": 4}),
     284.20, 3410.40, 84.0, 14,
     json.dumps({"cpu_p95": 24.1, "cpu_avg": 11.2, "mem_p95": 38.2, "samples": 336}),
     "bicep",
     '// Resize VM via Azure CLI:\n// az vm resize --resource-group prod-rg --name Standard_D8s_v3-prod-02 --size Standard_D4s_v3',
     None, None, "open"),
]

execute_values(cur, """
    INSERT INTO recommendations (
        id, rec_type, priority, provider, account_id,
        resource_id, resource_name, resource_type, namespace,
        title, description,
        current_config, recommended_config,
        monthly_savings, annual_savings, confidence_pct, evidence_days,
        evidence_data, iac_type, iac_patch, pr_url, pr_status, status
    ) VALUES %s
""", rec_rows)
print(f"     {len(rec_rows)} recommendations inserted")

# ── 10. Forecasts (30-day horizon) ────────────────────────────────────────────
print("  → Generating cost forecasts...")
forecast_rows = []

# Get recent daily totals per provider to base forecasts on
cur.execute("""
    SELECT provider, account_id, SUM(total_cost)/COUNT(DISTINCT summary_date) as daily_avg
    FROM daily_cost_summary
    WHERE summary_date >= CURRENT_DATE - 30
    GROUP BY provider, account_id
""")
provider_avgs = {(r["provider"], r["account_id"]): float(r["daily_avg"]) for r in cur.fetchall()}

for (provider, account_id), daily_avg in provider_avgs.items():
    for day_ahead in range(1, 31):
        fdate = today + timedelta(days=day_ahead)
        dow = fdate.weekday()
        weekend_factor = 0.72 if dow >= 5 else 1.0
        trend_factor   = 1 + (day_ahead / 90) * 0.12
        forecast       = daily_avg * weekend_factor * trend_factor
        uncertainty    = 0.08 + (day_ahead / 30) * 0.12  # wider bounds further out

        forecast_rows.append((
            str(uuid.uuid4()),
            provider, account_id, None,
            fdate,
            round(forecast, 2),
            round(forecast * (1 - uncertainty), 2),
            round(forecast * (1 + uncertainty), 2),
            "prophet_simple",
        ))

execute_values(cur, """
    INSERT INTO cost_forecasts (id, provider, account_id, service_name, forecast_date, forecasted_cost, lower_bound, upper_bound, model_type)
    VALUES %s
    ON CONFLICT (provider, account_id, service_name, forecast_date) DO NOTHING
""", forecast_rows)
print(f"     {len(forecast_rows)} forecast records inserted")

conn.commit()
cur.close()
conn.close()

print("\n✅ Seed complete.")
print("   Billing records:     ", len(billing_rows))
print("   Resources:           ", len(resource_rows))
print("   Pod attributions:    ", len(pod_rows))
print("   Egress flows:        ", len(egress_rows))
print("   Anomalies:           ", len(anomaly_rows))
print("   Recommendations:     ", len(rec_rows))
print("   Forecasts:           ", len(forecast_rows))
