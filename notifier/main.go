package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/robfig/cron/v3"
)

// ── Config ────────────────────────────────────────────────────────────────────

var (
	dbDSN          = envOrDie("DB_DSN")
	redisURL       = envOrDie("REDIS_URL")
	slackWebhook   = envOrDie("SLACK_WEBHOOK_URL")
	slackChannel   = envOr("SLACK_CHANNEL", "#cloud-costs")
	slackSecret    = envOr("SLACK_SIGNING_SECRET", "")
	oeAPIURL       = envOr("OE_API_URL", "http://api:8080")
	digestCron     = envOr("DIGEST_CRON", "0 9 * * 1") // Monday 09:00
	pollInterval   = envDuration("POLL_INTERVAL", 60*time.Second)
	tagPollInterval = envDuration("TAG_POLL_INTERVAL", 5*time.Minute)
)

func envOrDie(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("FATAL: required env var %s is not set", key)
	}
	return v
}
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// ── Slack Block Kit helpers ───────────────────────────────────────────────────

type slackText struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

type slackBlock struct {
	Type      string     `json:"type"`
	Text      *slackText `json:"text,omitempty"`
	Accessory *slackElement `json:"accessory,omitempty"`
	Fields    []slackText `json:"fields,omitempty"`
	Elements  []slackElement `json:"elements,omitempty"`
}

type slackElement struct {
	Type     string     `json:"type"`
	Text     *slackText `json:"text,omitempty"`
	ActionID string     `json:"action_id,omitempty"`
	Value    string     `json:"value,omitempty"`
	URL      string     `json:"url,omitempty"`
	Style    string     `json:"style,omitempty"`
}

type slackPayload struct {
	Channel string       `json:"channel,omitempty"`
	Text    string       `json:"text"` // fallback for notifications
	Blocks  []slackBlock `json:"blocks,omitempty"`
}

func postToSlack(payload slackPayload) error {
	payload.Channel = slackChannel
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	resp, err := http.Post(slackWebhook, "application/json", bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("slack returned %d: %s", resp.StatusCode, body)
	}
	return nil
}

func dividerBlock() slackBlock { return slackBlock{Type: "divider"} }

func headerBlock(text string) slackBlock {
	return slackBlock{
		Type: "header",
		Text: &slackText{Type: "plain_text", Text: text, Emoji: true},
	}
}

func sectionBlock(mrkdwn string) slackBlock {
	return slackBlock{
		Type: "section",
		Text: &slackText{Type: "mrkdwn", Text: mrkdwn},
	}
}

func actionsBlock(elements ...slackElement) slackBlock {
	return slackBlock{Type: "actions", Elements: elements}
}

func buttonElement(text, actionID, value, style string) slackElement {
	el := slackElement{
		Type:     "button",
		ActionID: actionID,
		Value:    value,
		Text:     &slackText{Type: "plain_text", Text: text, Emoji: true},
	}
	if style != "" {
		el.Style = style
	}
	return el
}

func linkButtonElement(text, actionID, linkURL string) slackElement {
	return slackElement{
		Type:     "button",
		ActionID: actionID,
		URL:      linkURL,
		Text:     &slackText{Type: "plain_text", Text: text, Emoji: true},
	}
}

// ── Notifier ─────────────────────────────────────────────────────────────────

type Notifier struct {
	db  *pgxpool.Pool
	rdb *redis.Client
}

func NewNotifier(db *pgxpool.Pool, rdb *redis.Client) *Notifier {
	return &Notifier{db: db, rdb: rdb}
}

// dedupKey returns true if we should skip this alert (already sent recently).
func (n *Notifier) shouldSkip(ctx context.Context, key string, ttl time.Duration) bool {
	set, err := n.rdb.SetNX(ctx, key, "1", ttl).Result()
	if err != nil {
		log.Printf("[notifier] redis SetNX error for %s: %v (will send anyway)", key, err)
		return false
	}
	return !set // set=false means key already existed → skip
}

// ── 4.1.1 Anomaly alert ───────────────────────────────────────────────────────

type anomalyRow struct {
	ID            string
	ServiceName   string
	MetricName    string
	CurrentValue  float64
	BaselineValue float64
	Deviation     float64
	ZScore        float64
	Severity      string
	ImpactUSD     float64
	Description   string
	IacPatch      string
}

