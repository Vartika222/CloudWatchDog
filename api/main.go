package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type Server struct {
	db  *pgxpool.Pool
	rdb *redis.Client
}

// ── Response helpers ──────────────────────────────────────────────────────────
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON error: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ── Overview / Summary ────────────────────────────────────────────────────────
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type overview struct {
		TotalSpend30d       float64 `json:"total_spend_30d"`
		TotalSpend7d        float64 `json:"total_spend_7d"`
		TotalSpendYesterday float64 `json:"total_spend_yesterday"`
		ChangeVsPrior7d     float64 `json:"change_vs_prior_7d_pct"`
		OpenAnomalies       int     `json:"open_anomalies"`
		CriticalAnomalies   int     `json:"critical_anomalies"`
		OpenRecommendations int     `json:"open_recommendations"`
		TotalMonthlySavings float64 `json:"total_monthly_savings"`
		OrphanResources     int     `json:"orphan_resources"`
		OrphanMonthlyCost   float64 `json:"orphan_monthly_cost"`
		ProviderBreakdown   []map[string]any `json:"provider_breakdown"`
	}

	var o overview

	// Spend totals
	s.db.QueryRow(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN summary_date >= CURRENT_DATE - 30 THEN total_cost END), 0),
			COALESCE(SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost END), 0),
			COALESCE(SUM(CASE WHEN summary_date = CURRENT_DATE - 1 THEN total_cost END), 0),
			COALESCE(SUM(CASE WHEN summary_date >= CURRENT_DATE - 7 THEN total_cost END), 0) -
			COALESCE(SUM(CASE WHEN summary_date >= CURRENT_DATE - 14 AND summary_date < CURRENT_DATE - 7 THEN total_cost END), 0)
		FROM daily_cost_summary
	`).Scan(&o.TotalSpend30d, &o.TotalSpend7d, &o.TotalSpendYesterday, &o.ChangeVsPrior7d)

	// Anomalies
	s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anomalies WHERE status='open'`).Scan(&o.OpenAnomalies)
	s.db.QueryRow(ctx, `SELECT COUNT(*) FROM anomalies WHERE status='open' AND severity IN ('high','critical')`).Scan(&o.CriticalAnomalies)

	// Recommendations
	s.db.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(monthly_savings),0) FROM recommendations WHERE status='open'`).
		Scan(&o.OpenRecommendations, &o.TotalMonthlySavings)

	// Orphans
	s.db.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(cost_30d),0) FROM resources WHERE is_orphan=TRUE`).
		Scan(&o.OrphanResources, &o.OrphanMonthlyCost)

	// Provider breakdown
	rows, err := s.db.Query(ctx, `
		SELECT provider,
			SUM(CASE WHEN summary_date >= CURRENT_DATE - 30 THEN total_cost ELSE 0 END) as spend_30d,
			SUM(CASE WHEN summary_date >= CURRENT_DATE - 7  THEN total_cost ELSE 0 END) as spend_7d
		FROM daily_cost_summary
		GROUP BY provider
		ORDER BY spend_30d DESC
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var provider string
			var spend30d, spend7d float64
			rows.Scan(&provider, &spend30d, &spend7d)
			o.ProviderBreakdown = append(o.ProviderBreakdown, map[string]any{
				"provider": provider, "spend_30d": spend30d, "spend_7d": spend7d,
			})
		}
	}

	writeJSON(w, http.StatusOK, o)
}

// ── Daily cost time series ────────────────────────────────────────────────────
func (s *Server) handleDailyCosts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := r.URL.Query().Get("provider")
	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 && d <= 90 {
		days = d
	}

	query := `
		SELECT summary_date, provider, SUM(total_cost) as total
		FROM daily_cost_summary
		WHERE summary_date >= CURRENT_DATE - $1
	`
	args := []any{days}
	if provider != "" {
		query += ` AND provider = $2`
		args = append(args, provider)
	}
	query += ` GROUP BY summary_date, provider ORDER BY summary_date ASC`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type DailyCost struct {
		Date     string  `json:"date"`
		Provider string  `json:"provider"`
		Total    float64 `json:"total"`
	}

	var result []DailyCost
	for rows.Next() {
		var dc DailyCost
		var d time.Time
		rows.Scan(&d, &dc.Provider, &dc.Total)
		dc.Date = d.Format("2006-01-02")
		result = append(result, dc)
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Service breakdown ─────────────────────────────────────────────────────────
func (s *Server) handleServiceBreakdown(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := r.URL.Query().Get("provider")

	query := `
		SELECT provider, service_name, service_category,
			SUM(billed_cost) as cost_30d,
			SUM(CASE WHEN usage_start >= NOW() - INTERVAL '7 days' THEN billed_cost ELSE 0 END) as cost_7d
		FROM billing_records
		WHERE usage_start >= NOW() - INTERVAL '30 days'
	`
	args := []any{}
	if provider != "" {
		query += ` AND provider = $1`
		args = append(args, provider)
	}
	query += ` GROUP BY provider, service_name, service_category ORDER BY cost_30d DESC LIMIT 20`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var provider, service, category string
		var cost30d, cost7d float64
		rows.Scan(&provider, &service, &category, &cost30d, &cost7d)
		result = append(result, map[string]any{
			"provider": provider, "service": service, "category": category,
			"cost_30d": cost30d, "cost_7d": cost7d,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Kubernetes cost map ───────────────────────────────────────────────────────
func (s *Server) handleK8sCostMap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.Query(ctx, `
		SELECT
			kc.cluster_name, kc.provider, kc.region,
			ndc.namespace,
			COALESCE(SUM(ndc.total_cost), 0) as cost_30d,
			COALESCE(SUM(ndc.egress_cost), 0) as egress_cost_30d,
			COALESCE(SUM(ndc.compute_cost), 0) as compute_cost_30d,
			COALESCE(SUM(ndc.memory_cost), 0) as memory_cost_30d,
			COALESCE(MAX(ndc.pod_count), 0) as pod_count
		FROM namespace_daily_cost ndc
		JOIN k8s_clusters kc ON kc.id = ndc.cluster_id
		WHERE ndc.summary_date >= CURRENT_DATE - 30
		GROUP BY kc.cluster_name, kc.provider, kc.region, ndc.namespace
		ORDER BY cost_30d DESC
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var clusterName, provider, region, namespace string
		var cost30d, egressCost, computeCost, memoryCost float64
		var podCount int
		rows.Scan(&clusterName, &provider, &region, &namespace, &cost30d, &egressCost, &computeCost, &memoryCost, &podCount)
		result = append(result, map[string]any{
			"cluster": clusterName, "provider": provider, "region": region,
			"namespace": namespace, "cost_30d": cost30d, "egress_cost_30d": egressCost,
			"compute_cost_30d": computeCost, "memory_cost_30d": memoryCost,
			"pod_count": podCount,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Top pods by cost ──────────────────────────────────────────────────────────
func (s *Server) handleTopPods(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	namespace := r.URL.Query().Get("namespace")
	limit := 20

	query := `
		SELECT
			namespace, pod_name, service_name,
			SUM(total_cost) as cost_7d,
			SUM(egress_cost_inter_az + egress_cost_internet) as egress_cost,
			AVG(cpu_used_cores / NULLIF(cpu_request_cores, 0)) * 100 as cpu_util_pct,
			AVG(memory_used_bytes::float / NULLIF(memory_request_bytes, 0)) * 100 as mem_util_pct
		FROM pod_cost_attribution
		WHERE window_start >= NOW() - INTERVAL '7 days'
	`
	args := []any{}
	if namespace != "" {
		query += ` AND namespace = $1`
		args = append(args, namespace)
		query += ` GROUP BY namespace, pod_name, service_name ORDER BY cost_7d DESC LIMIT $2`
		args = append(args, limit)
	} else {
		query += ` GROUP BY namespace, pod_name, service_name ORDER BY cost_7d DESC LIMIT $1`
		args = append(args, limit)
	}

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var ns, pod, svc string
		var cost7d, egressCost float64
		var cpuPct, memPct *float64
		rows.Scan(&ns, &pod, &svc, &cost7d, &egressCost, &cpuPct, &memPct)
		result = append(result, map[string]any{
			"namespace": ns, "pod": pod, "service": svc,
			"cost_7d": cost7d, "egress_cost": egressCost,
			"cpu_util_pct": cpuPct, "mem_util_pct": memPct,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Egress flows ──────────────────────────────────────────────────────────────
func (s *Server) handleEgressFlows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.Query(ctx, `
		SELECT
			ef.src_namespace, ef.src_service,
			ef.dst_namespace, ef.dst_service,
			ef.dst_ip::text,
			ef.flow_type,
			SUM(ef.bytes_total) as bytes_total,
			SUM(ef.egress_cost) as egress_cost,
			kc.cluster_name
		FROM egress_flows ef
		JOIN k8s_clusters kc ON kc.id = ef.cluster_id
		WHERE ef.window_start >= NOW() - INTERVAL '7 days'
		GROUP BY ef.src_namespace, ef.src_service, ef.dst_namespace,
		         ef.dst_service, ef.dst_ip::text, ef.flow_type, kc.cluster_name
		ORDER BY egress_cost DESC
		LIMIT 50
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var srcNS, srcSvc string
		var dstNS, dstSvc, dstIP, flowType, cluster *string
		var bytes float64
		var cost float64
		rows.Scan(&srcNS, &srcSvc, &dstNS, &dstSvc, &dstIP, &flowType, &bytes, &cost, &cluster)
		result = append(result, map[string]any{
			"src_namespace": srcNS, "src_service": srcSvc,
			"dst_namespace": dstNS, "dst_service": dstSvc,
			"dst_ip": dstIP, "flow_type": flowType,
			"bytes_total": bytes, "egress_cost": cost, "cluster": cluster,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Anomalies ─────────────────────────────────────────────────────────────────
func (s *Server) handleAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "open"
	}

	rows, err := s.db.Query(ctx, `
		SELECT id, anomaly_type, severity, provider, account_id,
		       COALESCE(resource_name,''), COALESCE(namespace,''), COALESCE(pod_name,''),
		       detected_at, metric_name,
		       COALESCE(metric_value,0), COALESCE(baseline_value,0),
		       COALESCE(deviation_pct,0), COALESCE(zscore,0),
		       description, COALESCE(impact_usd,0), status
		FROM anomalies
		WHERE status = $1
		ORDER BY
			CASE severity WHEN 'critical' THEN 1 WHEN 'high' THEN 2 WHEN 'medium' THEN 3 ELSE 4 END,
			detected_at DESC
		LIMIT 50
	`, status)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, atype, severity, provider, accountID, resourceName, namespace, podName, metricName, description, astatus string
		var detectedAt time.Time
		var metricVal, baselineVal, deviationPct, zscore, impactUSD float64
		rows.Scan(&id, &atype, &severity, &provider, &accountID, &resourceName, &namespace, &podName,
			&detectedAt, &metricName, &metricVal, &baselineVal, &deviationPct, &zscore, &description, &impactUSD, &astatus)
		result = append(result, map[string]any{
			"id": id, "type": atype, "severity": severity,
			"provider": provider, "account_id": accountID,
			"resource": resourceName, "namespace": namespace, "pod": podName,
			"detected_at": detectedAt, "metric": metricName,
			"metric_value": metricVal, "baseline": baselineVal,
			"deviation_pct": deviationPct, "zscore": zscore,
			"description": description, "impact_usd": impactUSD, "status": astatus,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Update anomaly status ─────────────────────────────────────────────────────
func (s *Server) handleUpdateAnomaly(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var body struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	var resolvedAt *time.Time
	if body.Status == "resolved" {
		t := time.Now()
		resolvedAt = &t
	}

	_, err := s.db.Exec(ctx, `
		UPDATE anomalies SET status=$1, resolution_note=$2, resolved_at=$3 WHERE id=$4
	`, body.Status, body.Note, resolvedAt, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ── Recommendations ───────────────────────────────────────────────────────────
func (s *Server) handleRecommendations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recType := r.URL.Query().Get("type")

	query := `
		SELECT id, rec_type, priority, provider, account_id,
		       COALESCE(resource_name,''), COALESCE(resource_type,''), COALESCE(namespace,''),
		       title, description,
		       COALESCE(monthly_savings,0), COALESCE(annual_savings,0), COALESCE(confidence_pct,0),
		       current_config, recommended_config,
		       COALESCE(iac_type,''), COALESCE(iac_patch,''),
		       COALESCE(pr_status,''), status, created_at
		FROM recommendations
		WHERE status = 'open'
	`
	args := []any{}
	if recType != "" {
		query += ` AND rec_type = $1`
		args = append(args, recType)
	}
	query += ` ORDER BY monthly_savings DESC NULLS LAST LIMIT 50`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, rtype, priority, provider, accountID, resourceName, resourceType, namespace string
		var title, description, iacType, iacPatch, prStatus, status string
		var monthlySavings, annualSavings, confidencePct float64
		var currentCfg, recCfg []byte
		var createdAt time.Time
		rows.Scan(&id, &rtype, &priority, &provider, &accountID, &resourceName, &resourceType, &namespace,
			&title, &description, &monthlySavings, &annualSavings, &confidencePct,
			&currentCfg, &recCfg, &iacType, &iacPatch, &prStatus, &status, &createdAt)

		result = append(result, map[string]any{
			"id": id, "type": rtype, "priority": priority,
			"provider": provider, "account_id": accountID,
			"resource": resourceName, "resource_type": resourceType, "namespace": namespace,
			"title": title, "description": description,
			"monthly_savings": monthlySavings, "annual_savings": annualSavings,
			"confidence_pct": confidencePct,
			"current_config": json.RawMessage(currentCfg),
			"recommended_config": json.RawMessage(recCfg),
			"iac_type": iacType, "iac_patch": iacPatch,
			"pr_status": prStatus, "status": status, "created_at": createdAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Dismiss / apply recommendation ───────────────────────────────────────────
func (s *Server) handleUpdateRecommendation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}

	_, err := s.db.Exec(ctx, `
		UPDATE recommendations SET status=$1, dismissed_reason=$2, updated_at=NOW() WHERE id=$3
	`, body.Status, body.Reason, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ── Forecasts ─────────────────────────────────────────────────────────────────
func (s *Server) handleForecasts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	provider := r.URL.Query().Get("provider")

	query := `
		SELECT cf.forecast_date, cf.provider, cf.account_id,
		       cf.forecasted_cost, cf.lower_bound, cf.upper_bound
		FROM cost_forecasts cf
		WHERE cf.forecast_date >= CURRENT_DATE
		  AND cf.service_name IS NULL
	`
	args := []any{}
	if provider != "" {
		query += ` AND cf.provider = $1`
		args = append(args, provider)
	}
	query += ` ORDER BY cf.provider, cf.forecast_date ASC`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var fdate time.Time
		var provider, accountID string
		var forecast, lower, upper float64
		rows.Scan(&fdate, &provider, &accountID, &forecast, &lower, &upper)
		result = append(result, map[string]any{
			"date": fdate.Format("2006-01-02"), "provider": provider,
			"forecast": forecast, "lower": lower, "upper": upper,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Resources / orphans ───────────────────────────────────────────────────────
func (s *Server) handleResources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	orphanOnly := r.URL.Query().Get("orphan") == "true"

	query := `
		SELECT provider, account_id, resource_id, resource_name, resource_type,
		       region, status, cost_30d, cost_7d,
		       is_orphan, COALESCE(orphan_reason,''),
		       COALESCE(commitment_type,'on_demand'),
		       cpu_cores, memory_gb, storage_gb, nic_count,
		       last_active_at, updated_at
		FROM resources
	`
	if orphanOnly {
		query += ` WHERE is_orphan = TRUE`
	}
	query += ` ORDER BY cost_30d DESC LIMIT 100`

	rows, err := s.db.Query(ctx, query)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var provider, accountID, resourceID, resourceName, resourceType string
		var region, status, orphanReason, commitmentType string
		var cost30d, cost7d float64
		var isOrphan bool
		var cpuCores, nicCount *int
		var memoryGB, storageGB *float64
		var lastActiveAt, updatedAt *time.Time
		rows.Scan(&provider, &accountID, &resourceID, &resourceName, &resourceType,
			&region, &status, &cost30d, &cost7d,
			&isOrphan, &orphanReason, &commitmentType,
			&cpuCores, &memoryGB, &storageGB, &nicCount,
			&lastActiveAt, &updatedAt)
		result = append(result, map[string]any{
			"provider": provider, "account_id": accountID,
			"resource_id": resourceID, "name": resourceName, "type": resourceType,
			"region": region, "status": status,
			"cost_30d": cost30d, "cost_7d": cost7d,
			"is_orphan": isOrphan, "orphan_reason": orphanReason,
			"commitment_type": commitmentType,
			"cpu_cores": cpuCores, "memory_gb": memoryGB,
			"storage_gb": storageGB, "nic_count": nicCount,
			"last_active_at": lastActiveAt,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── Cost movers ───────────────────────────────────────────────────────────────
func (s *Server) handleCostMovers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.Query(ctx, `SELECT provider, service_name, cost_7d, cost_prior_7d, delta, change_pct FROM v_cost_movers LIMIT 15`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var provider, service string
		var cost7d, priorCost, delta float64
		var changePct *float64
		rows.Scan(&provider, &service, &cost7d, &priorCost, &delta, &changePct)
		result = append(result, map[string]any{
			"provider": provider, "service": service,
			"cost_7d": cost7d, "cost_prior_7d": priorCost,
			"delta": delta, "change_pct": changePct,
		})
	}
	writeJSON(w, http.StatusOK, result)
}

// ── GAP 4: Tag coverage ──────────────────────────────────────────────────────
func (s *Server) handleTagCoverage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, `SELECT * FROM v_tag_coverage`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer rows.Close()
	var coverage []map[string]any
	for rows.Next() {
		var provider string
		var total, tagged, untagged int
		var coveragePct, untaggedCost float64
		rows.Scan(&provider, &total, &tagged, &untagged, &coveragePct, &untaggedCost)
		coverage = append(coverage, map[string]any{
			"provider": provider, "total": total, "tagged": tagged,
			"untagged": untagged, "coverage_pct": coveragePct, "untagged_cost_30d": untaggedCost,
		})
	}
	tagRows, err := s.db.Query(ctx, `
		SELECT resource_id, provider, tag_key, tag_value, confidence, signal_type, COALESCE(signal_detail,'')
		FROM inferred_tags WHERE accepted IS NULL ORDER BY confidence DESC LIMIT 50`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer tagRows.Close()
	var pending []map[string]any
	for tagRows.Next() {
		var resourceID, provider, tagKey, tagValue, signalType, signalDetail string
		var confidence float64
		tagRows.Scan(&resourceID, &provider, &tagKey, &tagValue, &confidence, &signalType, &signalDetail)
		pending = append(pending, map[string]any{
			"resource_id": resourceID, "provider": provider, "tag_key": tagKey,
			"tag_value": tagValue, "confidence": confidence, "signal": signalType, "detail": signalDetail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"coverage": coverage, "pending_inferences": pending, "pending_count": len(pending)})
}

// ── GAP 2: Commitment portfolio ───────────────────────────────────────────────
func (s *Server) handleCommitmentAnalysis(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recRows, err := s.db.Query(ctx, `
		SELECT provider, account_id, rec_action, resource_type,
		       COALESCE(instance_family,''), COALESCE(region,''), quantity,
		       COALESCE(current_od_spend,0), COALESCE(monthly_savings,0),
		       COALESCE(break_even_months,0), COALESCE(risk_score,0),
		       COALESCE(stability_days,0), COALESCE(confidence_pct,0), COALESCE(reasoning,'')
		FROM commitment_recommendations WHERE status='open'
		ORDER BY monthly_savings DESC NULLS LAST LIMIT 20`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer recRows.Close()
	var recs []map[string]any
	for recRows.Next() {
		var provider, accountID, action, resType, instanceFamily, region, reasoning string
		var qty, stabilityDays int
		var odSpend, savings, breakEven, risk, confidence float64
		recRows.Scan(&provider, &accountID, &action, &resType, &instanceFamily, &region,
			&qty, &odSpend, &savings, &breakEven, &risk, &stabilityDays, &confidence, &reasoning)
		recs = append(recs, map[string]any{
			"provider": provider, "action": action, "resource_type": resType,
			"instance_family": instanceFamily, "od_spend": odSpend,
			"monthly_savings": savings, "break_even_months": breakEven,
			"risk_score": risk, "stability_days": stabilityDays,
			"confidence_pct": confidence, "reasoning": reasoning,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"recommendations": recs})
}

// ── GAP 3: VPA comparison ─────────────────────────────────────────────────────
func (s *Server) handleVPAComparison(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, `
		SELECT v.namespace, v.target_name, v.target_kind,
		       COALESCE(v.vpa_cpu_request,''), COALESCE(v.vpa_mem_request,''),
		       COALESCE(v.our_cpu_request,''), COALESCE(v.our_mem_request,''),
		       COALESCE(v.current_monthly_cost,0), COALESCE(v.our_monthly_cost,0), COALESCE(v.monthly_savings,0),
		       v.has_hpa, v.dr_constraint, v.agrees_with_vpa, COALESCE(v.disagreement_reason,''),
		       v.nic_limit_ok, v.iops_limit_ok, kc.cluster_name, kc.provider
		FROM vpa_recommendations v
		JOIN k8s_clusters kc ON kc.id = v.cluster_id
		ORDER BY v.monthly_savings DESC NULLS LAST LIMIT 50`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer rows.Close()
	var result []map[string]any
	disagreements := 0
	totalSavings := 0.0
	for rows.Next() {
		var ns, target, kind, vpaCPU, vpaMem, ourCPU, ourMem, cluster, provider, disagreeReason string
		var currentCost, ourCost, savings float64
		var hasHPA, drConstraint, agreesVPA, nicOK, iopsOK bool
		rows.Scan(&ns, &target, &kind, &vpaCPU, &vpaMem, &ourCPU, &ourMem,
			&currentCost, &ourCost, &savings, &hasHPA, &drConstraint,
			&agreesVPA, &disagreeReason, &nicOK, &iopsOK, &cluster, &provider)
		if !agreesVPA { disagreements++ }
		totalSavings += savings
		result = append(result, map[string]any{
			"namespace": ns, "target": target, "kind": kind,
			"vpa_cpu": vpaCPU, "vpa_mem": vpaMem, "our_cpu": ourCPU, "our_mem": ourMem,
			"current_cost": currentCost, "our_cost": ourCost, "savings": savings,
			"has_hpa": hasHPA, "dr_constraint": drConstraint,
			"agrees_with_vpa": agreesVPA, "disagreement_reason": disagreeReason,
			"nic_ok": nicOK, "iops_ok": iopsOK, "cluster": cluster, "provider": provider,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pods": result, "total_pods": len(result),
		"disagreements": disagreements, "total_savings": totalSavings,
	})
}

// ── GAP 5: Tenants ────────────────────────────────────────────────────────────
func (s *Server) handleTenants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, `SELECT id, tenant_slug, tenant_name, plan, is_msp FROM tenants ORDER BY created_at`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		var id, slug, name, plan string; var isMSP bool
		rows.Scan(&id, &slug, &name, &plan, &isMSP)
		result = append(result, map[string]any{"id": id, "slug": slug, "name": name, "plan": plan, "is_msp": isMSP})
	}
	writeJSON(w, http.StatusOK, result)
}


// ── GET /api/v1/costs/estimate ────────────────────────────────────────────────
// Called by the GitHub Action on every PR. Returns estimated monthly cost delta.
// ?changed_files=main.tf,helm/values.yaml
func (s *Server) handleCostEstimate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	changedFiles := r.URL.Query().Get("changed_files")

	// Fetch current total monthly spend
	var currentMonthly float64
	s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_cost), 0)
		FROM daily_cost_summary
		WHERE summary_date >= CURRENT_DATE - 30
	`).Scan(&currentMonthly)
	currentMonthly = currentMonthly / 30 * 30 // normalize to monthly

	// Fetch open rightsizing savings (available but not yet applied)
	var openSavings float64
	s.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(monthly_savings), 0)
		FROM recommendations
		WHERE status = 'open' AND rec_type = 'rightsize'
	`).Scan(&openSavings)

	// Warnings: rightsizing recs relevant to changed files
	type Warning struct {
		Resource       string  `json:"resource"`
		CurrentType    string  `json:"current_type"`
		RecommendedType string `json:"recommended_type"`
		MonthlySavings float64 `json:"monthly_savings"`
		AvgCPUPct      float64 `json:"avg_cpu_pct"`
		Reason         string  `json:"reason"`
	}
	var warnings []Warning

	rows, err := s.db.Query(ctx, `
		SELECT
			COALESCE(resource_name, ''),
			COALESCE(current_config->>'instance_type', current_config->>'vm_size', ''),
			COALESCE(recommended_config->>'instance_type', recommended_config->>'vm_size', ''),
			COALESCE(monthly_savings, 0),
			COALESCE((evidence_data->>'cpu_avg')::float, 0),
			COALESCE(description, '')
		FROM recommendations
		WHERE status = 'open'
		  AND rec_type = 'rightsize'
		ORDER BY monthly_savings DESC
		LIMIT 5
	`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var w Warning
			rows.Scan(&w.Resource, &w.CurrentType, &w.RecommendedType,
				&w.MonthlySavings, &w.AvgCPUPct, &w.Reason)
			warnings = append(warnings, w)
		}
	}

	// Estimated delta: if changed files touch terraform/helm, flag open savings
	estimatedDelta := 0.0
	if changedFiles != "" && openSavings > 0 {
		estimatedDelta = 0 // no change assumed — warnings surface the opportunity
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"current_monthly":   currentMonthly,
		"estimated_monthly": currentMonthly + estimatedDelta,
		"delta":             estimatedDelta,
		"open_savings":      openSavings,
		"changed_files":     changedFiles,
		"warnings":          warnings,
		"note": "delta is 0 until Pixie utilization data is connected — " +
			"warnings show rightsizing opportunities for resources in changed files",
	})
}

// ── POST /api/v1/recommendations/{id}/open-pr ─────────────────────────────────
// Creates a GitHub PR from the recommendation's iac_patch content.
func (s *Server) handleOpenPR(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	githubToken := os.Getenv("GITHUB_TOKEN")
	githubRepo  := os.Getenv("GITHUB_REPO") // format: "owner/repo"

	if githubToken == "" || githubRepo == "" {
		writeJSON(w, http.StatusNotImplemented, map[string]any{
			"error": "PR generation requires GITHUB_TOKEN and GITHUB_REPO env vars",
			"setup": "Set GITHUB_TOKEN=ghp_xxx and GITHUB_REPO=owner/repo in docker-compose.yml",
		})
		return
	}

	// Fetch the recommendation
	var title, description, iacType, iacPatch, recType string
	var monthlySavings float64
	err := s.db.QueryRow(ctx, `
		SELECT title, description, COALESCE(iac_type,''), COALESCE(iac_patch,''),
		       rec_type, COALESCE(monthly_savings,0)
		FROM recommendations WHERE id = $1
	`, id).Scan(&title, &description, &iacType, &iacPatch, &recType, &monthlySavings)
	if err != nil {
		writeError(w, http.StatusNotFound, "recommendation not found")
		return
	}

	if iacPatch == "" {
		writeError(w, http.StatusBadRequest, "this recommendation has no IaC patch")
		return
	}

	// Determine file extension from iac_type
	ext := map[string]string{
		"terraform": "tf",
		"helm":      "yaml",
		"bicep":     "bicep",
	}[iacType]
	if ext == "" {
		ext = "txt"
	}

	branchName := fmt.Sprintf("cost-opt/%s-%s", recType, id[:8])
	fileName   := fmt.Sprintf("cost-optimizations/%s.%s", recType, ext)
	commitMsg  := fmt.Sprintf("cost: %s (saves $%.0f/mo)", title, monthlySavings)
	prBody     := fmt.Sprintf("## Cost Optimization

**%s**

%s

"+
		"**Estimated saving:** $%.2f/month ($%.2f/year)

"+
		"*Generated by Cloud Observability Engine*",
		title, description, monthlySavings, monthlySavings*12)

	// Call GitHub API
	apiBase := fmt.Sprintf("https://api.github.com/repos/%s", githubRepo)
	headers := map[string]string{
		"Authorization": "Bearer " + githubToken,
		"Content-Type":  "application/json",
		"Accept":        "application/vnd.github+json",
	}

	doGitHub := func(method, path string, body map[string]any) (map[string]any, error) {
		bodyBytes, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, apiBase+path, bytes.NewReader(bodyBytes))
		for k, v := range headers { req.Header.Set(k, v) }
		resp, err := http.DefaultClient.Do(req)
		if err != nil { return nil, err }
		defer resp.Body.Close()
		var result map[string]any
		json.NewDecoder(resp.Body).Decode(&result)
		return result, nil
	}

	// 1. Get default branch SHA
	repoInfo, err := doGitHub("GET", "", nil)
	if err != nil { writeError(w, http.StatusInternalServerError, "github api failed"); return }
	defaultBranch, _ := repoInfo["default_branch"].(string)

	refInfo, _ := doGitHub("GET", fmt.Sprintf("/git/ref/heads/%s", defaultBranch), nil)
	shaObj, _ := refInfo["object"].(map[string]any)
	sha, _ := shaObj["sha"].(string)

	// 2. Create branch
	doGitHub("POST", "/git/refs", map[string]any{
		"ref": fmt.Sprintf("refs/heads/%s", branchName),
		"sha": sha,
	})

	// 3. Create file
	encodedContent := base64.StdEncoding.EncodeToString([]byte(iacPatch))
	doGitHub("PUT", fmt.Sprintf("/contents/%s", fileName), map[string]any{
		"message": commitMsg,
		"content": encodedContent,
		"branch":  branchName,
	})

	// 4. Open PR
	prResult, _ := doGitHub("POST", "/pulls", map[string]any{
		"title": commitMsg,
		"body":  prBody,
		"head":  branchName,
		"base":  defaultBranch,
	})

	prURL, _ := prResult["html_url"].(string)
	prNumber, _ := prResult["number"].(float64)

	// Update recommendation with PR info
	s.db.Exec(ctx, `
		UPDATE recommendations SET pr_url=$1, pr_status='open', updated_at=NOW()
		WHERE id=$2
	`, prURL, id)

	writeJSON(w, http.StatusCreated, map[string]any{
		"pr_url":    prURL,
		"pr_number": int(prNumber),
		"branch":    branchName,
		"message":   fmt.Sprintf("PR opened: %s", prURL),
	})
}

// ── GET /api/v1/tags/inferences ───────────────────────────────────────────────
func (s *Server) handleListInferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.db.Query(ctx, `
		SELECT it.id, it.resource_id, it.resource_name, it.provider,
		       it.tag_key, it.tag_value, it.confidence_pct, it.rule_name,
		       it.created_at,
		       COALESCE(r.cost_30d, 0) as monthly_cost
		FROM inferred_tags it
		LEFT JOIN resources r ON r.resource_id = it.resource_id
		WHERE it.accepted IS NULL
		ORDER BY it.confidence_pct DESC, r.cost_30d DESC NULLS LAST
		LIMIT 50
	`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var id, resourceID, provider, tagKey, tagValue, ruleName string
		var resourceName *string
		var confidence, monthlyCost float64
		var createdAt time.Time
		rows.Scan(&id, &resourceID, &resourceName, &provider, &tagKey, &tagValue,
			&confidence, &ruleName, &createdAt, &monthlyCost)
		result = append(result, map[string]any{
			"id": id, "resource_id": resourceID, "resource_name": resourceName,
			"provider": provider, "tag_key": tagKey, "tag_value": tagValue,
			"confidence_pct": confidence, "rule_name": ruleName,
			"monthly_cost": monthlyCost, "created_at": createdAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"inferences": result,
		"count": len(result),
	})
}

// ── PATCH /api/v1/tags/inferences/{id} ───────────────────────────────────────
func (s *Server) handleUpdateInference(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := chi.URLParam(r, "id")

	var body struct {
		Accepted   bool   `json:"accepted"`
		AcceptedBy string `json:"accepted_by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body"); return
	}

	_, err := s.db.Exec(ctx, `
		UPDATE inferred_tags
		SET accepted = $1, accepted_by = $2, updated_at = NOW()
		WHERE id = $3
	`, body.Accepted, body.AcceptedBy, id)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }

	// If accepted, write the tag back to the resource
	if body.Accepted {
		s.db.Exec(ctx, `
			UPDATE resources r
			SET tags = tags || (
				SELECT jsonb_build_object(it.tag_key, it.tag_value)
				FROM inferred_tags it WHERE it.id = $1
			),
			updated_at = NOW()
			FROM inferred_tags it
			WHERE it.id = $1 AND r.resource_id = it.resource_id
		`, id)
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// ── GET /api/v1/efficiency ────────────────────────────────────────────────────
func (s *Server) handleEfficiencyScore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := s.db.Query(ctx, `
		SELECT score_date, utilization_eff, allocation_cov, commitment_util,
		       hygiene_score, composite_score, score_tier
		FROM efficiency_scores
		ORDER BY score_date DESC
		LIMIT 30
	`)
	if err != nil { writeError(w, http.StatusInternalServerError, err.Error()); return }
	defer rows.Close()

	var history []map[string]any
	for rows.Next() {
		var scoreDate time.Time
		var util, alloc, commit, hygiene, composite float64
		var tier string
		rows.Scan(&scoreDate, &util, &alloc, &commit, &hygiene, &composite, &tier)
		history = append(history, map[string]any{
			"date": scoreDate, "utilization": util, "allocation": alloc,
			"commitment": commit, "hygiene": hygiene,
			"composite": composite, "tier": tier,
		})
	}

	var latest map[string]any
	if len(history) > 0 {
		latest = history[0]
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"latest":  latest,
		"history": history,
		"weights": map[string]any{
			"utilization": 0.35, "allocation": 0.25,
			"commitment":  0.20, "hygiene":    0.20,
		},
		"tiers": map[string]any{
			"Elite": "90-100", "Good": "70-89", "Fair": "50-69", "Poor": "0-49",
		},
	})
}

// ── Health check ──────────────────────────────────────────────────────────────
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok",
		"time":   time.Now().UTC(),
	})
}

