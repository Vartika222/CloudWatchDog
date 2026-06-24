#!/usr/bin/env python3
"""
Attributor Service
------------------
Runs continuously. Every ATTRIBUTION_INTERVAL seconds:
  1. Pulls pod resource usage (from Pixie if enabled, else demo metrics)
  2. Joins with billing data to compute pod-level cost attribution
  3. Detects egress anomalies (cross-AZ traffic patterns)
  4. Runs Prophet-based cost forecasting
  5. Detects memory leaks via statistical trend analysis
  6. Writes results to PostgreSQL
"""

import os, time, logging, json, uuid, math, random
from datetime import datetime, timedelta, date
from decimal import Decimal
import psycopg2
from psycopg2.extras import execute_values, RealDictCursor

logging.basicConfig(level=logging.INFO, format="%(asctime)s [attributor] %(message)s")
log = logging.getLogger(__name__)

DB_DSN       = os.getenv("DB_DSN",       "postgresql://oe_user:oe_pass@localhost:5432/observability")
VM_URL       = os.getenv("VM_URL",        "http://localhost:8428")
PIXIE_ADDR   = os.getenv("PIXIE_ADDR",    "")
PIXIE_ENABLED = os.getenv("PIXIE_ENABLED", "false").lower() == "true"
DEMO_MODE    = os.getenv("DEMO_MODE",     "true").lower() == "true"
INTERVAL     = int(os.getenv("ATTRIBUTION_INTERVAL", "120").replace("s",""))

# Egress cost rates (USD per GB)
EGRESS_RATES = {
    "inter_az":  0.01,
    "internet":  0.09,
    "intra_az":  0.00,
}

# ── Database ──────────────────────────────────────────────────────────────────
def connect_db(retries=15):
    for i in range(retries):
        try:
            conn = psycopg2.connect(DB_DSN)
            log.info("Database connected.")
            return conn
        except Exception as e:
            log.warning(f"DB connect attempt {i+1}/{retries}: {e}")
            time.sleep(2)
    raise RuntimeError("Could not connect to database")

# ── Pixie integration stub ────────────────────────────────────────────────────
class PixieClient:
    """
    Real implementation: use px.Client from pixie-api-python.
    pip install pixie-api
    
    Usage:
        import px
        client = px.Client(server_url=PIXIE_ADDR, use_encryption=True)
        script = client.prepare_script(pxl_script)
        for row in script.run_sync():
            ...
    
    PxL scripts to use:
        px/pod_lifetime_resource_usage  → CPU/memory per pod
        px/net_flow_graph               → egress flows with pod identity
        px/service_edge_stats           → inter-service latency + bytes
    """
    def get_pod_resource_usage(self, cluster_name, window_minutes=60):
        raise NotImplementedError("Set PIXIE_ENABLED=true and configure PIXIE_ADDR")

    def get_network_flows(self, cluster_name, window_minutes=60):
        raise NotImplementedError("Set PIXIE_ENABLED=true and configure PIXIE_ADDR")


# ── Demo metrics generator ────────────────────────────────────────────────────
def generate_demo_pod_metrics(conn):
    """Generate realistic pod resource deltas for the current hour."""
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute("SELECT id, cluster_name FROM k8s_clusters")
        clusters = cur.fetchall()

    namespaces = [
        ("checkout",       0.18),
        ("payments",       0.22),
        ("catalog",        0.12),
        ("recommendations",0.15),
        ("user-service",   0.10),
        ("analytics",      0.11),
        ("monitoring",     0.06),
    ]

    pod_templates = {
        "checkout":       ["checkout-api-{r:02x}", "checkout-worker-{r:02x}"],
        "payments":       ["payments-api-{r:02x}", "payments-processor-{r:02x}"],
        "catalog":        ["catalog-api-{r:02x}", "catalog-search-{r:02x}"],
        "recommendations":["rec-model-{r:02x}", "rec-api-{r:02x}"],
        "user-service":   ["user-api-{r:02x}", "user-auth-{r:02x}"],
        "analytics":      ["analytics-ingest-{r:02x}", "analytics-query-{r:02x}"],
        "monitoring":     ["prom-server-{r:02x}", "alertmanager-{r:02x}"],
    }

    metrics = []
    for cluster in clusters:
        cluster_hourly = (8400 if "eks" in cluster["cluster_name"] else 7200) / (24 * 30)
        for ns, share in namespaces:
            ns_hourly = cluster_hourly * share
            for tmpl in pod_templates.get(ns, [f"{ns}-app-{{r:02x}}"]):
                for replica in range(1, 3):
                    pod_name = tmpl.format(r=replica)
                    pod_cost = ns_hourly / (len(pod_templates.get(ns, ["x"])) * 2)

                    cpu_req  = random.uniform(0.1, 1.0)
                    cpu_used = cpu_req * random.uniform(0.15, 0.75)
                    mem_req  = int(random.uniform(128, 1024) * 1024 * 1024)
                    mem_used = int(mem_req * random.uniform(0.2, 0.8))

                    # Memory leak simulation: recommendations namespace trends up
                    hour_of_day = datetime.now().hour
                    if ns == "recommendations":
                        leak_factor = 1 + (hour_of_day / 24) * 0.3
                        mem_used = int(mem_used * leak_factor)

                    inter_az = int(random.uniform(0, 100) * 1024 * 1024)
                    if ns == "payments":
                        inter_az = int(random.uniform(100, 800) * 1024 * 1024)

                    inet_bytes = int(random.uniform(1, 30) * 1024 * 1024)

                    metrics.append({
                        "cluster_id":          cluster["id"],
                        "namespace":           ns,
                        "pod_name":            pod_name,
                        "service_name":        ns,
                        "owner_kind":          "Deployment",
                        "owner_name":          f"{ns}-deployment",
                        "node_name":           f"node-{random.randint(1,10):02d}",
                        "availability_zone":   random.choice(["a","b","c"]),
                        "cpu_request_cores":   round(cpu_req, 6),
                        "cpu_limit_cores":     round(cpu_req * 1.5, 6),
                        "cpu_used_cores":      round(cpu_used, 6),
                        "memory_request_bytes":mem_req,
                        "memory_limit_bytes":  int(mem_req * 1.5),
                        "memory_used_bytes":   mem_used,
                        "bytes_tx_intra_az":   int(random.uniform(10, 200) * 1024 * 1024),
                        "bytes_tx_inter_az":   inter_az,
                        "bytes_tx_internet":   inet_bytes,
                        "bytes_rx_total":      int(random.uniform(50, 500) * 1024 * 1024),
                        "compute_cost":        round(pod_cost * 0.6, 8),
                        "memory_cost":         round(pod_cost * 0.3, 8),
                        "egress_cost_inter_az":round(inter_az   * EGRESS_RATES["inter_az"] / (1024**3), 8),
                        "egress_cost_internet":round(inet_bytes * EGRESS_RATES["internet"] / (1024**3), 8),
                        "labels":              json.dumps({"team": "platform", "app": ns}),
                    })
    return metrics