func (n *Notifier) PollAnomalies(ctx context.Context) {
	rows, err := n.db.Query(ctx, `
		SELECT
			id::text,
			COALESCE(service_name, 'unknown'),
			COALESCE(metric_name, ''),
			COALESCE(current_value, 0),
			COALESCE(baseline_value, 0),
			COALESCE(deviation_pct, 0),
			COALESCE(zscore, 0),
			COALESCE(severity, 'medium'),
			COALESCE(impact_usd, 0),
			COALESCE(description, ''),
			COALESCE((SELECT iac_patch FROM recommendations r
			           WHERE r.anomaly_id = anomalies.id
			           AND r.iac_patch IS NOT NULL
			           LIMIT 1), '')
		FROM anomalies
		WHERE status = 'open'
		  AND notified_at IS NULL
		ORDER BY impact_usd DESC
		LIMIT 20
	`)
	if err != nil {
		log.Printf("[notifier] anomaly query error: %v", err)
		return
	}
	defer rows.Close()

	var sent int
	for rows.Next() {
		var a anomalyRow
		if err := rows.Scan(
			&a.ID, &a.ServiceName, &a.MetricName,
			&a.CurrentValue, &a.BaselineValue, &a.Deviation,
			&a.ZScore, &a.Severity, &a.ImpactUSD, &a.Description, &a.IacPatch,
		); err != nil {
			log.Printf("[notifier] scan error: %v", err)
			continue
		}

		dedupKey := fmt.Sprintf("alert:anomaly:%s", a.ID)
		if n.shouldSkip(ctx, dedupKey, 72*time.Hour) {
			log.Printf("[notifier] skipping anomaly %s (dedup)", a.ID)
			continue
		}

		if err := n.sendAnomalyAlert(ctx, a); err != nil {
			log.Printf("[notifier] send anomaly %s failed: %v", a.ID, err)
			// Release the dedup key so we retry next cycle
			n.rdb.Del(ctx, dedupKey)
			continue
		}

		// Mark notified
		n.db.Exec(ctx, `UPDATE anomalies SET notified_at=NOW() WHERE id=$1`, a.ID)
		sent++
	}
	if sent > 0 {
		log.Printf("[notifier] sent %d anomaly alert(s)", sent)
	}
}

func (n *Notifier) sendAnomalyAlert(ctx context.Context, a anomalyRow) error {
	severityEmoji := map[string]string{
		"critical": "🚨",
		"high":     "🔴",
		"medium":   "🟡",
		"low":      "🟢",
	}
	emoji := severityEmoji[a.Severity]
	if emoji == "" {
		emoji = "⚠️"
	}

	headerText := fmt.Sprintf("%s Cost Anomaly: %s", emoji, a.ServiceName)

	body := fmt.Sprintf(
		"*Service:* %s\n*Deviation:* +%.0f%% (z-score: %.1f)\n*Estimated impact:* $%.2f/month\n\n_%s_",
		a.ServiceName, a.Deviation, a.ZScore, a.ImpactUSD, a.Description,
	)

	blocks := []slackBlock{
		headerBlock(headerText),
		sectionBlock(body),
	}

	// Collapsible IaC patch (Slack doesn't support true collapse, so we show a
	// truncated excerpt and link to dashboard for full view)
	if a.IacPatch != "" {
		patch := a.IacPatch
		if len(patch) > 300 {
			patch = patch[:300] + "\n…"
		}
		blocks = append(blocks, sectionBlock(fmt.Sprintf("*Suggested IaC patch:*\n```%s```", patch)))
	}

	dashboardURL := fmt.Sprintf("%s/anomalies?id=%s", oeAPIURL, a.ID)
	actions := actionsBlock(
		linkButtonElement("🔍 View Dashboard", "view_dashboard_"+a.ID, dashboardURL),
		buttonElement("🔧 Open PR", "open_pr_"+a.ID, a.ID, "primary"),
		buttonElement("💤 Snooze 24h", "snooze_"+a.ID, a.ID, ""),
	)
	blocks = append(blocks, dividerBlock(), actions)

	return postToSlack(slackPayload{
		Text:   fmt.Sprintf("%s Cost anomaly detected: %s (+%.0f%%, $%.2f/mo impact)", emoji, a.ServiceName, a.Deviation, a.ImpactUSD),
		Blocks: blocks,
	})
}

