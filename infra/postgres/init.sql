-- ============================================================
-- Cloud Observability Engine — Database Schema
-- FOCUS-compatible cost schema + attribution tables
-- ============================================================

CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";

-- ── FOCUS-normalized billing records ────────────────────────────────────────
CREATE TABLE billing_records (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- FOCUS required fields
    billing_account_id    TEXT NOT NULL,
    billing_account_name  TEXT,
    provider              TEXT NOT NULL CHECK (provider IN ('aws', 'azure', 'gcp')),
    service_name          TEXT NOT NULL,
    service_category      TEXT,
    region                TEXT,
    availability_zone     TEXT,
    resource_id           TEXT,
    resource_name         TEXT,
    resource_type         TEXT,
    -- Cost fields (FOCUS spec)
    billed_cost           NUMERIC(18,6) NOT NULL DEFAULT 0,
    effective_cost        NUMERIC(18,6) NOT NULL DEFAULT 0,
    list_cost             NUMERIC(18,6),
    amortized_cost        NUMERIC(18,6),
    currency              CHAR(3) NOT NULL DEFAULT 'USD',
    -- Time
    billing_period_start  TIMESTAMPTZ NOT NULL,
    billing_period_end    TIMESTAMPTZ NOT NULL,
    usage_start           TIMESTAMPTZ NOT NULL,
    usage_end             TIMESTAMPTZ NOT NULL,
    -- Usage
    usage_quantity        NUMERIC(18,6),
    usage_unit            TEXT,
    -- Tags (FOCUS: provider-agnostic)
    tags                  JSONB DEFAULT '{}',
    -- Internal
    raw_record            JSONB,
    ingested_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_billing_provider        ON billing_records (provider);
CREATE INDEX idx_billing_period          ON billing_records (billing_period_start, billing_period_end);
CREATE INDEX idx_billing_resource        ON billing_records (resource_id);
CREATE INDEX idx_billing_service         ON billing_records (service_name);
CREATE INDEX idx_billing_account         ON billing_records (billing_account_id);
CREATE INDEX idx_billing_tags            ON billing_records USING GIN (tags);
CREATE INDEX idx_billing_usage_start     ON billing_records (usage_start);

-- ── Resources (inventory) ────────────────────────────────────────────────────
CREATE TABLE resources (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider         TEXT NOT NULL,
    account_id       TEXT NOT NULL,
    resource_id      TEXT NOT NULL,
    resource_name    TEXT,
    resource_type    TEXT NOT NULL,
    region           TEXT,
    availability_zone TEXT,
    status           TEXT DEFAULT 'active',
    -- Metadata
    configuration    JSONB DEFAULT '{}',
    tags             JSONB DEFAULT '{}',
    -- Cost summary (denormalized for fast queries)
    cost_30d         NUMERIC(18,2) DEFAULT 0,
    cost_7d          NUMERIC(18,2) DEFAULT 0,
    cost_yesterday   NUMERIC(18,2) DEFAULT 0,
    -- Sizing data
    cpu_cores        INT,
    memory_gb        NUMERIC(8,2),
    storage_gb       NUMERIC(10,2),
    nic_count        INT,
    -- RI/commitment coverage
    commitment_type  TEXT,  -- 'on_demand', 'reserved_1yr', 'reserved_3yr', 'spot', 'savings_plan'
    commitment_end   DATE,
    -- Orphan detection
    last_active_at   TIMESTAMPTZ,
    is_orphan        BOOLEAN DEFAULT FALSE,
    orphan_reason    TEXT,
    -- Timestamps
    discovered_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, account_id, resource_id)
);

CREATE INDEX idx_resources_provider   ON resources (provider);
CREATE INDEX idx_resources_type       ON resources (resource_type);
CREATE INDEX idx_resources_orphan     ON resources (is_orphan) WHERE is_orphan = TRUE;
CREATE INDEX idx_resources_cost       ON resources (cost_30d DESC);

-- ── Kubernetes clusters ──────────────────────────────────────────────────────
CREATE TABLE k8s_clusters (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider        TEXT NOT NULL,
    account_id      TEXT NOT NULL,
    cluster_name    TEXT NOT NULL,
    region          TEXT,
    node_count      INT DEFAULT 0,
    total_cost_30d  NUMERIC(18,2) DEFAULT 0,
    pixie_enabled   BOOLEAN DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, account_id, cluster_name)
);

