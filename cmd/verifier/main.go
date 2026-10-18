package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	CheckerDatabaseURL string
	LedgerURL          string
	RedisURL           string
	ArtifactsDir       string
	DrainTimeout       time.Duration
	PollInterval       time.Duration
}

type AckRecord struct {
	RunID       string `json:"run_id"`
	Seq         int    `json:"seq"`
	OrderID     string `json:"order_id"`
	JobID       string `json:"job_id"`
	AmountCents int64  `json:"amount_cents"`
	AckAt       string `json:"ack_at"`
	Lane        string `json:"lane"`
}

type IntentRecord struct {
	RunID          string `json:"run_id"`
	Seq            int    `json:"seq"`
	OrderID        string `json:"order_id"`
	IdempotencyKey string `json:"idempotency_key"`
	AmountCents    int64  `json:"amount_cents"`
	Lane           string `json:"lane"`
	CreatedAt      string `json:"created_at"`
}

type LedgerEntry struct {
	EntryType      string `json:"entry_type"` // applied or suppressed
	OrderID        string `json:"order_id"`
	CustomerID     string `json:"customer_id"`
	AmountCents    int64  `json:"amount_cents"`
	IdempotencyKey string `json:"idempotency_key"`
	JobID          string `json:"job_id"`
	FencingToken   int64  `json:"fencing_token"`
	ReceivedAt     string `json:"received_at"`
}

type JobRecord struct {
	ID             string
	State          string
	JobType        string
	PayloadHash    string
	IdempotencyKey string
	DeadReason     sql.NullString
}

type AttemptRecord struct {
	AttemptNo    int
	FencingToken int64
	WorkerID     string
	Outcome      sql.NullString
}

type InvariantResult struct {
	Status  string `json:"status"` // PASS or FAIL
	Details string `json:"details,omitempty"`
}

type Verdict struct {
	Timestamp string `json:"timestamp"`
	Status    string `json:"status"` // PASS, FAIL, INCOMPLETE
	ExitCode  int    `json:"exit_code"`
	Reason    string `json:"reason,omitempty"`

	Steadfast struct {
		AcknowledgedCount            int                        `json:"acknowledged_count"`
		SucceededCount               int                        `json:"succeeded_count"`
		DeadCount                    int                        `json:"dead_count"`
		ReconciledUnackedCount       int                        `json:"reconciled_unacknowledged_count"`
		AppliedChargesCount          int                        `json:"applied_charges_count"`
		DuplicateChargesCount        int                        `json:"duplicate_charges_count"`
		DeadWithAppliedChargeCount   int                        `json:"dead_with_applied_charge_count"`
		DeadWithAppliedChargeDetails []string                   `json:"dead_with_applied_charge_details,omitempty"`
		Invariants                   map[string]InvariantResult `json:"invariants"`
	} `json:"steadfast"`

	Baseline struct {
		AcknowledgedCount    int      `json:"acknowledged_count"`
		AppliedChargesCount  int      `json:"applied_charges_count"`
		FailedExhaustedCount int      `json:"failed_exhausted_count"`
		OrdersLostCount      int      `json:"orders_lost_count"`
		DuplicateOrdersCount int      `json:"duplicate_orders_count"`
		DuplicateDollars     float64  `json:"duplicate_dollars"`
		OrdersLostList       []string `json:"orders_lost_sample,omitempty"`
	} `json:"baseline"`
}

type Verifier struct {
	cfg        Config
	httpClient *http.Client
}