// ── 4.1.2 Tag ownership prompt ────────────────────────────────────────────────

type inferredTagRow struct {
	ID            string
	ResourceID    string
	ResourceName  string
	Provider      string
	TagKey        string
	TagValue      string
	ConfidencePct float64
	RuleName      string
	MonthlyCost   float64
	DaysIdle      int
}

func (n *Notifier) PollTagPrompts(ctx context.Context) {
	rows, err := n.db.Query(ctx, `
		SELECT
			it.id::text,
			it.resource_id,
			COALESCE((SELECT resource_name FROM resources WHERE resource_id = it.resource_id LIMIT 1), it.resource_id),
			it.provider,
			it.tag_key,
			it.tag_value,
			it.confidence,
			COALESCE(it.signal_type, ''),
			COALESCE((SELECT cost_30d FROM resources WHERE resource_id = it.resource_id LIMIT 1), 0),
			COALESCE(EXTRACT(DAY FROM NOW() - (SELECT last_active_at FROM resources WHERE resource_id = it.resource_id LIMIT 1))::int, 0)
		FROM inferred_tags it
		WHERE it.accepted IS NULL
		  AND it.confidence > 80
		  AND it.notified_at IS NULL
		ORDER BY it.confidence DESC
		LIMIT 10
	`)
	if err != nil {
		log.Printf("[notifier] tag query error: %v", err)
		return
	}
	defer rows.Close()

	var sent int
	for rows.Next() {
		var t inferredTagRow
		if err := rows.Scan(
			&t.ID, &t.ResourceID, &t.ResourceName, &t.Provider,
			&t.TagKey, &t.TagValue, &t.ConfidencePct, &t.RuleName,
			&t.MonthlyCost, &t.DaysIdle,
		); err != nil {
			log.Printf("[notifier] tag scan error: %v", err)
			continue
		}

		dedupKey := fmt.Sprintf("alert:tag:%s", t.ID)
		if n.shouldSkip(ctx, dedupKey, 48*time.Hour) {
			continue
		}

		if err := n.sendTagPrompt(ctx, t); err != nil {
			log.Printf("[notifier] send tag prompt %s failed: %v", t.ID, err)
			n.rdb.Del(ctx, dedupKey)
			continue
		}

		n.db.Exec(ctx, `UPDATE inferred_tags SET notified_at=NOW() WHERE id=$1`, t.ID)
		sent++
	}
	if sent > 0 {
		log.Printf("[notifier] sent %d tag prompt(s)", sent)
	}
}

func (n *Notifier) sendTagPrompt(ctx context.Context, t inferredTagRow) error {
	body := fmt.Sprintf(
		"*Resource:* `%s` (%s)\n*Inferred tag:* `%s = %s`\n*Confidence:* %.0f%% (rule: %s)\n*Monthly cost:* $%.2f%s",
		t.ResourceName, t.Provider,
		t.TagKey, t.TagValue,
		t.ConfidencePct, t.RuleName,
		t.MonthlyCost,
		func() string {
			if t.DaysIdle > 0 {
				return fmt.Sprintf(" · %d days idle", t.DaysIdle)
			}
			return ""
		}(),
	)

	blocks := []slackBlock{
		headerBlock("🏷️ Tag Ownership Check"),
		sectionBlock("We inferred a tag for an untagged resource. Is this correct?"),
		sectionBlock(body),
		dividerBlock(),
		actionsBlock(
			buttonElement("✅ Yes, apply it", "tag_yes_"+t.ID, t.ID, "primary"),
			buttonElement("❌ No, it's mine", "tag_no_mine_"+t.ID, t.ID, "danger"),
			buttonElement("🤷 Don't know", "tag_unknown_"+t.ID, t.ID, ""),
		),
	}

	return postToSlack(slackPayload{
		Text:   fmt.Sprintf("🏷️ Tag ownership check: is `%s` owned by team `%s`? (%.0f%% confidence)", t.ResourceName, t.TagValue, t.ConfidencePct),
		Blocks: blocks,
	})
}