-- ── Pod cost attribution (Phase 2 core) ─────────────────────────────────────
CREATE TABLE pod_cost_attribution (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    cluster_id       UUID REFERENCES k8s_clusters(id),
    namespace        TEXT NOT NULL,
    pod_name         TEXT NOT NULL,
    service_name     TEXT,
    owner_kind       TEXT,   -- Deployment, StatefulSet, DaemonSet, etc.
    owner_name       TEXT,
    node_name        TEXT,
    availability_zone TEXT,
    -- Time window
    window_start     TIMESTAMPTZ NOT NULL,
    window_end       TIMESTAMPTZ NOT NULL,
    -- Resource usage (from Pixie/metrics)
    cpu_request_cores     NUMERIC(10,6) DEFAULT 0,
    cpu_limit_cores       NUMERIC(10,6) DEFAULT 0,
    cpu_used_cores        NUMERIC(10,6) DEFAULT 0,
    memory_request_bytes  BIGINT DEFAULT 0,
    memory_limit_bytes    BIGINT DEFAULT 0,
    memory_used_bytes     BIGINT DEFAULT 0,
    -- Network (from Pixie eBPF)
    bytes_tx_intra_az     BIGINT DEFAULT 0,   -- same AZ
    bytes_tx_inter_az     BIGINT DEFAULT 0,   -- cross-AZ (costs money!)
    bytes_tx_internet     BIGINT DEFAULT 0,   -- internet egress (expensive)
    bytes_rx_total        BIGINT DEFAULT 0,
    -- Cost attribution
    compute_cost          NUMERIC(12,6) DEFAULT 0,
    memory_cost           NUMERIC(12,6) DEFAULT 0,
    egress_cost_inter_az  NUMERIC(12,6) DEFAULT 0,
    egress_cost_internet  NUMERIC(12,6) DEFAULT 0,
    total_cost            NUMERIC(12,6) DEFAULT 0,
    -- Attribution method
    attribution_method    TEXT DEFAULT 'request_ratio',  -- 'request_ratio', 'usage_ratio', 'ebpf'
    -- Labels
    labels               JSONB DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_pod_attribution_cluster    ON pod_cost_attribution (cluster_id);
CREATE INDEX idx_pod_attribution_namespace  ON pod_cost_attribution (namespace);
CREATE INDEX idx_pod_attribution_window     ON pod_cost_attribution (window_start, window_end);
CREATE INDEX idx_pod_attribution_cost       ON pod_cost_attribution (total_cost DESC);
CREATE INDEX idx_pod_attribution_egress     ON pod_cost_attribution (egress_cost_inter_az DESC);

-- ── Egress flows (from Pixie net_flow_graph) ────────────────────────────────
CREATE TABLE egress_flows (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    cluster_id       UUID REFERENCES k8s_clusters(id),
    src_namespace    TEXT,
    src_pod          TEXT,
    src_service      TEXT,
    dst_namespace    TEXT,
    dst_pod          TEXT,
    dst_service      TEXT,
    dst_ip           INET,
    flow_type        TEXT NOT NULL CHECK (flow_type IN ('intra_az', 'inter_az', 'internet', 'intra_pod')),
    bytes_total      BIGINT DEFAULT 0,
    request_count    BIGINT DEFAULT 0,
    egress_cost      NUMERIC(12,6) DEFAULT 0,
    window_start     TIMESTAMPTZ NOT NULL,
    window_end       TIMESTAMPTZ NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_egress_cluster      ON egress_flows (cluster_id);
CREATE INDEX idx_egress_type         ON egress_flows (flow_type);
CREATE INDEX idx_egress_cost         ON egress_flows (egress_cost DESC);
CREATE INDEX idx_egress_window       ON egress_flows (window_start);

-- ── Anomalies ────────────────────────────────────────────────────────────────
CREATE TABLE anomalies (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    anomaly_type     TEXT NOT NULL,  -- 'cost_spike', 'egress_spike', 'cpu_anomaly', 'memory_leak', 'ddos_suspected'
    severity         TEXT NOT NULL CHECK (severity IN ('low', 'medium', 'high', 'critical')),
    provider         TEXT,
    account_id       TEXT,
    resource_id      TEXT,
    resource_name    TEXT,
    namespace        TEXT,
    pod_name         TEXT,
    -- Detection
    detected_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    metric_name      TEXT NOT NULL,
    metric_value     NUMERIC(18,6),
    baseline_value   NUMERIC(18,6),
    deviation_pct    NUMERIC(8,2),
    zscore           NUMERIC(8,4),
    -- Context
    description      TEXT NOT NULL,
    impact_usd       NUMERIC(12,2),
    -- Resolution
    resolved_at      TIMESTAMPTZ,
    resolution_note  TEXT,
    status           TEXT DEFAULT 'open' CHECK (status IN ('open', 'acknowledged', 'resolved', 'false_positive')),
    -- Metadata
    raw_context      JSONB DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_anomalies_type      ON anomalies (anomaly_type);
CREATE INDEX idx_anomalies_severity  ON anomalies (severity);
CREATE INDEX idx_anomalies_status    ON anomalies (status);
CREATE INDEX idx_anomalies_detected  ON anomalies (detected_at DESC);

-- ── Recommendations ──────────────────────────────────────────────────────────
CREATE TABLE recommendations (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    rec_type         TEXT NOT NULL,  -- 'rightsize', 'ri_purchase', 'orphan_delete', 'zone_routing', 'ahb_activate'
    priority         TEXT NOT NULL CHECK (priority IN ('low', 'medium', 'high', 'critical')),
    provider         TEXT NOT NULL,
    account_id       TEXT,
    resource_id      TEXT,
    resource_name    TEXT,
    resource_type    TEXT,
    namespace        TEXT,
    -- The recommendation
    title            TEXT NOT NULL,
    description      TEXT NOT NULL,
    current_config   JSONB DEFAULT '{}',
    recommended_config JSONB DEFAULT '{}',
    -- Financial impact
    monthly_savings  NUMERIC(12,2),
    annual_savings   NUMERIC(12,2),
    confidence_pct   NUMERIC(5,2),
    -- Evidence
    evidence_days    INT DEFAULT 14,
    evidence_data    JSONB DEFAULT '{}',
    -- IaC patch
    iac_type         TEXT,   -- 'terraform', 'helm', 'bicep'
    iac_patch        TEXT,   -- the actual patch content
    pr_url           TEXT,
    pr_status        TEXT,   -- 'pending', 'open', 'merged', 'closed'
    -- Lifecycle
    status           TEXT DEFAULT 'open' CHECK (status IN ('open', 'applied', 'dismissed', 'snoozed')),
    dismissed_reason TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_rec_type       ON recommendations (rec_type);
CREATE INDEX idx_rec_priority   ON recommendations (priority);
CREATE INDEX idx_rec_status     ON recommendations (status);
CREATE INDEX idx_rec_savings    ON recommendations (monthly_savings DESC NULLS LAST);

-- ── Cost forecasts ───────────────────────────────────────────────────────────
CREATE TABLE cost_forecasts (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider         TEXT,       -- NULL = all providers
    account_id       TEXT,
    service_name     TEXT,
    forecast_date    DATE NOT NULL,
    forecasted_cost  NUMERIC(14,2) NOT NULL,
    lower_bound      NUMERIC(14,2),
    upper_bound      NUMERIC(14,2),
    model_type       TEXT DEFAULT 'prophet',
    generated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, account_id, service_name, forecast_date)
);

CREATE INDEX idx_forecast_date     ON cost_forecasts (forecast_date);
CREATE INDEX idx_forecast_provider ON cost_forecasts (provider, account_id);

-- ── Daily cost summary (materialized for fast queries) ───────────────────────
CREATE TABLE daily_cost_summary (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    summary_date     DATE NOT NULL,
    provider         TEXT NOT NULL,
    account_id       TEXT NOT NULL,
    service_name     TEXT NOT NULL,
    region           TEXT,
    total_cost       NUMERIC(14,4) NOT NULL DEFAULT 0,
    resource_count   INT DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (summary_date, provider, account_id, service_name, region)
);

CREATE INDEX idx_daily_cost_date     ON daily_cost_summary (summary_date DESC);
CREATE INDEX idx_daily_cost_provider ON daily_cost_summary (provider, account_id);
CREATE INDEX idx_daily_cost_service  ON daily_cost_summary (service_name);

-- ── Namespace daily cost summary ────────────────────────────────────────────
CREATE TABLE namespace_daily_cost (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    summary_date     DATE NOT NULL,
    cluster_id       UUID REFERENCES k8s_clusters(id),
    namespace        TEXT NOT NULL,
    compute_cost     NUMERIC(12,4) DEFAULT 0,
    memory_cost      NUMERIC(12,4) DEFAULT 0,
    egress_cost      NUMERIC(12,4) DEFAULT 0,
    total_cost       NUMERIC(12,4) DEFAULT 0,
    pod_count        INT DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (summary_date, cluster_id, namespace)
);

CREATE INDEX idx_ns_cost_date      ON namespace_daily_cost (summary_date DESC);
CREATE INDEX idx_ns_cost_namespace ON namespace_daily_cost (namespace);

-- ══════════════════════════════════════════════════════════════════════════════
-- GAP 5: MULTI-TENANCY — tenant isolation from day one
-- ══════════════════════════════════════════════════════════════════════════════

CREATE TABLE tenants (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_slug      TEXT NOT NULL UNIQUE,   -- e.g. "acme-corp", "msp-client-7"
    tenant_name      TEXT NOT NULL,
    plan             TEXT DEFAULT 'standard' CHECK (plan IN ('standard','pro','enterprise','msp')),
    -- MSP parent relationship
    parent_tenant_id UUID REFERENCES tenants(id),
    is_msp           BOOLEAN DEFAULT FALSE,
    -- Cloud account bindings (which accounts belong to this tenant)
    aws_account_ids  TEXT[] DEFAULT '{}',
    azure_sub_ids    TEXT[] DEFAULT '{}',
    -- Settings
    settings         JSONB DEFAULT '{}',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Add tenant_id to all major tables (Row-Level Security ready)
ALTER TABLE billing_records      ADD COLUMN tenant_id UUID REFERENCES tenants(id);
ALTER TABLE resources            ADD COLUMN tenant_id UUID REFERENCES tenants(id);
ALTER TABLE anomalies            ADD COLUMN tenant_id UUID REFERENCES tenants(id);
ALTER TABLE recommendations      ADD COLUMN tenant_id UUID REFERENCES tenants(id);
ALTER TABLE k8s_clusters         ADD COLUMN tenant_id UUID REFERENCES tenants(id);
ALTER TABLE cost_forecasts       ADD COLUMN tenant_id UUID REFERENCES tenants(id);

-- Default tenant for demo/single-org mode
INSERT INTO tenants (id, tenant_slug, tenant_name, plan)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default Organization', 'pro');

CREATE INDEX idx_billing_tenant  ON billing_records (tenant_id);
CREATE INDEX idx_resources_tenant ON resources (tenant_id);
CREATE INDEX idx_anomalies_tenant ON anomalies (tenant_id);
CREATE INDEX idx_recs_tenant      ON recommendations (tenant_id);

-- ══════════════════════════════════════════════════════════════════════════════
-- GAP 4: TAG INFERENCE — infer ownership when tags are missing
-- ══════════════════════════════════════════════════════════════════════════════

CREATE TABLE tag_inference_rules (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id        UUID REFERENCES tenants(id) DEFAULT '00000000-0000-0000-0000-000000000001',
    -- Signal source
    signal_type      TEXT NOT NULL CHECK (signal_type IN (
        'iam_principal',      -- infer from which IAM role/user created/last-modified
        'name_pattern',       -- regex on resource name  e.g. "-prod-" → env=production
        'network_topology',   -- resources in same VPC/subnet → same team
        'deploy_history',     -- CloudTrail/Activity Log: who deployed it
        'cost_cluster'        -- resources with correlated spend patterns → same team
    )),
    signal_pattern   TEXT NOT NULL,        -- regex or exact match
    -- Inferred tag
    tag_key          TEXT NOT NULL,
    tag_value        TEXT NOT NULL,
    confidence       NUMERIC(4,2) DEFAULT 0.80,   -- 0.0–1.0
    is_active        BOOLEAN DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE inferred_tags (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id        UUID REFERENCES tenants(id),
    resource_id      TEXT NOT NULL,
    provider         TEXT NOT NULL,
    tag_key          TEXT NOT NULL,
    tag_value        TEXT NOT NULL,
    confidence       NUMERIC(4,2),
    signal_type      TEXT NOT NULL,
    signal_detail    TEXT,
    -- Is this a conflict with an existing tag?
    conflicts_with   TEXT,
    accepted         BOOLEAN DEFAULT NULL,  -- NULL=pending, TRUE=accepted, FALSE=rejected
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (resource_id, provider, tag_key)
);

CREATE INDEX idx_inferred_tags_resource ON inferred_tags (resource_id, provider);
CREATE INDEX idx_inferred_tags_pending  ON inferred_tags (accepted) WHERE accepted IS NULL;

-- Tag coverage report view
CREATE VIEW v_tag_coverage AS
SELECT
    r.provider,
    COUNT(*) AS total_resources,
    COUNT(CASE WHEN r.tags != '{}' AND r.tags IS NOT NULL THEN 1 END) AS tagged_resources,
    COUNT(CASE WHEN r.tags = '{}' OR r.tags IS NULL THEN 1 END) AS untagged_resources,
    ROUND(100.0 * COUNT(CASE WHEN r.tags != '{}' AND r.tags IS NOT NULL THEN 1 END) / NULLIF(COUNT(*),0), 2) AS coverage_pct,
    SUM(CASE WHEN r.tags = '{}' OR r.tags IS NULL THEN r.cost_30d ELSE 0 END) AS untagged_cost_30d
FROM resources r
GROUP BY r.provider;

-- ══════════════════════════════════════════════════════════════════════════════
-- GAP 2: COMMITMENT PORTFOLIO — RI/Savings Plan optimization
-- ══════════════════════════════════════════════════════════════════════════════

CREATE TABLE commitment_inventory (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id           UUID REFERENCES tenants(id) DEFAULT '00000000-0000-0000-0000-000000000001',
    provider            TEXT NOT NULL,
    account_id          TEXT NOT NULL,
    commitment_id       TEXT NOT NULL,
    commitment_type     TEXT NOT NULL CHECK (commitment_type IN (
        -- AWS
        'ec2_reserved_1yr', 'ec2_reserved_3yr',
        'compute_savings_plan', 'ec2_savings_plan', 'sagemaker_savings_plan',
        'rds_reserved', 'elasticache_reserved',
        -- Azure
        'azure_reserved_1yr', 'azure_reserved_3yr',
        'azure_savings_plan'
    )),
    -- Coverage
    resource_type       TEXT,
    instance_family     TEXT,     -- e.g. "m5", "r5", "Standard_D"
    region              TEXT,
    quantity            INT DEFAULT 1,
    -- Financial
    hourly_rate         NUMERIC(10,6),
    monthly_cost        NUMERIC(12,2),
    -- Dates
    start_date          DATE,
    end_date            DATE,
    -- Utilization (updated by collector)
    utilization_pct     NUMERIC(5,2),   -- how much of the commitment is actually being used
    waste_monthly       NUMERIC(12,2),  -- $ wasted due to under-utilization
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, account_id, commitment_id)
);

CREATE TABLE commitment_recommendations (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tenant_id            UUID REFERENCES tenants(id) DEFAULT '00000000-0000-0000-0000-000000000001',
    provider             TEXT NOT NULL,
    account_id           TEXT NOT NULL,
    rec_action           TEXT NOT NULL CHECK (rec_action IN (
        'purchase_1yr_ri', 'purchase_3yr_ri',
        'purchase_compute_sp', 'purchase_ec2_sp',
        'convert_to_savings_plan', 'sell_ri',
        'modify_ri_scope', 'exchange_ri'
    )),
    resource_type        TEXT,
    instance_family      TEXT,
    region               TEXT,
    quantity             INT DEFAULT 1,
    -- Portfolio context
    current_od_spend     NUMERIC(12,2),   -- current on-demand spend eligible
    commitment_coverage_pct NUMERIC(5,2), -- current % of spend under commitment
    target_coverage_pct  NUMERIC(5,2),   -- recommended target
    -- Financial impact
    monthly_savings      NUMERIC(12,2),
    break_even_months    NUMERIC(5,1),
    risk_score           NUMERIC(4,2),   -- 0=safe, 1=risky (based on workload churn probability)
    -- Evidence
    stability_days       INT,            -- how many days has this workload run continuously
    uptime_pct           NUMERIC(5,2),
    confidence_pct       NUMERIC(5,2),
    reasoning            TEXT,
    status               TEXT DEFAULT 'open',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ══════════════════════════════════════════════════════════════════════════════
-- GAP 3: VPA COMPARISON — track VPA recommendations vs ours
-- ══════════════════════════════════════════════════════════════════════════════

CREATE TABLE vpa_recommendations (
    id                   UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    cluster_id           UUID REFERENCES k8s_clusters(id),
    namespace            TEXT NOT NULL,
    target_name          TEXT NOT NULL,  -- Deployment/StatefulSet name
    target_kind          TEXT NOT NULL,
    container_name       TEXT,
    -- VPA's suggestion (resource-only, no cost awareness)
    vpa_cpu_request      TEXT,   -- e.g. "250m"
    vpa_mem_request      TEXT,   -- e.g. "512Mi"
    -- Our suggestion (cost-aware + NIC/IOPS validated)
    our_cpu_request      TEXT,
    our_mem_request      TEXT,
    -- Cost impact (VPA cannot compute this — our differentiator)
    current_monthly_cost NUMERIC(12,4),
    our_monthly_cost     NUMERIC(12,4),
    monthly_savings      NUMERIC(12,4),
    -- Constraint checks VPA ignores
    nic_limit_ok         BOOLEAN,
    iops_limit_ok        BOOLEAN,
    has_hpa              BOOLEAN,   -- VPA + HPA conflict warning
    dr_constraint        BOOLEAN,   -- part of DR configuration
    -- Agreement
    agrees_with_vpa      BOOLEAN,
    disagreement_reason  TEXT,      -- why we differ from VPA
    observed_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Views ────────────────────────────────────────────────────────────────────

-- Top cost movers (7d vs prior 7d)
CREATE VIEW v_cost_movers AS
SELECT
    provider,
    service_name,
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost ELSE 0 END) AS cost_7d,
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END) AS cost_prior_7d,
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost ELSE 0 END) -
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END) AS delta,
    CASE
        WHEN SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END) > 0
        THEN ROUND(100.0 * (
            SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost ELSE 0 END) -
            SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END)
        ) / SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END), 2)
        ELSE NULL
    END AS change_pct