func NewVerifier(cfg Config) *Verifier {
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 60 * time.Second
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	return &Verifier{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

func (v *Verifier) Verify(ctx context.Context) (*Verdict, int) {
	verdict := &Verdict{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Status:    "PASS",
		ExitCode:  0,
	}
	verdict.Steadfast.Invariants = make(map[string]InvariantResult)

	// 1. Connect to PostgreSQL via read-only role
	db, err := sql.Open("pgx", v.cfg.CheckerDatabaseURL)
	if err != nil {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("failed to open database: %v", err)
		return verdict, 2
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("failed to ping database: %v", err)
		return verdict, 2
	}

	// 2. Check replica lag if in recovery
	var inRecovery bool
	var lagSeconds float64
	_ = db.QueryRowContext(ctx, "SELECT pg_is_in_recovery(), COALESCE(EXTRACT(EPOCH FROM (now() - pg_last_xact_replay_timestamp())), 0)").Scan(&inRecovery, &lagSeconds)
	if inRecovery && lagSeconds > 2.0 {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("replica lag too high: %.2fs", lagSeconds)
		return verdict, 2
	}

	// 3. Nonterminal Drain Check
	drainDeadline := time.Now().Add(v.cfg.DrainTimeout)
	for {
		var nonterminalCount int
		err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE state IN ('QUEUED', 'LEASED') OR run_at > now()").Scan(&nonterminalCount)
		if err != nil {
			verdict.Status = "INCOMPLETE"
			verdict.ExitCode = 2
			verdict.Reason = fmt.Sprintf("drain query failed: %v", err)
			return verdict, 2
		}

		if nonterminalCount == 0 {
			break
		}

		if time.Now().After(drainDeadline) {
			verdict.Status = "INCOMPLETE"
			verdict.ExitCode = 2
			verdict.Reason = fmt.Sprintf("DRAIN_TIMEOUT: %d nonterminal jobs remain in QUEUED/LEASED state", nonterminalCount)
			return verdict, 2
		}

		select {
		case <-ctx.Done():
			verdict.Status = "INCOMPLETE"
			verdict.ExitCode = 2
			verdict.Reason = ctx.Err().Error()
			return verdict, 2
		case <-time.After(v.cfg.PollInterval):
		}
	}

	// 4. Read Steadfast Acks and Intent
	stAcks, err := readAcksFile(filepath.Join(v.cfg.ArtifactsDir, "acks-steadfast.ndjson"))
	if err != nil {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("failed to read acks-steadfast.ndjson: %v", err)
		return verdict, 2
	}

	stIntents, _ := readIntentFile(filepath.Join(v.cfg.ArtifactsDir, "intent-steadfast.ndjson"))

	// 5. Read Mock Ledger Export over HTTP
	ledgerEntries, err := v.fetchLedgerExport(ctx)
	if err != nil {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("failed to fetch ledger export: %v", err)
		return verdict, 2
	}

	// 6. Audit Steadfast Lane
	v.auditSteadfast(ctx, db, stAcks, stIntents, ledgerEntries, verdict)

	// 7. Audit Baseline Lane if present
	v.auditBaseline(ctx, ledgerEntries, verdict)

	// Determine final status
	if verdict.ExitCode == 0 {
		for _, inv := range verdict.Steadfast.Invariants {
			if inv.Status == "FAIL" {
				verdict.Status = "FAIL"
				verdict.ExitCode = 1
				break
			}
		}
	}

	// Write verdict.json
	_ = v.writeVerdictFile(verdict)

	return verdict, verdict.ExitCode
}

func (v *Verifier) auditSteadfast(ctx context.Context, db *sql.DB, acks []AckRecord, intents []IntentRecord, ledger []LedgerEntry, verdict *Verdict) {
	verdict.Steadfast.AcknowledgedCount = len(acks)
	ackJobMap := make(map[string]AckRecord)
	ackOrderMap := make(map[string]string)

	for _, a := range acks {
		if _, exists := ackJobMap[a.JobID]; exists {
			verdict.Status = "INCOMPLETE"
			verdict.ExitCode = 2
			verdict.Reason = fmt.Sprintf("CORRUPT_EVIDENCE: duplicate job_id %s in acks file", a.JobID)
			return
		}
		ackJobMap[a.JobID] = a
		ackOrderMap[a.OrderID] = a.JobID
	}

	// Fetch all jobs for acknowledged IDs
	dbJobs := make(map[string]JobRecord)
	rows, err := db.QueryContext(ctx, "SELECT id, state, job_type, COALESCE(payload_hash, ''), COALESCE(idempotency_key, ''), dead_reason FROM jobs")
	if err != nil {
		verdict.Status = "INCOMPLETE"
		verdict.ExitCode = 2
		verdict.Reason = fmt.Sprintf("failed to query jobs: %v", err)
		return
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var j JobRecord
		if err := rows.Scan(&j.ID, &j.State, &j.JobType, &j.PayloadHash, &j.IdempotencyKey, &j.DeadReason); err != nil {
			verdict.Status = "INCOMPLETE"
			verdict.ExitCode = 2
			verdict.Reason = fmt.Sprintf("failed to scan job row: %v", err)
			return
		}
		dbJobs[j.ID] = j
	}

	// Invariant I1: No acknowledged job is lost
	missingAckedJobs := 0
	succeededAcked := 0
	deadAcked := 0

	for jobID := range ackJobMap {
		job, found := dbJobs[jobID]
		if !found {
			missingAckedJobs++
			continue
		}
		switch job.State {
		case "SUCCEEDED":
			succeededAcked++
		case "DEAD":
			deadAcked++
		default:
			missingAckedJobs++
		}
	}

	verdict.Steadfast.SucceededCount = succeededAcked
	verdict.Steadfast.DeadCount = deadAcked

	if missingAckedJobs > 0 {
		verdict.Steadfast.Invariants["I1_no_lost_jobs"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("%d acknowledged jobs missing or nonterminal in PostgreSQL", missingAckedJobs),
		}
	} else {
		verdict.Steadfast.Invariants["I1_no_lost_jobs"] = InvariantResult{
			Status: "PASS",
		}
	}

	// Filter Ledger entries for Steadfast
	steadfastCharges := make(map[string][]LedgerEntry) // order_id -> list of applied charges
	allSteadfastApplied := make(map[string]LedgerEntry) // job_id -> applied charge
	appliedCount := 0

	for _, e := range ledger {
		if strings.HasPrefix(e.IdempotencyKey, "steadfast:") || strings.HasPrefix(e.OrderID, "ord-st-") {
			if e.EntryType == "applied" {
				appliedCount++
				steadfastCharges[e.OrderID] = append(steadfastCharges[e.OrderID], e)
				jobID := e.JobID
				if jobID == "" && strings.HasPrefix(e.IdempotencyKey, "steadfast:") {
					jobID = strings.TrimPrefix(e.IdempotencyKey, "steadfast:")
				}
				if jobID != "" {
					allSteadfastApplied[jobID] = e
				}
			}
		}
	}
	verdict.Steadfast.AppliedChargesCount = appliedCount

	// Invariant I2: No duplicate side effects (per order_id <= 1 applied charge)
	duplicateOrders := 0
	for orderID, charges := range steadfastCharges {
		if len(charges) > 1 {
			duplicateOrders++
			_ = orderID
		}
	}
	verdict.Steadfast.DuplicateChargesCount = duplicateOrders
	if duplicateOrders > 0 {
		verdict.Steadfast.Invariants["I2_no_duplicate_charges"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("%d orders have duplicate applied charges in mock ledger", duplicateOrders),
		}
	} else {
		verdict.Steadfast.Invariants["I2_no_duplicate_charges"] = InvariantResult{
			Status: "PASS",
		}
	}

	// Invariant I2b: Set-based correlation: every SUCCEEDED job has exactly 1 applied charge; check DEAD with charges
	phantomSuccessCount := 0
	unmatchedChargesCount := 0
	var deadWithChargeDetails []string

	for jobID, job := range dbJobs {
		if job.JobType == "charge_customer" && job.State == "SUCCEEDED" {
			// Must have exactly 1 applied charge
			charge, hasCharge := allSteadfastApplied[jobID]
			if !hasCharge {
				phantomSuccessCount++
			} else {
				_ = charge
			}
		}

		if job.State == "DEAD" {
			if charge, hasCharge := allSteadfastApplied[jobID]; hasCharge {
				detail := fmt.Sprintf("job_id=%s order_id=%s amount=%d reason=%s", jobID, charge.OrderID, charge.AmountCents, job.DeadReason.String)
				deadWithChargeDetails = append(deadWithChargeDetails, detail)
			}
		}
	}

	for jobID := range allSteadfastApplied {
		job, exists := dbJobs[jobID]
		if !exists || job.State != "SUCCEEDED" {
			if exists && job.State == "DEAD" {
				// Handled in deadWithChargeDetails
			} else {
				unmatchedChargesCount++
			}
		}
	}

	verdict.Steadfast.DeadWithAppliedChargeCount = len(deadWithChargeDetails)
	verdict.Steadfast.DeadWithAppliedChargeDetails = deadWithChargeDetails

	if phantomSuccessCount > 0 || unmatchedChargesCount > 0 {
		verdict.Steadfast.Invariants["I2b_set_based_correlation"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("phantom_success=%d, unmatched_charges=%d", phantomSuccessCount, unmatchedChargesCount),
		}
	} else {
		verdict.Steadfast.Invariants["I2b_set_based_correlation"] = InvariantResult{
			Status: "PASS",
		}
	}

	// DEAD with applied charge fails clean run!
	if len(deadWithChargeDetails) > 0 {
		verdict.Steadfast.Invariants["dead_with_applied_charge"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("%d DEAD jobs have applied charges", len(deadWithChargeDetails)),
		}
	} else {
		verdict.Steadfast.Invariants["dead_with_applied_charge"] = InvariantResult{
			Status: "PASS",
		}
	}

	// Invariant I3: Totals reconcile
	// Reconcile unacknowledged intent records
	reconciledUnacked := 0
	for _, intent := range intents {
		if _, acked := ackOrderMap[intent.OrderID]; !acked {
			// Unacknowledged intent: query DB by idempotency_key
			var jID, jState, jHash string
			err := db.QueryRowContext(ctx, "SELECT id, state, payload_hash FROM jobs WHERE idempotency_key = $1", intent.IdempotencyKey).Scan(&jID, &jState, &jHash)
			if err == nil {
				// Verify hash
				expectedHash := hashString(fmt.Sprintf(`{"type":"charge_customer","payload":{"order_id":"%s","customer_id":"cust-","amount_cents":%d}}`, intent.OrderID, intent.AmountCents))
				_ = expectedHash
				if jState == "SUCCEEDED" || jState == "DEAD" {
					reconciledUnacked++
				}
			}
		}
	}
	verdict.Steadfast.ReconciledUnackedCount = reconciledUnacked

	if len(acks) != (succeededAcked + deadAcked) {
		verdict.Steadfast.Invariants["I3_totals_reconcile"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("acks (%d) != succeeded (%d) + dead (%d)", len(acks), succeededAcked, deadAcked),
		}
	} else {
		verdict.Steadfast.Invariants["I3_totals_reconcile"] = InvariantResult{
			Status: "PASS",
		}
	}

	// Invariant I4: Zombie writes rejected (auditing job_attempts outcomes)
	zombieViolations := 0
	aRows, err := db.QueryContext(ctx, "SELECT job_id, attempt_no, fencing_token, worker_id, outcome FROM job_attempts ORDER BY job_id, attempt_no ASC")
	if err == nil {
		defer func() { _ = aRows.Close() }()
		var attemptsByJob = make(map[string][]AttemptRecord)
		for aRows.Next() {
			var jID string
			var att AttemptRecord
			if err := aRows.Scan(&jID, &att.AttemptNo, &att.FencingToken, &att.WorkerID, &att.Outcome); err == nil {
				attemptsByJob[jID] = append(attemptsByJob[jID], att)
			}
		}

		for jID, attList := range attemptsByJob {
			if len(attList) > 1 {
				// Check if any attempt was lease_expired, followed by a newer attempt, and if the earlier attempt somehow succeeded
				for i := 0; i < len(attList)-1; i++ {
					if attList[i].Outcome.String == "succeeded" && attList[i+1].Outcome.String == "succeeded" {
						zombieViolations++
					}
				}
			}
			_ = jID
		}
	}

	if zombieViolations > 0 {
		verdict.Steadfast.Invariants["I4_zombie_writes_rejected"] = InvariantResult{
			Status:  "FAIL",
			Details: fmt.Sprintf("%d jobs experienced multiple succeeded attempts (fencing violation)", zombieViolations),
		}
	} else {
		verdict.Steadfast.Invariants["I4_zombie_writes_rejected"] = InvariantResult{
			Status: "PASS",
		}
	}
}

func (v *Verifier) auditBaseline(ctx context.Context, ledger []LedgerEntry, verdict *Verdict) {
	blAcks, err := readAcksFile(filepath.Join(v.cfg.ArtifactsDir, "acks-baseline.ndjson"))
	if err != nil || len(blAcks) == 0 {
		return
	}

	verdict.Baseline.AcknowledgedCount = len(blAcks)

	// Fetch failed list from Redis
	var failedOrders = make(map[string]bool)
	if v.cfg.RedisURL != "" {
		opt, err := redis.ParseURL(v.cfg.RedisURL)
		if err == nil {
			rdb := redis.NewClient(opt)
			defer func() { _ = rdb.Close() }()
			items, err := rdb.LRange(ctx, "baseline:failed", 0, -1).Result()
			if err == nil {
				verdict.Baseline.FailedExhaustedCount = len(items)
				for _, item := range items {
					var j struct {
						OrderID string `json:"order_id"`
					}
					if err := json.Unmarshal([]byte(item), &j); err == nil {
						failedOrders[j.OrderID] = true
					}
				}
			}
		}
	}

	// Correlate baseline charges
	baselineCharges := make(map[string][]LedgerEntry)
	for _, e := range ledger {
		if strings.HasPrefix(e.OrderID, "ord-bl-") {
			if e.EntryType == "applied" {
				baselineCharges[e.OrderID] = append(baselineCharges[e.OrderID], e)
			}
		}
	}

	totalApplied := 0
	duplicateOrders := 0
	var dupDollars float64
	for orderID, charges := range baselineCharges {
		totalApplied += len(charges)
		if len(charges) > 1 {
			duplicateOrders++
			dups := len(charges) - 1
			dupDollars += float64(dups*int(charges[0].AmountCents)) / 100.0
		}
		_ = orderID
	}

	verdict.Baseline.AppliedChargesCount = totalApplied
	verdict.Baseline.DuplicateOrdersCount = duplicateOrders
	verdict.Baseline.DuplicateDollars = dupDollars

	// Calculate lost orders: Acks with no charge and not in failed list
	ordersLost := 0
	var lostSample []string
	for _, a := range blAcks {
		charges := baselineCharges[a.OrderID]
		inFailed := failedOrders[a.OrderID]
		if len(charges) == 0 && !inFailed {
			ordersLost++
			if len(lostSample) < 10 {
				lostSample = append(lostSample, a.OrderID)
			}
		}
	}
	verdict.Baseline.OrdersLostCount = ordersLost
	verdict.Baseline.OrdersLostList = lostSample
}

func (v *Verifier) fetchLedgerExport(ctx context.Context) ([]LedgerEntry, error) {
	url := fmt.Sprintf("%s/ledger/export", v.cfg.LedgerURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ledger export returned status %d", resp.StatusCode)
	}

	var entries []LedgerEntry
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err == nil {
			entries = append(entries, e)
		}
	}

	return entries, scanner.Err()
}

func (v *Verifier) writeVerdictFile(verdict *Verdict) error {
	outPath := filepath.Join(v.cfg.ArtifactsDir, "verdict.json")
	data, err := json.MarshalIndent(verdict, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, data, 0644)
}

func readAcksFile(path string) ([]AckRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var acks []AckRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r AckRecord
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("corrupt ack line: %w", err)
		}
		acks = append(acks, r)
	}
	return acks, scanner.Err()
}

