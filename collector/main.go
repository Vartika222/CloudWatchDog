package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// ── FOCUS-normalized billing record ─────────────────────────────────────────
type BillingRecord struct {
	Provider           string          `json:"provider"`
	BillingAccountID   string          `json:"billing_account_id"`
	BillingAccountName string          `json:"billing_account_name"`
	ServiceName        string          `json:"service_name"`
	ServiceCategory    string          `json:"service_category"`
	Region             string          `json:"region"`
	AvailabilityZone   string          `json:"availability_zone"`
	ResourceID         string          `json:"resource_id"`
	ResourceName       string          `json:"resource_name"`
	ResourceType       string          `json:"resource_type"`
	BilledCost         float64         `json:"billed_cost"`
	EffectiveCost      float64         `json:"effective_cost"`
	ListCost           float64         `json:"list_cost"`
	AmortizedCost      float64         `json:"amortized_cost"`
	Currency           string          `json:"currency"`
	BillingPeriodStart time.Time       `json:"billing_period_start"`
	BillingPeriodEnd   time.Time       `json:"billing_period_end"`
	UsageStart         time.Time       `json:"usage_start"`
	UsageEnd           time.Time       `json:"usage_end"`
	UsageQuantity      float64         `json:"usage_quantity"`
	UsageUnit          string          `json:"usage_unit"`
	Tags               map[string]string `json:"tags"`
}

// ── Resource inventory record ────────────────────────────────────────────────
type ResourceRecord struct {
	Provider         string            `json:"provider"`
	AccountID        string            `json:"account_id"`
	ResourceID       string            `json:"resource_id"`
	ResourceName     string            `json:"resource_name"`
	ResourceType     string            `json:"resource_type"`
	Region           string            `json:"region"`
	Status           string            `json:"status"`
	Configuration    map[string]any    `json:"configuration"`
	Tags             map[string]string `json:"tags"`
	CPUCores         *int              `json:"cpu_cores,omitempty"`
	MemoryGB         *float64          `json:"memory_gb,omitempty"`
	StorageGB        *float64          `json:"storage_gb,omitempty"`
	NICCount         *int              `json:"nic_count,omitempty"`
	CommitmentType   string            `json:"commitment_type"`
	LastActiveAt     *time.Time        `json:"last_active_at,omitempty"`
	IsOrphan         bool              `json:"is_orphan"`
	OrphanReason     string            `json:"orphan_reason,omitempty"`
}

// ── Collector ─────────────────────────────────────────────────────────────────
type Collector struct {
	db         *pgxpool.Pool
	rdb        *redis.Client
	vmURL      string
	demoMode   bool
	awsEnabled bool
	azEnabled  bool
}

func NewCollector(db *pgxpool.Pool, rdb *redis.Client, vmURL string) *Collector {
	return &Collector{
		db:         db,
		rdb:        rdb,
		vmURL:      vmURL,
		demoMode:   os.Getenv("DEMO_MODE") == "true",
		awsEnabled: os.Getenv("AWS_ENABLED") == "true",
		azEnabled:  os.Getenv("AZURE_ENABLED") == "true",
	}
}