# ── Attribution writer ────────────────────────────────────────────────────────
def write_pod_attributions(conn, metrics):
    if not metrics:
        return

    now = datetime.utcnow()
    window_start = now.replace(minute=0, second=0, microsecond=0)
    window_end   = window_start + timedelta(hours=1)

    rows = []
    for m in metrics:
        total = m["compute_cost"] + m["memory_cost"] + m["egress_cost_inter_az"] + m["egress_cost_internet"]
        rows.append((
            str(uuid.uuid4()),
            m["cluster_id"], m["namespace"], m["pod_name"], m["service_name"],
            m["owner_kind"], m["owner_name"], m["node_name"],
            m.get("availability_zone"),
            window_start, window_end,
            m["cpu_request_cores"], m["cpu_limit_cores"], m["cpu_used_cores"],
            m["memory_request_bytes"], m["memory_limit_bytes"], m["memory_used_bytes"],
            m["bytes_tx_intra_az"], m["bytes_tx_inter_az"],
            m["bytes_tx_internet"], m["bytes_rx_total"],
            m["compute_cost"], m["memory_cost"],
            m["egress_cost_inter_az"], m["egress_cost_internet"],
            round(total, 8),
            "usage_ratio" if PIXIE_ENABLED else "demo_simulated",
            m["labels"],
        ))

    with conn.cursor() as cur:
        execute_values(cur, """
            INSERT INTO pod_cost_attribution (
                id, cluster_id, namespace, pod_name, service_name,
                owner_kind, owner_name, node_name, availability_zone,
                window_start, window_end,
                cpu_request_cores, cpu_limit_cores, cpu_used_cores,
                memory_request_bytes, memory_limit_bytes, memory_used_bytes,
                bytes_tx_intra_az, bytes_tx_inter_az, bytes_tx_internet, bytes_rx_total,
                compute_cost, memory_cost, egress_cost_inter_az, egress_cost_internet,
                total_cost, attribution_method, labels
            ) VALUES %s
        """, rows)
    conn.commit()
    log.info(f"Wrote {len(rows)} pod attribution records")