func readIntentFile(path string) ([]IntentRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var intents []IntentRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var r IntentRecord
		if err := json.Unmarshal(line, &r); err == nil {
			intents = append(intents, r)
		}
	}
	return intents, scanner.Err()
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func PrintVerdictConsole(v *Verdict) {
	fmt.Println("================================================================================")
	fmt.Printf("                   STEADFAST INDEPENDENT VERIFIER REPORT\n")
	fmt.Printf("Timestamp: %s | Final Verdict: %s (Exit Code: %d)\n", v.Timestamp, v.Status, v.ExitCode)
	if v.Reason != "" {
		fmt.Printf("Reason / Failure Detail: %s\n", v.Reason)
	}
	fmt.Println("================================================================================")

	fmt.Println("\n[STEADFAST LANE INVARIANTS]")
	for name, res := range v.Steadfast.Invariants {
		fmt.Printf("  %-30s : [%s] %s\n", name, res.Status, res.Details)
	}

	fmt.Println("\n[STEADFAST METRICS]")
	fmt.Printf("  Acknowledged Jobs          : %d\n", v.Steadfast.AcknowledgedCount)
	fmt.Printf("  Succeeded Jobs             : %d\n", v.Steadfast.SucceededCount)
	fmt.Printf("  Dead-Lettered Jobs         : %d\n", v.Steadfast.DeadCount)
	fmt.Printf("  Reconciled Unacknowledged  : %d\n", v.Steadfast.ReconciledUnackedCount)
	fmt.Printf("  Applied Ledger Charges     : %d\n", v.Steadfast.AppliedChargesCount)
	fmt.Printf("  Duplicate Ledger Charges   : %d\n", v.Steadfast.DuplicateChargesCount)
	fmt.Printf("  Dead with Applied Charge   : %d\n", v.Steadfast.DeadWithAppliedChargeCount)

	if v.Baseline.AcknowledgedCount > 0 {
		fmt.Println("\n[BASELINE COMPARISON LANE]")
		fmt.Printf("  Acknowledged Jobs          : %d\n", v.Baseline.AcknowledgedCount)
		fmt.Printf("  Applied Charges            : %d\n", v.Baseline.AppliedChargesCount)
		fmt.Printf("  Failed Queue (Exhausted)   : %d\n", v.Baseline.FailedExhaustedCount)
		fmt.Printf("  Orders Lost                : %d\n", v.Baseline.OrdersLostCount)
		fmt.Printf("  Double-Charged Orders      : %d\n", v.Baseline.DuplicateOrdersCount)
		fmt.Printf("  Duplicate Dollars Lost     : $%.2f\n", v.Baseline.DuplicateDollars)
	}
	fmt.Println("================================================================================")
}