func (c *Collector) Run(ctx context.Context) {
	intervalStr := os.Getenv("COLLECTION_INTERVAL")
	if intervalStr == "" {
		intervalStr = "60s"
	}
	interval, err := time.ParseDuration(intervalStr)
	if err != nil {
		interval = 60 * time.Second
	}

	log.Printf("Collector starting. demo=%v aws=%v azure=%v interval=%s",
		c.demoMode, c.awsEnabled, c.azEnabled, interval)

	// Run once immediately
	c.collect(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	log.Println("Starting collection run...")
	start := time.Now()

	if c.demoMode {
		c.collectDemoIncremental(ctx)
	} else {
		if c.awsEnabled {
			c.collectAWS(ctx)
		}
		if c.azEnabled {
			c.collectAzure(ctx)
		}
	}

	// Always run analysis passes
	c.detectOrphanResources(ctx)
	c.updateResourceCostSummaries(ctx)
	c.runAnomalyDetection(ctx)
	c.generateRecommendations(ctx)

	log.Printf("Collection run complete in %s", time.Since(start))
}

// ── Demo mode: push small incremental updates so the UI shows live data ───────
func (c *Collector) collectDemoIncremental(ctx context.Context) {
	now := time.Now()
	today := now.Truncate(24 * time.Hour)

	// AWS services to update
	services := []struct {
		name     string
		category string
		baseCost float64
	}{
		{"EC2", "Compute", 140},
		{"EKS", "Compute", 70},
		{"RDS", "Database", 60},
		{"Data Transfer", "Networking", 37},
		{"NAT Gateway", "Networking", 30},
		{"S3", "Storage", 21},
	}

	for _, svc := range services {
		cost := svc.baseCost * (1 + (rand.Float64()-0.5)*0.12)
		_, err := c.db.Exec(ctx, `
			INSERT INTO daily_cost_summary (summary_date, provider, account_id, service_name, region, total_cost, resource_count)
			VALUES ($1, 'aws', '123456789012', $2, 'us-east-1', $3, $4)
			ON CONFLICT (summary_date, provider, account_id, service_name, region)
			DO UPDATE SET total_cost = EXCLUDED.total_cost
		`, today, svc.name, cost, rand.Intn(5)+1)
		if err != nil {
			log.Printf("WARN: demo update failed for %s: %v", svc.name, err)
		}
	}

	// Azure services
	azureServices := []struct {
		name     string
		baseCost float64
	}{
		{"Virtual Machines", 127},
		{"AKS", 63},
		{"Azure SQL", 47},
		{"Bandwidth", 21},
		{"Cosmos DB", 25},
	}
	for _, svc := range azureServices {
		cost := svc.baseCost * (1 + (rand.Float64()-0.5)*0.10)
		_, err := c.db.Exec(ctx, `
			INSERT INTO daily_cost_summary (summary_date, provider, account_id, service_name, region, total_cost, resource_count)
			VALUES ($1, 'azure', 'sub-a1b2c3d4-prod', $2, 'eastus', $3, $4)
			ON CONFLICT (summary_date, provider, account_id, service_name, region)
			DO UPDATE SET total_cost = EXCLUDED.total_cost
		`, today, svc.name, cost, rand.Intn(5)+1)
		if err != nil {
			log.Printf("WARN: demo azure update failed for %s: %v", svc.name, err)
		}
	}

	// Cache collection timestamp in Redis
	c.rdb.Set(ctx, "last_collection_at", now.Format(time.RFC3339), 5*time.Minute)
	log.Printf("Demo incremental update complete")
}

// ── Real AWS collection (requires credentials) ────────────────────────────────
func (c *Collector) collectAWS(ctx context.Context) {
	log.Println("AWS collection: not yet implemented (set DEMO_MODE=true for local dev)")
	// TODO Phase 2: implement aws-sdk-go-v2 Cost Explorer ingestion
	// Key APIs:
	//   costexplorer.GetCostAndUsage → daily costs by service/region
	//   ec2.DescribeInstances        → resource inventory
	//   ec2.DescribeVolumes          → orphan volume detection
	//   ce.GetReservationCoverage    → RI coverage gaps
}

// ── Real Azure collection (requires credentials) ──────────────────────────────
func (c *Collector) collectAzure(ctx context.Context) {
	log.Println("Azure collection: not yet implemented (set DEMO_MODE=true for local dev)")
	// TODO Phase 2: implement azure-sdk-for-go Cost Management ingestion
	// Key APIs:
	//   costmanagement.Query         → daily costs
	//   compute.VirtualMachinesList  → resource inventory
	//   compute.DisksList            → orphan disk detection
	//   reservations.List            → RI coverage
}

// ── Orphan resource detection ─────────────────────────────────────────────────
func (c *Collector) detectOrphanResources(ctx context.Context) {
	// Mark volumes/disks with no recent activity as orphans
	_, err := c.db.Exec(ctx, `
		UPDATE resources
		SET is_orphan = TRUE,
		    orphan_reason = 'No activity detected in 45+ days',
		    updated_at = NOW()
		WHERE resource_type IN ('ebs_volume', 'managed_disk', 'elastic_ip', 'public_ip')
		  AND last_active_at < NOW() - INTERVAL '45 days'
		  AND is_orphan = FALSE
	`)
	if err != nil {
		log.Printf("WARN: orphan detection failed: %v", err)
	}
}

// ── Update resource cost summaries ───────────────────────────────────────────
func (c *Collector) updateResourceCostSummaries(ctx context.Context) {
	_, err := c.db.Exec(ctx, `
		UPDATE resources r
		SET
			cost_30d = (
				SELECT COALESCE(SUM(billed_cost), 0)
				FROM billing_records b
				WHERE b.resource_id = r.resource_id
				  AND b.usage_start >= NOW() - INTERVAL '30 days'
			),
			cost_7d = (
				SELECT COALESCE(SUM(billed_cost), 0)
				FROM billing_records b
				WHERE b.resource_id = r.resource_id
				  AND b.usage_start >= NOW() - INTERVAL '7 days'
			),
			updated_at = NOW()
	`)
	if err != nil {
		log.Printf("WARN: cost summary update failed: %v", err)
	}
}

// ── Statistical anomaly detection (z-score on rolling 14-day baseline) ────────
func (c *Collector) runAnomalyDetection(ctx context.Context) {
	rows, err := c.db.Query(ctx, `
		WITH daily_costs AS (
			SELECT
				provider,
				account_id,
				service_name,
				summary_date,
				total_cost,
				AVG(total_cost) OVER (
					PARTITION BY provider, account_id, service_name
					ORDER BY summary_date
					ROWS BETWEEN 14 PRECEDING AND 1 PRECEDING
				) AS rolling_avg,
				STDDEV(total_cost) OVER (
					PARTITION BY provider, account_id, service_name
					ORDER BY summary_date
					ROWS BETWEEN 14 PRECEDING AND 1 PRECEDING
				) AS rolling_stddev
			FROM daily_cost_summary
			WHERE summary_date >= CURRENT_DATE - 30
		)
		SELECT
			provider, account_id, service_name, summary_date,
			total_cost, rolling_avg, rolling_stddev,
			CASE WHEN rolling_stddev > 0
				THEN (total_cost - rolling_avg) / rolling_stddev
				ELSE 0
			END AS zscore
		FROM daily_costs
		WHERE rolling_avg IS NOT NULL
		  AND rolling_stddev > 0
		  AND summary_date = CURRENT_DATE - 1
		  AND ABS((total_cost - rolling_avg) / rolling_stddev) > 2.5
	`)
	if err != nil {
		log.Printf("WARN: anomaly query failed: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var provider, accountID, serviceName string
		var summaryDate time.Time
		var totalCost, rollingAvg, rollingStddev, zscore float64

		if err := rows.Scan(&provider, &accountID, &serviceName, &summaryDate,
			&totalCost, &rollingAvg, &rollingStddev, &zscore); err != nil {
			continue
		}

		deviationPct := 0.0
		if rollingAvg > 0 {
			deviationPct = ((totalCost - rollingAvg) / rollingAvg) * 100
		}

		severity := "medium"
		if zscore > 3.5 || deviationPct > 100 {
			severity = "high"
		} else if zscore < 2.0 {
			severity = "low"
		}

		direction := "above"
		if zscore < 0 {
			direction = "below"
		}

		description := fmt.Sprintf(
			"%s costs on %s were %.1f%% %s the 14-day baseline (z-score: %.2f). Daily cost: $%.2f vs baseline: $%.2f.",
			serviceName, summaryDate.Format("Jan 2"),
			abs(deviationPct), direction, zscore, totalCost, rollingAvg,
		)

		impactUSD := abs(totalCost - rollingAvg)

		// Insert only if no open anomaly for this service in last 3 days
		_, err = c.db.Exec(ctx, `
			INSERT INTO anomalies (
				anomaly_type, severity, provider, account_id, resource_name,
				detected_at, metric_name, metric_value, baseline_value,
				deviation_pct, zscore, description, impact_usd, status
			)
			SELECT $1,$2,$3,$4,$5,NOW(),$6,$7,$8,$9,$10,$11,$12,'open'
			WHERE NOT EXISTS (
				SELECT 1 FROM anomalies
				WHERE provider=$3 AND account_id=$4 AND resource_name=$5
				  AND status='open' AND detected_at > NOW() - INTERVAL '3 days'
			)
		`,
			"cost_spike", severity, provider, accountID, serviceName,
			"billed_cost_daily", totalCost, rollingAvg,
			deviationPct, zscore, description, impactUSD,
		)
		if err != nil {
			log.Printf("WARN: anomaly insert failed: %v", err)
		}
	}
}

// ── Recommendation generator ──────────────────────────────────────────────────
func (c *Collector) generateRecommendations(ctx context.Context) {
	// Orphan resource recommendations
	rows, err := c.db.Query(ctx, `
		SELECT provider, account_id, resource_id, resource_name, resource_type,
		       cost_30d, last_active_at
		FROM resources
		WHERE is_orphan = TRUE
		  AND cost_30d > 5
		  AND NOT EXISTS (
				SELECT 1 FROM recommendations
				WHERE resource_id = resources.resource_id
				  AND rec_type = 'orphan_delete'
				  AND status = 'open'
		  )
		LIMIT 20
	`)
	if err != nil {
		log.Printf("WARN: orphan rec query failed: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var provider, accountID, resourceID, resourceName, resourceType string
		var cost30d float64
		var lastActive *time.Time

		if err := rows.Scan(&provider, &accountID, &resourceID, &resourceName,
			&resourceType, &cost30d, &lastActive); err != nil {
			continue
		}

		daysIdle := 0
		if lastActive != nil {
			daysIdle = int(time.Since(*lastActive).Hours() / 24)
		}

		currentCfg, _ := json.Marshal(map[string]any{
			"resource_type": resourceType,
			"days_idle":     daysIdle,
			"monthly_cost":  cost30d,
		})
		recCfg, _ := json.Marshal(map[string]any{"action": "delete", "snapshot_first": true})

		title := fmt.Sprintf("Delete orphan %s '%s' (idle %d days, $%.2f/mo)",
			resourceType, resourceName, daysIdle, cost30d)

		_, _ = c.db.Exec(ctx, `
			INSERT INTO recommendations (
				rec_type, priority, provider, account_id, resource_id, resource_name, resource_type,
				title, description, current_config, recommended_config,
				monthly_savings, annual_savings, confidence_pct, evidence_days, status
			) VALUES (
				'orphan_delete', 'medium', $1, $2, $3, $4, $5,
				$6, $7, $8, $9,
				$10, $11, 99.0, $12, 'open'
			)
		`,
			provider, accountID, resourceID, resourceName, resourceType,
			title,
			fmt.Sprintf("Resource '%s' has been idle for %d days. No activity detected.", resourceName, daysIdle),
			currentCfg, recCfg,
			cost30d, cost30d*12, daysIdle,
		)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// ── Main ──────────────────────────────────────────────────────────────────────
func main() {
	ctx := context.Background()

	dbDSN := os.Getenv("DB_DSN")
	if dbDSN == "" {
		dbDSN = "postgres://oe_user:oe_pass@localhost:5432/observability?sslmode=disable"
	}

	vmURL := os.Getenv("VM_URL")
	if vmURL == "" {
		vmURL = "http://localhost:8428"
	}

	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "localhost:6379"
	}

	// Connect to Postgres with retries
	var db *pgxpool.Pool
	var err error
	for i := 0; i < 15; i++ {
		db, err = pgxpool.New(ctx, dbDSN)
		if err == nil {
			if pingErr := db.Ping(ctx); pingErr == nil {
				break
			}
		}
		log.Printf("Waiting for database... attempt %d/15", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Connect to Redis
	redisPort, _ := strconv.Atoi("6379")
	_ = redisPort
	rdb := redis.NewClient(&redis.Options{Addr: redisURL})

	collector := NewCollector(db, rdb, vmURL)
	collector.Run(ctx)
}