// ── 4.1.3 Weekly digest ───────────────────────────────────────────────────────

type costMover struct {
	ServiceName  string
	Current7d    float64
	Previous7d   float64
	DeltaPct     float64
}

type topRec struct {
	Title          string
	MonthlySavings float64
	RecType        string
}

func (n *Notifier) SendWeeklyDigest(ctx context.Context) {
	log.Println("[notifier] sending weekly digest")

	// Efficiency score
	var composite, utilEff, allocCov, commitUtil, hygiene float64
	var scoreTier string
	n.db.QueryRow(ctx, `
		SELECT composite_score, utilization_eff, allocation_cov,
		       commitment_util, hygiene_score, score_tier
		FROM efficiency_scores
		ORDER BY score_date DESC LIMIT 1
	`).Scan(&composite, &utilEff, &allocCov, &commitUtil, &hygiene, &scoreTier)

	var prevComposite float64
	n.db.QueryRow(ctx, `
		SELECT composite_score FROM efficiency_scores
		ORDER BY score_date DESC LIMIT 1 OFFSET 1
	`).Scan(&prevComposite)

	scoreDelta := composite - prevComposite
	scoreDeltaStr := fmt.Sprintf("+%.1f", scoreDelta)
	if scoreDelta < 0 {
		scoreDeltaStr = fmt.Sprintf("%.1f", scoreDelta)
	}

	// Top 3 cost movers
	moverRows, _ := n.db.Query(ctx, `
		SELECT service_name,
		       cost_7d,
		       cost_prior_7d,
		       COALESCE(change_pct, 0) as change_pct
		FROM v_cost_movers
		LIMIT 3
	`)
	var movers []costMover
	if moverRows != nil {
		defer moverRows.Close()
		for moverRows.Next() {
			var m costMover
			moverRows.Scan(&m.ServiceName, &m.Current7d, &m.Previous7d, &m.DeltaPct)
			movers = append(movers, m)
		}
	}

	// Top 3 open recommendations
	recRows, _ := n.db.Query(ctx, `
		SELECT title, COALESCE(monthly_savings,0), rec_type
		FROM recommendations
		WHERE status = 'open'
		ORDER BY monthly_savings DESC
		LIMIT 3
	`)
	var recs []topRec
	if recRows != nil {
		defer recRows.Close()
		for recRows.Next() {
			var r topRec
			recRows.Scan(&r.Title, &r.MonthlySavings, &r.RecType)
			recs = append(recs, r)
		}
	}

	// Count recommendations applied in last 7 days
	var appliedCount int
	n.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM recommendations
		WHERE status = 'applied' AND updated_at >= NOW() - INTERVAL '7 days'
	`).Scan(&appliedCount)

	// Build blocks
	blocks := []slackBlock{
		headerBlock("☁️ Weekly Cloud Cost Digest"),
		sectionBlock(fmt.Sprintf("*Week ending %s*", time.Now().Format("Jan 2, 2006"))),
		dividerBlock(),
	}

	// Efficiency score card
	scoreEmoji := "🟢"
	if composite < 50 {
		scoreEmoji = "🔴"
	} else if composite < 70 {
		scoreEmoji = "🟡"
	}
	blocks = append(blocks, sectionBlock(fmt.Sprintf(
		"%s *Efficiency Score: %.0f / 100* (%s tier) · %s vs last week\n"+
			"Utilization: %.0f  ·  Coverage: %.0f  ·  Commitment: %.0f  ·  Hygiene: %.0f",
		scoreEmoji, composite, scoreTier, scoreDeltaStr,
		utilEff, allocCov, commitUtil, hygiene,
	)))

	// Cost movers
	if len(movers) > 0 {
		blocks = append(blocks, dividerBlock(), sectionBlock("*📈 Top Cost Movers (7d)*"))
		for _, m := range movers {
			arrow := "↑"
			if m.DeltaPct < 0 {
				arrow = "↓"
			}
			blocks = append(blocks, sectionBlock(fmt.Sprintf(
				"• *%s* %s%.0f%% — $%.0f → $%.0f/wk",
				m.ServiceName, arrow, m.DeltaPct, m.Previous7d, m.Current7d,
			)))
		}
	}

	// Top recommendations
	if len(recs) > 0 {
		blocks = append(blocks, dividerBlock(), sectionBlock("*💡 Top Savings Opportunities*"))
		totalSavings := 0.0
		for _, r := range recs {
			totalSavings += r.MonthlySavings
			blocks = append(blocks, sectionBlock(fmt.Sprintf(
				"• *%s* — save $%.0f/mo", r.Title, r.MonthlySavings,
			)))
		}
		blocks = append(blocks, sectionBlock(fmt.Sprintf(
			"_Total potential savings: $%.0f/month_", totalSavings,
		)))
	}

	// Applied this week
	blocks = append(blocks, dividerBlock(), sectionBlock(fmt.Sprintf(
		"✅ *%d recommendation(s) applied* in the past 7 days.", appliedCount,
	)))

	// Actions
	dashURL := fmt.Sprintf("%s/", oeAPIURL)
	blocks = append(blocks, dividerBlock(), actionsBlock(
		linkButtonElement("📊 Open Dashboard", "digest_dashboard", dashURL),
	))

	err := postToSlack(slackPayload{
		Text:   fmt.Sprintf("☁️ Weekly Cloud Cost Digest — Efficiency Score: %.0f/100 (%s)", composite, scoreTier),
		Blocks: blocks,
	})
	if err != nil {
		log.Printf("[notifier] weekly digest failed: %v", err)
	} else {
		log.Println("[notifier] weekly digest sent")
	}
}

// ── Interaction endpoint (port 3001) ─────────────────────────────────────────

// verifySlackSignature validates the X-Slack-Signature header using HMAC-SHA256.
func verifySlackSignature(r *http.Request, body []byte) bool {
	if slackSecret == "" {
		log.Println("[notifier] WARN: SLACK_SIGNING_SECRET not set, skipping signature verification")
		return true
	}
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if ts == "" || sig == "" {
		return false
	}
	// Replay protection: reject requests older than 5 minutes
	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Now().Unix()-tsInt > 300 {
		return false
	}
	baseString := fmt.Sprintf("v0:%s:%s", ts, string(body))
	mac := hmac.New(sha256.New, []byte(slackSecret))
	mac.Write([]byte(baseString))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(sig))
}

type slackInteractionPayload struct {
	Type    string `json:"type"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

func (n *Notifier) handleInteraction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if !verifySlackSignature(r, body) {
		log.Println("[notifier] invalid Slack signature")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Slack sends form-encoded payload=<url-encoded-json>
	decoded, err := url.QueryUnescape(strings.TrimPrefix(string(body), "payload="))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var payload slackInteractionPayload
	if err := json.Unmarshal([]byte(decoded), &payload); err != nil {
		log.Printf("[notifier] unmarshal interaction: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	for _, action := range payload.Actions {
		id := action.Value
		switch {
		case strings.HasPrefix(action.ActionID, "snooze_"):
			// Snooze: set notified_at to NOW() + 24h so the poll skips it
			n.db.Exec(ctx,
				`UPDATE anomalies SET notified_at = NOW() + INTERVAL '24 hours' WHERE id = $1`, id)
			n.rdb.Del(ctx, fmt.Sprintf("alert:anomaly:%s", id))
			log.Printf("[notifier] snoozed anomaly %s by %s", id, payload.User.Username)

		case strings.HasPrefix(action.ActionID, "open_pr_"):
			// Trigger PR creation via API service
			// The value here is an anomaly ID — find the linked open recommendation
			go func(anomalyID string) {
				apiCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				// Find the best open recommendation (highest savings) to create a PR for
				var recID string
				n.db.QueryRow(apiCtx,
					`SELECT id::text FROM recommendations
					 WHERE status='open' AND iac_patch IS NOT NULL
					 ORDER BY monthly_savings DESC NULLS LAST LIMIT 1`,
				).Scan(&recID)
				if recID == "" {
					log.Printf("[notifier] no recommendation with iac_patch found for anomaly %s", anomalyID)
					return
				}
				prURL := fmt.Sprintf("%s/api/v1/recommendations/%s/open-pr", oeAPIURL, recID)
				resp, err := http.Post(prURL, "application/json", nil)
				if err != nil {
					log.Printf("[notifier] open-pr call failed: %v", err)
					return
				}
				defer resp.Body.Close()
				log.Printf("[notifier] open-pr for recommendation %s: HTTP %d", recID, resp.StatusCode)
			}(id)

		case strings.HasPrefix(action.ActionID, "tag_yes_"):
			// Accept tag inference
			n.db.Exec(ctx,
				`UPDATE inferred_tags SET accepted = TRUE, accepted_by = $1, updated_at = NOW() WHERE id = $2`,
				payload.User.Username, id)
			log.Printf("[notifier] tag inference %s accepted by %s", id, payload.User.Username)

		case strings.HasPrefix(action.ActionID, "tag_no_mine_"):
			// Reject inference, prompt for confirmation in a thread reply
			n.db.Exec(ctx,
				`UPDATE inferred_tags SET accepted = FALSE, accepted_by = $1, updated_at = NOW() WHERE id = $2`,
				payload.User.Username, id)
			log.Printf("[notifier] tag inference %s rejected by %s", id, payload.User.Username)

		case strings.HasPrefix(action.ActionID, "tag_unknown_"):
			// Leave accepted=NULL but mark notified so we don't re-prompt for 48h
			log.Printf("[notifier] tag inference %s marked unknown by %s", id, payload.User.Username)

		default:
			log.Printf("[notifier] unknown action_id: %s", action.ActionID)
		}
	}

	// Respond with 200 immediately — Slack requires a response within 3 seconds
	w.WriteHeader(http.StatusOK)
}

func (n *Notifier) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var one int
	err := n.db.QueryRow(ctx, "SELECT 1").Scan(&one)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"unhealthy","db":"%v"}`, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","time":"%s"}`, time.Now().Format(time.RFC3339))
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[notifier] ")

	ctx := context.Background()

	// ── DB connection with retry ──────────────────────────────────────────────
	var db *pgxpool.Pool
	var err error
	for attempt := 1; attempt <= 15; attempt++ {
		db, err = pgxpool.New(ctx, dbDSN)
		if err == nil {
			if err = db.Ping(ctx); err == nil {
				break
			}
		}
		log.Printf("db connect attempt %d/15 failed: %v — retrying in %ds", attempt, err, attempt*2)
		time.Sleep(time.Duration(attempt*2) * time.Second)
	}
	if err != nil {
		log.Fatalf("could not connect to database after 15 attempts: %v", err)
	}
	defer db.Close()
	log.Println("database connected")

	// ── Redis connection ──────────────────────────────────────────────────────
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("invalid REDIS_URL: %v", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("redis connect failed: %v", err)
	}
	defer rdb.Close()
	log.Println("redis connected")

	n := NewNotifier(db, rdb)

	// ── Poll loop: anomaly alerts ─────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		log.Printf("anomaly poll loop started (interval: %s)", pollInterval)
		n.PollAnomalies(ctx) // run immediately on start
		for {
			select {
			case <-ticker.C:
				n.PollAnomalies(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// ── Poll loop: tag ownership prompts ─────────────────────────────────────
	go func() {
		ticker := time.NewTicker(tagPollInterval)
		defer ticker.Stop()
		log.Printf("tag prompt poll loop started (interval: %s)", tagPollInterval)
		n.PollTagPrompts(ctx) // run immediately on start
		for {
			select {
			case <-ticker.C:
				n.PollTagPrompts(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()

	// ── Weekly digest cron ────────────────────────────────────────────────────
	c := cron.New()
	_, err = c.AddFunc(digestCron, func() {
		n.SendWeeklyDigest(context.Background())
	})
	if err != nil {
		log.Fatalf("invalid DIGEST_CRON expression %q: %v", digestCron, err)
	}
	c.Start()
	defer c.Stop()
	log.Printf("weekly digest scheduled: %s", digestCron)

	// ── HTTP server: Slack interaction callbacks ──────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/slack/interactions", n.handleInteraction)
	mux.HandleFunc("/health", n.handleHealth)

	srv := &http.Server{
		Addr:         ":3001",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	log.Println("interaction endpoint listening on :3001")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("http server error: %v", err)
	}
}