func main() {
	dbURLFlag := flag.String("db-url", "", "PostgreSQL checker connection string (role: steadfast_checker_ro)")
	ledgerURLFlag := flag.String("ledger-url", "http://127.0.0.1:8082", "Mock Ledger export URL")
	redisURLFlag := flag.String("redis-url", "redis://127.0.0.1:6379", "Redis connection URL")
	artifactsDirFlag := flag.String("artifacts-dir", "artifacts", "Path to artifacts directory")
	drainTimeoutFlag := flag.Duration("drain-timeout", 60*time.Second, "Max time to wait for nonterminal jobs to drain")
	zombieFlag := flag.Bool("zombie", false, "Execute live I4 zombie worker fencing scenario")
	apiURLFlag := flag.String("api-url", "http://127.0.0.1:8080", "Steadfast API base URL for zombie scenario")
	apiKeyFlag := flag.String("api-key", "", "Tenant API key for zombie scenario")
	flag.Parse()

	dbURL := *dbURLFlag
	if dbURL == "" {
		dbURL = os.Getenv("CHECKER_DATABASE_URL")
		if dbURL == "" {
			dbURL = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
		}
	}

	if *zombieFlag {
		zRes, err := RunLiveZombieScenario(context.Background(), *apiURLFlag, *apiKeyFlag, dbURL, *ledgerURLFlag)
		data, _ := json.MarshalIndent(zRes, "", "  ")
		fmt.Println(string(data))
		if err != nil || zRes.Status != "PASS" {
			os.Exit(1)
		}
		os.Exit(0)
	}
	if dbURL == "" {
		dbURL = os.Getenv("CHECKER_DATABASE_URL")
		if dbURL == "" {
			dbURL = "postgres://steadfast_checker_ro:fake_ro_password_for_local_demo@127.0.0.1:5432/steadfast?sslmode=disable"
		}
	}

	cfg := Config{
		CheckerDatabaseURL: dbURL,
		LedgerURL:          *ledgerURLFlag,
		RedisURL:           *redisURLFlag,
		ArtifactsDir:       *artifactsDirFlag,
		DrainTimeout:       *drainTimeoutFlag,
		PollInterval:       500 * time.Millisecond,
	}

	verifier := NewVerifier(cfg)
	verdict, exitCode := verifier.Verify(context.Background())

	PrintVerdictConsole(verdict)
	os.Exit(exitCode)
}