// ── Router ────────────────────────────────────────────────────────────────────
func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Content-Type", "Authorization"},
		AllowCredentials: false,
	}))

	r.Get("/health", s.handleHealth)

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/overview",             s.handleOverview)
		r.Get("/costs/daily",          s.handleDailyCosts)
		r.Get("/costs/services",       s.handleServiceBreakdown)
		r.Get("/costs/movers",         s.handleCostMovers)
		r.Get("/costs/forecast",       s.handleForecasts)
		r.Get("/kubernetes/costmap",   s.handleK8sCostMap)
		r.Get("/kubernetes/pods",      s.handleTopPods)
		r.Get("/kubernetes/egress",    s.handleEgressFlows)
		r.Get("/anomalies",            s.handleAnomalies)
		r.Patch("/anomalies/{id}",     s.handleUpdateAnomaly)
		r.Get("/recommendations",      s.handleRecommendations)
		r.Patch("/recommendations/{id}", s.handleUpdateRecommendation)
		r.Get("/resources",            s.handleResources)
		// Gap fixes
		r.Get("/tags/coverage",          s.handleTagCoverage)
		r.Get("/commitments",            s.handleCommitmentAnalysis)
		r.Get("/kubernetes/vpa",         s.handleVPAComparison)
		r.Get("/tenants",                s.handleTenants)
		// Phase 3 additions
		r.Get("/costs/estimate",             s.handleCostEstimate)
		r.Post("/recommendations/{id}/open-pr", s.handleOpenPR)
		r.Get("/tags/inferences",            s.handleListInferences)
		r.Patch("/tags/inferences/{id}",     s.handleUpdateInference)
		r.Get("/efficiency",                 s.handleEfficiencyScore)
	})

	return r
}

// ── Main ──────────────────────────────────────────────────────────────────────
func main() {
	ctx := context.Background()

	dbDSN := os.Getenv("DB_DSN")
	if dbDSN == "" {
		dbDSN = "postgres://oe_user:oe_pass@localhost:5432/observability?sslmode=disable"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	var db *pgxpool.Pool
	var err error
	for i := 0; i < 20; i++ {
		db, err = pgxpool.New(ctx, dbDSN)
		if err == nil {
			if pingErr := db.Ping(ctx); pingErr == nil {
				break
			}
		}
		log.Printf("Waiting for database... attempt %d/20", i+1)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer db.Close()

	redisAddr := os.Getenv("REDIS_URL")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})

	srv := &Server{db: db, rdb: rdb}

	log.Printf("API server listening on :%s", port)

	addr := ":" + port
	_ = strconv.Atoi // keep import
	if err := http.ListenAndServe(addr, srv.routes()); err != nil {
		log.Fatal(err)
	}
}