# ── Namespace rollup ──────────────────────────────────────────────────────────
def update_namespace_daily_cost(conn):
    with conn.cursor() as cur:
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
            WHERE DATE(window_start) = CURRENT_DATE
            GROUP BY DATE(window_start), cluster_id, namespace
            ON CONFLICT (summary_date, cluster_id, namespace)
            DO UPDATE SET
                compute_cost = EXCLUDED.compute_cost,
                memory_cost  = EXCLUDED.memory_cost,
                egress_cost  = EXCLUDED.egress_cost,
                total_cost   = EXCLUDED.total_cost,
                pod_count    = EXCLUDED.pod_count
        """)
    conn.commit()


# ── Memory leak detector ──────────────────────────────────────────────────────
def detect_memory_leaks(conn):
    """
    Statistical trend detection: if a pod's memory usage has been increasing
    monotonically for 6+ consecutive hours, flag it as a potential leak.
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute("""
            WITH hourly_mem AS (
                SELECT
                    cluster_id, namespace, pod_name,
                    DATE_TRUNC('hour', window_start) AS hour,
                    AVG(memory_used_bytes) AS avg_mem,
                    AVG(memory_request_bytes) AS avg_req
                FROM pod_cost_attribution
                WHERE window_start >= NOW() - INTERVAL '12 hours'
                GROUP BY cluster_id, namespace, pod_name, DATE_TRUNC('hour', window_start)
            ),
            trends AS (
                SELECT
                    cluster_id, namespace, pod_name,
                    COUNT(*) AS hours,
                    CORR(EXTRACT(EPOCH FROM hour), avg_mem) AS mem_trend_corr,
                    MAX(avg_mem) AS peak_mem,
                    MIN(avg_req) AS mem_request,
                    MAX(avg_mem) - MIN(avg_mem) AS mem_growth_bytes
                FROM hourly_mem
                GROUP BY cluster_id, namespace, pod_name
                HAVING COUNT(*) >= 6
            )
            SELECT * FROM trends
            WHERE mem_trend_corr > 0.85
              AND mem_growth_bytes > 50 * 1024 * 1024  -- >50MB growth
              AND peak_mem > mem_request * 0.7          -- approaching limits
        """)
        leaks = cur.fetchall()

    for leak in leaks:
        growth_mb = leak["mem_growth_bytes"] / (1024 * 1024)
        usage_pct = (leak["peak_mem"] / leak["mem_request"]) * 100 if leak["mem_request"] > 0 else 0

        with conn.cursor() as cur:
            cur.execute("""
                INSERT INTO anomalies (
                    anomaly_type, severity, namespace, pod_name,
                    detected_at, metric_name, metric_value, baseline_value,
                    deviation_pct, zscore, description, impact_usd, status
                )
                SELECT 'memory_leak', %s, %s, %s,
                    NOW(), 'memory_used_bytes', %s, %s,
                    %s, %s,
                    %s, %s, 'open'
                WHERE NOT EXISTS (
                    SELECT 1 FROM anomalies
                    WHERE namespace=%s AND pod_name=%s
                      AND anomaly_type='memory_leak'
                      AND status='open'
                      AND detected_at > NOW() - INTERVAL '6 hours'
                )
            """,
            (
                "high" if usage_pct > 85 else "medium",
                leak["namespace"], leak["pod_name"],
                leak["peak_mem"], leak["mem_request"],
                round(((leak["peak_mem"] - leak["mem_request"]) / leak["mem_request"]) * 100, 2) if leak["mem_request"] > 0 else 0,
                round(leak["mem_trend_corr"] * 4, 2),
                f"Memory leak detected in {leak['pod_name']} ({leak['namespace']}): "
                f"+{growth_mb:.1f}MB over {leak['hours']} hours. "
                f"Correlation: {leak['mem_trend_corr']:.3f}. "
                f"Currently at {usage_pct:.1f}% of memory request. "
                f"Risk of OOMKill within estimated 2-4 hours if trend continues.",
                round(growth_mb * 0.0001, 4),  # rough cost impact
                leak["namespace"], leak["pod_name"],
            ))
        conn.commit()
        log.info(f"Memory leak detected: {leak['namespace']}/{leak['pod_name']} (+{growth_mb:.1f}MB)")


# ── Egress anomaly detection ──────────────────────────────────────────────────
def detect_egress_anomalies(conn):
    """Flag namespaces where inter-AZ traffic is disproportionately high."""
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute("""
            SELECT
                namespace,
                SUM(bytes_tx_inter_az) AS inter_az_bytes,
                SUM(bytes_tx_intra_az) AS intra_az_bytes,
                SUM(bytes_tx_internet) AS internet_bytes,
                SUM(egress_cost_inter_az + egress_cost_internet) AS total_egress_cost
            FROM pod_cost_attribution
            WHERE window_start >= NOW() - INTERVAL '24 hours'
            GROUP BY namespace
            HAVING SUM(bytes_tx_inter_az) > 0
        """)
        ns_egress = cur.fetchall()

    for row in ns_egress:
        total_bytes = (row["inter_az_bytes"] or 0) + (row["intra_az_bytes"] or 0)
        if total_bytes == 0:
            continue
        inter_az_pct = (row["inter_az_bytes"] / total_bytes) * 100

        # Flag if >40% of traffic is crossing AZs unnecessarily
        if inter_az_pct > 40:
            monthly_egress_est = float(row["total_egress_cost"] or 0) * 30
            with conn.cursor() as cur2:
                cur2.execute("""
                    INSERT INTO anomalies (
                        anomaly_type, severity, namespace,
                        detected_at, metric_name, metric_value, baseline_value,
                        deviation_pct, description, impact_usd, status
                    )
                    SELECT 'egress_spike', %s, %s,
                        NOW(), 'inter_az_traffic_pct', %s, 15.0,
                        %s, %s, %s, 'open'
                    WHERE NOT EXISTS (
                        SELECT 1 FROM anomalies
                        WHERE namespace=%s AND anomaly_type='egress_spike'
                          AND status='open' AND detected_at > NOW() - INTERVAL '12 hours'
                    )
                """, (
                    "high" if inter_az_pct > 60 else "medium",
                    row["namespace"],
                    round(inter_az_pct, 2),
                    round(inter_az_pct - 15, 2),
                    f"{row['namespace']} namespace: {inter_az_pct:.1f}% of traffic is crossing availability zones "
                    f"(baseline: ~15%). This is generating unnecessary egress costs. "
                    f"Recommendation: Enable Topology Aware Routing to keep traffic within the same AZ. "
                    f"Estimated monthly saving: ${monthly_egress_est:.2f}",
                    round(monthly_egress_est, 2),
                    row["namespace"],
                ))
            conn.commit()


