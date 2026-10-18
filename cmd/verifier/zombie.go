package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

type ZombieResult struct {
	Status        string `json:"status"` // PASS or FAIL
	StaleHTTPCode int    `json:"stale_http_code"`
	Details       string `json:"details"`
	JobID         string `json:"job_id"`
	OrderID       string `json:"order_id"`
	AppliedCount  int    `json:"applied_charges_count"`
}

func RunLiveZombieScenario(ctx context.Context, apiURL, apiKey, dbURL, ledgerURL string) (ZombieResult, error) {
	result := ZombieResult{
		Status: "FAIL",
	}

	if apiKey == "" {
		apiKey = os.Getenv("DEMO_TENANT_A_KEY")
		if apiKey == "" {
			apiKey = os.Getenv("TENANT_KEY")
		}
		if apiKey == "" {
			// Read from .env if possible
			for _, p := range []string{".env", "../.env", "../../.env", "/home/dkvis/steadfast/.env"} {
				data, err := os.ReadFile(p)
				if err == nil {
					for _, line := range strings.Split(string(data), "\n") {
						if strings.HasPrefix(line, "DEMO_TENANT_A_KEY=") {
							apiKey = strings.TrimSpace(strings.TrimPrefix(line, "DEMO_TENANT_A_KEY="))
							break
						}
					}
				}
				if apiKey != "" {
					break
				}
			}
		}
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Submit a job
	orderID := fmt.Sprintf("ord-zombie-%s", uuid.NewString()[:8])
	result.OrderID = orderID
	idemKey := fmt.Sprintf("zombie-key-%s", uuid.NewString()[:8])

	submitPayload := map[string]any{
		"job_type": "charge_customer",
		"payload": map[string]any{
			"order_id":    orderID,
			"customer_id": "cust-zombie",
			"amount":      99.95,
			"currency":    "USD",
		},
	}
	submitBytes, _ := json.Marshal(submitPayload)
	submitReq, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/jobs", bytes.NewReader(submitBytes))
	if err != nil {
		return result, fmt.Errorf("failed to create submit request: %w", err)
	}
	submitReq.Header.Set("Content-Type", "application/json")
	submitReq.Header.Set("Authorization", "Bearer "+apiKey)
	submitReq.Header.Set("Idempotency-Key", idemKey)

	submitResp, err := client.Do(submitReq)
	if err != nil {
		return result, fmt.Errorf("failed to submit job: %w", err)
	}
	defer func() { _ = submitResp.Body.Close() }()

	if submitResp.StatusCode != http.StatusAccepted {
		return result, fmt.Errorf("expected 202 Accepted from submit, got %d", submitResp.StatusCode)
	}

	var submitData struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(submitResp.Body).Decode(&submitData); err != nil {
		return result, fmt.Errorf("failed to decode submit response: %w", err)
	}
	jobID := submitData.ID
	result.JobID = jobID

	// 2. Worker 1 claims job with 1-second lease
	claimPayload1 := map[string]any{
		"worker_id":        "worker-zombie-1",
		"job_types":        []string{"charge_customer"},
		"limit":            1,
		"lease_duration_ms": 1000,
	}
	claimBytes1, _ := json.Marshal(claimPayload1)
	claimReq1, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/jobs/claim", bytes.NewReader(claimBytes1))
	claimReq1.Header.Set("Content-Type", "application/json")
	claimReq1.Header.Set("Authorization", "Bearer "+apiKey)

	claimResp1, err := client.Do(claimReq1)
	if err != nil {
		return result, fmt.Errorf("claim 1 failed: %w", err)
	}
	defer func() { _ = claimResp1.Body.Close() }()

	var claimData1 struct {
		Jobs []struct {
			ID           string `json:"id"`
			FencingToken int64  `json:"fencing_token"`
		} `json:"jobs"`
	}
	_ = json.NewDecoder(claimResp1.Body).Decode(&claimData1)
	if len(claimData1.Jobs) == 0 || claimData1.Jobs[0].ID != jobID {
		return result, fmt.Errorf("worker 1 failed to claim job %s", jobID)
	}
	token1 := claimData1.Jobs[0].FencingToken

	// 3. Worker 1 charges ledger with token 1
	chargePayload := map[string]any{
		"order_id":    orderID,
		"customer_id": "cust-zombie",
		"amount":      99.95,
		"currency":    "USD",
	}
	chargeBytes, _ := json.Marshal(chargePayload)
	chargeReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ledgerURL+"/charge", bytes.NewReader(chargeBytes))
	chargeReq.Header.Set("Content-Type", "application/json")
	chargeReq.Header.Set("Idempotency-Key", fmt.Sprintf("steadfast:%s", jobID))
	chargeReq.Header.Set("X-Fencing-Token", fmt.Sprintf("%d", token1))

	chargeResp, err := client.Do(chargeReq)
	if err != nil {
		return result, fmt.Errorf("charge 1 failed: %w", err)
	}
	_ = chargeResp.Body.Close()

	// 4. Sleep 1.5 seconds so lease expires and reaper reaps
	time.Sleep(1500 * time.Millisecond)

	// 5. Worker 2 claims the re-queued job
	var token2 int64
	deadline := time.Now().Add(5 * time.Second)
	claimedBy2 := false
	for time.Now().Before(deadline) {
		claimPayload2 := map[string]any{
			"worker_id":        "worker-zombie-2",
			"job_types":        []string{"charge_customer"},
			"limit":            1,
			"lease_duration_ms": 5000,
		}
		claimBytes2, _ := json.Marshal(claimPayload2)
		claimReq2, _ := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/v1/jobs/claim", bytes.NewReader(claimBytes2))
		claimReq2.Header.Set("Content-Type", "application/json")
		claimReq2.Header.Set("Authorization", "Bearer "+apiKey)

		claimResp2, err := client.Do(claimReq2)
		if err == nil {
			var claimData2 struct {
				Jobs []struct {
					ID           string `json:"id"`
					FencingToken int64  `json:"fencing_token"`
				} `json:"jobs"`
			}
			_ = json.NewDecoder(claimResp2.Body).Decode(&claimData2)
			_ = claimResp2.Body.Close()
			for _, j := range claimData2.Jobs {
				if j.ID == jobID {
					token2 = j.FencingToken
					claimedBy2 = true
					break
				}
			}
		}
		if claimedBy2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !claimedBy2 {
		return result, fmt.Errorf("worker 2 timed out waiting to claim reaped job %s", jobID)
	}

	// 6. Worker 2 completes job with token 2
	compPayload2 := map[string]any{
		"worker_id":     "worker-zombie-2",
		"fencing_token": token2,
		"result":        map[string]any{"status": "charged"},
	}
	compBytes2, _ := json.Marshal(compPayload2)
	compReq2, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/complete", apiURL, jobID), bytes.NewReader(compBytes2))
	compReq2.Header.Set("Content-Type", "application/json")
	compReq2.Header.Set("Authorization", "Bearer "+apiKey)

	compResp2, err := client.Do(compReq2)
	if err != nil || compResp2.StatusCode != http.StatusOK {
		return result, fmt.Errorf("worker 2 complete failed: %v", err)
	}
	_ = compResp2.Body.Close()

	// 7. Stale Worker 1 wakes up and attempts to complete with token 1
	compPayload1 := map[string]any{
		"worker_id":     "worker-zombie-1",
		"fencing_token": token1,
		"result":        map[string]any{"status": "zombie_override"},
	}
	compBytes1, _ := json.Marshal(compPayload1)
	compReq1, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/complete", apiURL, jobID), bytes.NewReader(compBytes1))
	compReq1.Header.Set("Content-Type", "application/json")
	compReq1.Header.Set("Authorization", "Bearer "+apiKey)

	compResp1, err := client.Do(compReq1)
	if err != nil {
		return result, fmt.Errorf("stale worker 1 complete request failed: %w", err)
	}
	defer func() { _ = compResp1.Body.Close() }()

	result.StaleHTTPCode = compResp1.StatusCode

	// 8. Verify DB state independently
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return result, fmt.Errorf("failed to connect to db for verification: %w", err)
	}
	defer func() { _ = db.Close() }()

	var dbState string
	var dbToken int64
	err = db.QueryRowContext(ctx, "SELECT state, fencing_token FROM jobs WHERE id = $1", jobID).Scan(&dbState, &dbToken)
	if err != nil {
		return result, fmt.Errorf("failed to query job from db: %w", err)
	}

	// 9. Verify Ledger export
	expResp, err := client.Get(ledgerURL + "/export")
	if err != nil {
		return result, fmt.Errorf("failed to export ledger: %w", err)
	}
	defer func() { _ = expResp.Body.Close() }()

	var ledgerEntries []struct {
		OrderID   string `json:"order_id"`
		EntryType string `json:"entry_type"`
	}
	decoder := json.NewDecoder(expResp.Body)
	for decoder.More() {
		var e struct {
			OrderID   string `json:"order_id"`
			EntryType string `json:"entry_type"`
		}
		if err := decoder.Decode(&e); err == nil {
			ledgerEntries = append(ledgerEntries, e)
		}
	}

	appliedCount := 0
	for _, e := range ledgerEntries {
		if e.OrderID == orderID && e.EntryType == "applied" {
			appliedCount++
		}
	}
	result.AppliedCount = appliedCount

	// Evaluation:
	// A proper fencing system must return 409 Conflict to the stale complete,
	// maintain state SUCCEEDED with token 2, and have exactly 1 applied ledger entry.
	if compResp1.StatusCode == http.StatusConflict && dbState == "SUCCEEDED" && dbToken == token2 && appliedCount == 1 {
		result.Status = "PASS"
		result.Details = "Stale write rejected with 409 Conflict; job remains SUCCEEDED with token 2 and exactly 1 applied charge"
		return result, nil
	}

	// Zombie write occurred
	result.Status = "FAIL"
	if compResp1.StatusCode != http.StatusConflict {
		result.Details = fmt.Sprintf("Zombie write accepted with HTTP %d (expected 409 Conflict)", compResp1.StatusCode)
	} else if appliedCount != 1 {
		result.Details = fmt.Sprintf("Ledger has %d applied charges (expected 1)", appliedCount)
	} else {
		result.Details = fmt.Sprintf("Database state is %s with token %d (expected SUCCEEDED with token %d)", dbState, dbToken, token2)
	}

	return result, nil
}