FROM daily_cost_summary
WHERE summary_date >= CURRENT_DATE - 14
GROUP BY provider, service_name
ORDER BY ABS(
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost ELSE 0 END) -
    SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost ELSE 0 END)
) DESC NULLS LAST;

-- Open recommendations summary
CREATE VIEW v_recommendations_summary AS
SELECT
    rec_type,
    COUNT(*) AS count,
    SUM(monthly_savings) AS total_monthly_savings,
    AVG(confidence_pct) AS avg_confidence
FROM recommendations
WHERE status = 'open'
GROUP BY rec_type
ORDER BY total_monthly_savings DESC NULLS LAST;

-- ══════════════════════════════════════════════════════════════════════════════
-- PHASE 3 ADDITIONS
-- ══════════════════════════════════════════════════════════════════════════════

-- 1. Track when anomalies were sent to Slack (prevents re-alerting)
ALTER TABLE anomalies ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ;
ALTER TABLE anomalies ADD COLUMN IF NOT EXISTS notification_channel TEXT;

-- 2. Tag inference notifier support
-- NOTE: inferred_tags is defined once, above (line ~369), as the multi-tenant
-- version with signal_type/confidence/conflicts_with. A second, incompatible
-- definition used to live here and was silently skipped by IF NOT EXISTS,
-- while run_tag_inference() inserted into ITS columns -- guaranteed runtime
-- crash. Fixed by extending the real table instead of redefining it.
ALTER TABLE inferred_tags ADD COLUMN IF NOT EXISTS notified_at TIMESTAMPTZ;
ALTER TABLE inferred_tags ADD COLUMN IF NOT EXISTS accepted_by TEXT;

CREATE INDEX IF NOT EXISTS idx_inferred_pending ON inferred_tags (accepted, notified_at)
    WHERE accepted IS NULL;

-- 3. Efficiency score history (computed nightly by attributor)
CREATE TABLE IF NOT EXISTS efficiency_scores (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    score_date          DATE NOT NULL UNIQUE,
    utilization_eff     NUMERIC(5,2),
    allocation_cov      NUMERIC(5,2),
    commitment_util     NUMERIC(5,2),
    hygiene_score       NUMERIC(5,2),
    composite_score     NUMERIC(5,2),
    score_tier          TEXT GENERATED ALWAYS AS (
        CASE
            WHEN composite_score >= 90 THEN 'Elite'
            WHEN composite_score >= 70 THEN 'Good'
            WHEN composite_score >= 50 THEN 'Fair'
            ELSE 'Poor'
        END
    ) STORED,
    computed_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);