# ── Simple Prophet-style forecast (no dependency required) ────────────────────
def update_forecasts(conn):
    """
    Linear trend + weekend seasonality forecast.
    In production: replace with from prophet import Prophet.
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:
        cur.execute("""
            SELECT provider, account_id,
                   AVG(total_cost) as daily_avg,
                   REGR_SLOPE(total_cost, EXTRACT(EPOCH FROM summary_date::timestamp)) as trend_slope
            FROM daily_cost_summary
            WHERE summary_date >= CURRENT_DATE - 30
            GROUP BY provider, account_id
        """)
        baselines = cur.fetchall()

    today = date.today()
    forecast_rows = []

    for b in baselines:
        daily_avg = float(b["daily_avg"] or 0)
        slope = float(b["trend_slope"] or 0)

        for day_ahead in range(1, 31):
            fdate = today + timedelta(days=day_ahead)
            dow = fdate.weekday()
            weekend_factor = 0.72 if dow >= 5 else 1.0
            trend_adjustment = slope * day_ahead * 86400  # slope per second * seconds
            forecast = (daily_avg + trend_adjustment) * weekend_factor

            # Confidence interval widens with horizon
            uncertainty = 0.06 + (day_ahead / 30) * 0.14

            forecast_rows.append((
                str(uuid.uuid4()),
                b["provider"], b["account_id"], None,
                fdate,
                round(max(forecast, 0), 2),
                round(max(forecast * (1 - uncertainty), 0), 2),
                round(forecast * (1 + uncertainty), 2),
                "linear_trend_seasonal",
            ))

    if forecast_rows:
        with conn.cursor() as cur:
            execute_values(cur, """
                INSERT INTO cost_forecasts
                    (id, provider, account_id, service_name, forecast_date, forecasted_cost, lower_bound, upper_bound, model_type)
                VALUES %s
                ON CONFLICT (provider, account_id, service_name, forecast_date)
                DO UPDATE SET
                    forecasted_cost = EXCLUDED.forecasted_cost,
                    lower_bound = EXCLUDED.lower_bound,
                    upper_bound = EXCLUDED.upper_bound,
                    model_type = EXCLUDED.model_type,
                    generated_at = NOW()
            """, forecast_rows)
        conn.commit()
        log.info(f"Updated {len(forecast_rows)} forecast records")


# ── GAP 4: Tag Inference Engine ──────────────────────────────────────────────
class TagInferenceEngine:
    """
    Infers ownership tags from non-tag signals when explicit tags are missing.
    
    This solves the 'tag hygiene bootstrapping problem': most orgs at $50K-$1M/month
    have 30-60% of resources untagged. Without ownership tags, cost attribution is
    impossible. This engine infers tags from 5 signal types and presents them
    as pending suggestions that an owner can accept/reject.
    
    Signal priority (highest confidence first):
      1. deploy_history  — CloudTrail/Activity Log: who created/last-modified it
      2. iam_principal   — which role/user owns it based on last action
      3. name_pattern    — regex on resource name (fast, lower confidence)
      4. network_topology — co-located resources in same VPC → same team
      5. cost_cluster    — correlated spend patterns → probably same team
    """
    
    # Built-in name pattern rules (seed these into tag_inference_rules table)
    BUILTIN_PATTERNS = [
        # Environment detection
        ("name_pattern", r"(?i)[-_\./]prod[-_\./]|[-_]production|[-_]prd[-_]",  "env", "production",  0.92),
        ("name_pattern", r"(?i)[-_\./]staging|[-_]stage[-_]|[-_]stg[-_]",        "env", "staging",     0.90),
        ("name_pattern", r"(?i)[-_\./]dev[-_\./]|[-_]development|[-_]sandbox",   "env", "development", 0.88),
        ("name_pattern", r"(?i)[-_\./]test[-_\./]|[-_]qa[-_]|[-_]uat[-_]",       "env", "testing",     0.85),
        # Team detection from resource naming conventions
        ("name_pattern", r"(?i)checkout|cart|basket",     "team", "checkout",  0.80),
        ("name_pattern", r"(?i)payment|billing|invoice",  "team", "payments",  0.82),
        ("name_pattern", r"(?i)ml-|model|recommend|nlp",  "team", "ml",        0.78),
        ("name_pattern", r"(?i)data-|analytics|warehouse", "team", "data",     0.78),
        ("name_pattern", r"(?i)infra|platform|sre|ops",   "team", "platform",  0.75),
        ("name_pattern", r"(?i)auth|identity|sso|login",  "team", "security",  0.80),
        # Cost center
        ("name_pattern", r"(?i)shared|common|core",       "cost-center", "shared",      0.70),
        ("name_pattern", r"(?i)frontend|ui|web",          "cost-center", "product",     0.72),
    ]
    
    def __init__(self, conn):
        self.conn = conn
        self._seed_builtin_rules()
    
    def _seed_builtin_rules(self):
        """Insert built-in rules if not already present."""
        with self.conn.cursor() as cur:
            for signal_type, pattern, tag_key, tag_value, confidence in self.BUILTIN_PATTERNS:
                cur.execute("""
                    INSERT INTO tag_inference_rules
                        (signal_type, signal_pattern, tag_key, tag_value, confidence)
                    SELECT %s,%s,%s,%s,%s
                    WHERE NOT EXISTS (
                        SELECT 1 FROM tag_inference_rules
                        WHERE signal_type=%s AND signal_pattern=%s AND tag_key=%s
                    )
                """, (signal_type, pattern, tag_key, tag_value, confidence,
                      signal_type, pattern, tag_key))
        self.conn.commit()
    
    def run(self):
        """Run inference on all untagged/undertagged resources."""
        import re
        
        # Load active rules
        with self.conn.cursor(cursor_factory=RealDictCursor) as cur:
            cur.execute("SELECT * FROM tag_inference_rules WHERE is_active=TRUE")
            rules = cur.fetchall()
            
            # Get resources missing key tags
            cur.execute("""
                SELECT resource_id, provider, resource_name, resource_type,
                       COALESCE(tags, '{}') as tags,
                       COALESCE(configuration, '{}') as configuration
                FROM resources
                WHERE tags->>'team' IS NULL OR tags->>'env' IS NULL
                LIMIT 500
            """)
            untagged = cur.fetchall()
        
        inferences = []
        for resource in untagged:
            name = resource["resource_name"] or ""
            existing_tags = resource["tags"] or {}
            
            for rule in rules:
                # Skip if tag already exists with a real value
                if rule["tag_key"] in existing_tags and existing_tags[rule["tag_key"]]:
                    continue
                
                matched = False
                signal_detail = None
                
                if rule["signal_type"] == "name_pattern":
                    if re.search(rule["signal_pattern"], name):
                        matched = True
                        signal_detail = f"Name '{name}' matches pattern '{rule['signal_pattern']}'"
                
                if matched:
                    inferences.append((
                        str(uuid.uuid4()),
                        '00000000-0000-0000-0000-000000000001',
                        resource["resource_id"],
                        resource["provider"],
                        rule["tag_key"],
                        rule["tag_value"],
                        float(rule["confidence"]),
                        rule["signal_type"],
                        signal_detail,
                    ))
        
        if inferences:
            with self.conn.cursor() as cur:
                execute_values(cur, """
                    INSERT INTO inferred_tags
                        (id, tenant_id, resource_id, provider, tag_key, tag_value,
                         confidence, signal_type, signal_detail)
                    VALUES %s
                    ON CONFLICT (resource_id, provider, tag_key) DO UPDATE
                        SET confidence=EXCLUDED.confidence,
                            signal_detail=EXCLUDED.signal_detail
                """, inferences)
            self.conn.commit()
            log.info(f"Tag inference: {len(inferences)} inferences across {len(untagged)} resources")
        
        # Report coverage
        with self.conn.cursor(cursor_factory=RealDictCursor) as cur:
            cur.execute("SELECT * FROM v_tag_coverage")
            coverage = cur.fetchall()
        for row in coverage:
            log.info(f"Tag coverage [{row['provider']}]: {row['coverage_pct']}% "
                    f"({row['untagged_resources']} untagged, ${row['untagged_cost_30d']:.0f}/mo unattributed)")


# ── GAP 2: Commitment Portfolio Optimizer ─────────────────────────────────────
class CommitmentPortfolioOptimizer:
    """
    Treats RI/Savings Plan coverage as a PORTFOLIO OPTIMIZATION problem,
    not a simple rule engine.
    
    Key insight from ProsperOps: the right commitment strategy is a continuous
    rebalancing problem. You don't just buy RIs once — you maintain a portfolio
    where the risk/reward tradeoff shifts as workloads evolve.
    
    AWS Savings Plans — three types with different flexibility:
      - Compute SP:      Most flexible. Covers EC2, Lambda, Fargate across any family/region.
      - EC2 Instance SP: Less flexible. Fixed family+region, but deeper discount.
      - SageMaker SP:    Only for SageMaker. Don't buy unless you're ML-heavy.
    
    Decision framework:
      - Workload uptime >95% for 90d + no arch change planned → eligible for commitment
      - Baseline stable spend → EC2 Instance SP (deeper discount, less flexibility)
      - Dynamic/variable compute → Compute SP (flexibility premium worth it)
      - 3yr only for: zero-churn legacy, on-prem migrated workloads
    """
    
    # Discount rates vs on-demand (approximate, varies by region/family)
    SAVINGS_RATES = {
        "ec2_reserved_1yr":      0.38,  # 38% discount
        "ec2_reserved_3yr":      0.57,  # 57% discount
        "compute_savings_plan":  0.34,  # 34% vs on-demand
        "ec2_savings_plan":      0.42,  # 42% — less flexible than Compute SP
        "azure_reserved_1yr":    0.36,
        "azure_reserved_3yr":    0.54,
        "azure_savings_plan":    0.32,
    }
    
    def __init__(self, conn):
        self.conn = conn
    
    def run(self):
        """Analyze on-demand spend and generate commitment recommendations."""
        
        # Find stable on-demand workloads (resources running continuously)
        with self.conn.cursor(cursor_factory=RealDictCursor) as cur:
            cur.execute("""
                SELECT
                    r.provider, r.account_id, r.resource_type,
                    r.resource_name, r.resource_id,
                    r.cost_30d,
                    r.commitment_type,
                    EXTRACT(EPOCH FROM (NOW() - r.discovered_at)) / 86400 AS age_days,
                    -- Uptime proxy: if cost is consistent, instance is running
                    COALESCE(
                        (SELECT STDDEV(total_cost) / NULLIF(AVG(total_cost), 0)
                         FROM daily_cost_summary dcs
                         WHERE dcs.provider = r.provider
                           AND dcs.summary_date >= CURRENT_DATE - 90),
                        1.0
                    ) AS cost_cv  -- coefficient of variation (lower = more stable)
                FROM resources r
                WHERE r.commitment_type = 'on_demand'
                  AND r.cost_30d > 50  -- minimum $50/mo to be worth committing
                  AND r.is_orphan = FALSE
                  AND r.resource_type IN (
                      'ec2_instance', 'rds_instance',
                      'virtual_machine', 'sql_database'
                  )
            """)
            candidates = cur.fetchall()
        
        recs = []
        for c in candidates:
            age    = float(c["age_days"] or 0)
            cv     = float(c["cost_cv"] or 1.0)
            cost30 = float(c["cost_30d"] or 0)
            
            # Stability score: high age + low coefficient of variation = stable
            stability = min(age / 90, 1.0) * (1 - min(cv, 1.0))
            
            if stability < 0.3:
                continue  # Too volatile, don't recommend commitment
            
            # Choose commitment type based on stability + provider
            if c["provider"] == "aws":
                if stability > 0.75 and age > 180:
                    # Very stable, long-running → EC2 SP (better discount)
                    rec_type = "ec2_savings_plan"
                    term = "1yr" if age < 365 else "can consider 3yr"
                else:
                    # Moderately stable → Compute SP (more flexibility)
                    rec_type = "compute_savings_plan"
                    term = "1yr"
            else:  # azure
                rec_type = "azure_reserved_1yr" if stability > 0.6 else "azure_savings_plan"
                term = "1yr"
            
            savings_rate = self.SAVINGS_RATES.get(rec_type, 0.35)
            monthly_savings = cost30 * savings_rate
            break_even = 1.0  # savings plans are immediate
            
            # Risk score: lower stability = higher risk
            risk = round(1.0 - stability, 2)
            
            reasoning = (
                f"Resource has been running for {age:.0f} days with a cost "
                f"coefficient of variation of {cv:.2f} (lower is more stable). "
                f"Stability score: {stability:.2f}/1.0. "
                f"Recommended: {rec_type} ({term}). "
                f"Risk score: {risk:.2f}/1.0. "
                f"{'High confidence — workload is very stable.' if stability > 0.7 else 'Moderate confidence — monitor for 30 more days before purchasing.'}"
            )
            
            recs.append((
                str(uuid.uuid4()),
                '00000000-0000-0000-0000-000000000001',
                c["provider"], c["account_id"],
                rec_type,
                c["resource_type"], None, None,
                1,
                cost30,
                None,  # current coverage pct (needs commitment inventory)
                70.0,  # target coverage
                round(monthly_savings, 2),
                round(break_even, 1),
                risk,
                int(age),
                round((1 - cv) * 100, 2),
                round(stability * 100, 2),
                reasoning,
                "open",
            ))
        
        if recs:
            with self.conn.cursor() as cur:
                execute_values(cur, """
                    INSERT INTO commitment_recommendations (
                        id, tenant_id, provider, account_id, rec_action,
                        resource_type, instance_family, region, quantity,
                        current_od_spend, commitment_coverage_pct, target_coverage_pct,
                        monthly_savings, break_even_months, risk_score,
                        stability_days, uptime_pct, confidence_pct, reasoning, status
                    ) VALUES %s
                    ON CONFLICT DO NOTHING
                """, recs)
            self.conn.commit()
            log.info(f"Commitment optimizer: {len(recs)} recommendations generated")


# ── GAP 3: VPA Comparison Engine ──────────────────────────────────────────────
class VPAComparisonEngine:
    """
    Our rightsizing vs Kubernetes VPA (Vertical Pod Autoscaler).
    
    VPA's limitations (our differentiation):
      1. VPA has NO cost awareness — it sees CPU/memory, not dollars
      2. VPA ignores NIC limits — downsize an m5.4xlarge to m5.large and lose NICs
      3. VPA ignores IOPS — resize a DB pod and hit storage performance wall
      4. VPA + HPA conflict — running both can cause pod thrashing
      5. VPA ignores DR constraints — might resize a standby instance to nothing
      6. VPA doesn't generate IaC — no Terraform patch, no PR
    
    This engine simulates what VPA would recommend, then shows where our
    recommendations differ and WHY — this is the demo that wins customers.
    """
    
    def __init__(self, conn):
        self.conn = conn
    
    def run(self):
        """Compare our rightsizing recs against simulated VPA recommendations."""
        
        with self.conn.cursor(cursor_factory=RealDictCursor) as cur:
            # Get pods where we have 14+ days of utilization data
            cur.execute("""
                SELECT
                    pca.cluster_id,
                    pca.namespace,
                    pca.owner_name AS target_name,
                    pca.owner_kind AS target_kind,
                    pca.pod_name,
                    AVG(pca.cpu_request_cores)     AS avg_cpu_req,
                    AVG(pca.cpu_used_cores)        AS avg_cpu_used,
                    PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY pca.cpu_used_cores) AS p95_cpu,
                    AVG(pca.memory_request_bytes)  AS avg_mem_req,
                    AVG(pca.memory_used_bytes)     AS avg_mem_used,
                    PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY pca.memory_used_bytes) AS p95_mem,
                    SUM(pca.total_cost)            AS cost_14d,
                    COUNT(DISTINCT DATE(pca.window_start)) AS days_observed
                FROM pod_cost_attribution pca
                WHERE pca.window_start >= NOW() - INTERVAL '14 days'
                GROUP BY pca.cluster_id, pca.namespace, pca.owner_name,
                         pca.owner_kind, pca.pod_name
                HAVING COUNT(DISTINCT DATE(pca.window_start)) >= 7
            """)
            pods = cur.fetchall()
        
        vpa_rows = []
        for pod in pods:
            avg_cpu_req  = float(pod["avg_cpu_req"]  or 0)
            p95_cpu      = float(pod["p95_cpu"]      or 0)
            avg_mem_req  = float(pod["avg_mem_req"]  or 0)
            p95_mem      = float(pod["p95_mem"]      or 0)
            cost_14d     = float(pod["cost_14d"]     or 0)
            
            if avg_cpu_req == 0 or avg_mem_req == 0:
                continue
            
            # VPA recommendation: target p95 usage + 20% headroom
            vpa_cpu = p95_cpu * 1.20
            vpa_mem = p95_mem * 1.20
            
            # Our recommendation: same utilization signal BUT also check:
            # - Does this pod have an HPA? (if so, VPA conflict warning)
            # - Is CPU under 20%? Only recommend if consistently low, not just avg
            our_cpu = vpa_cpu  # start same as VPA
            our_mem = vpa_mem
            
            has_hpa = pod["namespace"] in ("checkout", "payments", "catalog")  # demo heuristic
            
            # HPA conflict: if HPA manages replicas, VPA shouldn't change CPU requests
            # because HPA scaling decisions depend on per-pod CPU
            disagrees = False
            disagreement_reason = None
            
            if has_hpa and avg_cpu_req > 0:
                # We WON'T resize CPU when HPA is active — VPA would, incorrectly
                our_cpu = avg_cpu_req  # keep current
                disagrees = True
                disagreement_reason = (
                    f"HPA detected on {pod['namespace']}/{pod['target_name']}. "
                    f"Resizing CPU requests while HPA is active causes pod thrashing: "
                    f"HPA scales based on CPU%, which changes when requests change. "
                    f"VPA does not check for HPA. We do not resize CPU on HPA-managed deployments."
                )
            
            # Cost impact
            ratio = our_cpu / avg_cpu_req if avg_cpu_req > 0 else 1.0
            our_cost = cost_14d * ratio
            savings = cost_14d - our_cost
            
            vpa_cpu_str = f"{int(vpa_cpu * 1000)}m"
            vpa_mem_str = f"{int(vpa_mem / (1024*1024))}Mi"
            our_cpu_str = f"{int(our_cpu * 1000)}m"
            our_mem_str = f"{int(our_mem / (1024*1024))}Mi"
            
            vpa_rows.append((
                str(uuid.uuid4()),
                pod["cluster_id"],
                pod["namespace"],
                pod["target_name"] or pod["pod_name"],
                pod["target_kind"] or "Deployment",
                None,  # container_name
                vpa_cpu_str, vpa_mem_str,
                our_cpu_str, our_mem_str,
                round(cost_14d / 14 * 30, 4),
                round(our_cost / 14 * 30, 4),
                round(savings / 14 * 30, 4),
                True,   # nic_limit_ok (demo: assume ok)
                True,   # iops_limit_ok
                has_hpa,
                False,  # dr_constraint
                not disagrees,
                disagreement_reason,
            ))
        
        if vpa_rows:
            with self.conn.cursor() as cur:
                execute_values(cur, """
                    INSERT INTO vpa_recommendations (
                        id, cluster_id, namespace, target_name, target_kind, container_name,
                        vpa_cpu_request, vpa_mem_request,
                        our_cpu_request, our_mem_request,
                        current_monthly_cost, our_monthly_cost, monthly_savings,
                        nic_limit_ok, iops_limit_ok, has_hpa, dr_constraint,
                        agrees_with_vpa, disagreement_reason
                    ) VALUES %s
                """, vpa_rows)
            self.conn.commit()
            
            disagreements = sum(1 for r in vpa_rows if not r[17])
            log.info(f"VPA comparison: {len(vpa_rows)} pods analyzed, "
                    f"{disagreements} disagreements with VPA (HPA conflicts etc.)")


# ── NOTE ──────────────────────────────────────────────────────────────────────
# A second, standalone tag-inference implementation (run_tag_inference() with a
# module-level TAG_RULES list) used to live here. It inserted into columns
# (confidence_pct, rule_name) that don't exist on the real inferred_tags table
# and crashed on every call. Removed in favor of TagInferenceEngine above,
# which is schema-correct, multi-tenant aware, and already covers the same
# env/team/cost-center signal categories via tag_inference_rules.
import re


# ── Efficiency Score (Phase 3) ────────────────────────────────────────────────
def compute_efficiency_score(conn):
    """
    Composite score (0-100) across four dimensions:
      - Utilization efficiency: actual CPU/mem used vs requested
      - Allocation coverage: % of spend with team tag
      - Commitment utilization: % of spend on non-on_demand
      - Hygiene: orphan cost %, cross-AZ traffic ratio
    Stored in efficiency_scores table. Surface in weekly Slack digest.
    """
    with conn.cursor(cursor_factory=RealDictCursor) as cur:

        # 1. Utilization efficiency (0-100): avg cpu_used/cpu_request
        cur.execute("""
            SELECT COALESCE(
                AVG(
                    CASE WHEN cpu_request_cores > 0
                         THEN LEAST(cpu_used_cores / cpu_request_cores, 1.0)
                         ELSE NULL END
                ) * 100, 0
            ) AS util_eff
            FROM pod_cost_attribution
            WHERE window_start >= NOW() - INTERVAL '7 days'
        """)
        util_eff = float(cur.fetchone()['util_eff'] or 0)

        # 2. Allocation coverage (0-100): % of cost_30d with team tag
        cur.execute("""
            SELECT
                COALESCE(SUM(cost_30d) FILTER (
                    WHERE tags->>'team' IS NOT NULL
                       OR EXISTS (SELECT 1 FROM inferred_tags it
                                  WHERE it.resource_id = resources.resource_id
                                    AND it.tag_key = 'team'
                                    AND it.accepted = TRUE)
                ), 0) AS attributed_cost,
                COALESCE(SUM(cost_30d), 1) AS total_cost
            FROM resources
        """)
        row = cur.fetchone()
        alloc_cov = float(row['attributed_cost']) / float(row['total_cost']) * 100

        # 3. Commitment utilization (0-100): % of cost_30d not on-demand
        cur.execute("""
            SELECT
                COALESCE(SUM(cost_30d) FILTER (
                    WHERE commitment_type != 'on_demand'
                      AND commitment_type IS NOT NULL
                ), 0) AS committed_cost,
                COALESCE(SUM(cost_30d), 1) AS total_cost
            FROM resources
        """)
        row = cur.fetchone()
        commit_util = float(row['committed_cost']) / float(row['total_cost']) * 100

        # 4. Hygiene score (0-100): penalize orphans and cross-AZ waste
        cur.execute("""
            SELECT
                COALESCE(SUM(cost_30d) FILTER (WHERE is_orphan = TRUE), 0) AS orphan_cost,
                COALESCE(SUM(cost_30d), 1) AS total_cost
            FROM resources
        """)
        row = cur.fetchone()
        orphan_pct = float(row['orphan_cost']) / float(row['total_cost']) * 100

        cur.execute("""
            SELECT
                COALESCE(SUM(bytes_tx_inter_az), 0) AS inter_az,
                COALESCE(SUM(bytes_tx_intra_az + bytes_tx_inter_az), 1) AS total
            FROM pod_cost_attribution
            WHERE window_start >= NOW() - INTERVAL '7 days'
        """)
        row = cur.fetchone()
        cross_az_pct = float(row['inter_az']) / float(row['total']) * 100

        # Hygiene: 100 - orphan penalty - cross-AZ penalty
        hygiene = max(0, 100 - (orphan_pct * 3) - (max(0, cross_az_pct - 15) * 2))

    # Weighted composite: utilization heaviest (it's what you control day-to-day)
    weights = {'util': 0.35, 'alloc': 0.25, 'commit': 0.20, 'hygiene': 0.20}
    composite = (
        weights['util']    * min(util_eff, 100) +
        weights['alloc']   * min(alloc_cov, 100) +
        weights['commit']  * min(commit_util, 100) +
        weights['hygiene'] * min(hygiene, 100)
    )
    composite = round(composite, 2)

    with conn.cursor() as cur:
        cur.execute("""
            INSERT INTO efficiency_scores
                (score_date, utilization_eff, allocation_cov,
                 commitment_util, hygiene_score, composite_score)
            VALUES (CURRENT_DATE, %s, %s, %s, %s, %s)
            ON CONFLICT (score_date) DO UPDATE
                SET utilization_eff  = EXCLUDED.utilization_eff,
                    allocation_cov   = EXCLUDED.allocation_cov,
                    commitment_util  = EXCLUDED.commitment_util,
                    hygiene_score    = EXCLUDED.hygiene_score,
                    composite_score  = EXCLUDED.composite_score,
                    computed_at      = NOW()
        """, (
            round(util_eff, 2), round(alloc_cov, 2),
            round(commit_util, 2), round(hygiene, 2), composite
        ))
    conn.commit()

    tier = 'Elite' if composite >= 90 else 'Good' if composite >= 70 else 'Fair' if composite >= 50 else 'Poor'
    log.info(f"Efficiency score: {composite}/100 ({tier}) — "
            f"util={util_eff:.1f} alloc={alloc_cov:.1f} "
            f"commit={commit_util:.1f} hygiene={hygiene:.1f}")


def main():
    log.info(f"Attributor starting. demo={DEMO_MODE} pixie={PIXIE_ENABLED} interval={INTERVAL}s")

    conn = connect_db()
    pixie = PixieClient() if PIXIE_ENABLED else None

    # Initialize gap-fix engines
    tag_engine        = TagInferenceEngine(conn)
    commitment_engine = CommitmentPortfolioOptimizer(conn)
    vpa_engine        = VPAComparisonEngine(conn)

    cycle = 0
    while True:
        try:
            log.info(f"Running attribution cycle {cycle}...")

            # 1. Get pod metrics
            if DEMO_MODE:
                metrics = generate_demo_pod_metrics(conn)
            elif PIXIE_ENABLED and pixie:
                metrics = []
            else:
                metrics = []

            # 2. Write attributions
            if metrics:
                write_pod_attributions(conn, metrics)
                update_namespace_daily_cost(conn)

            # 3. Anomaly detection
            detect_memory_leaks(conn)
            detect_egress_anomalies(conn)

            # 4. Forecasting
            update_forecasts(conn)

            # 5. GAP 4: Tag inference (every 5 cycles)
            if cycle % 5 == 0:
                tag_engine.run()

            # 6. GAP 2: Commitment portfolio (every 10 cycles ~20min)
            if cycle % 10 == 0:
                commitment_engine.run()

            # 7. GAP 3: VPA comparison (every 5 cycles)
            if cycle % 5 == 0:
                vpa_engine.run()

            # (Removed: a second, duplicate tag-inference call here used to
            # invoke the now-deleted run_tag_inference(), which crashed every
            # 3rd cycle against the real schema. tag_engine.run() above is
            # the single, correct tag-inference path.)

            # 9. Efficiency score (daily — every 720 cycles at 120s interval)
            if cycle % 720 == 0 or cycle == 0:
                compute_efficiency_score(conn)

            log.info(f"Attribution cycle {cycle} complete")
            cycle += 1

        except Exception as e:
            log.error(f"Attribution cycle failed: {e}", exc_info=True)
            try:
                conn.rollback()
            except Exception:
                pass
            try:
                conn = connect_db(retries=3)
            except Exception:
                pass

        time.sleep(INTERVAL)


if __name__ == "__main__":
    main()