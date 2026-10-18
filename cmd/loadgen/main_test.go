package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLoadgen_IntentAndAckLogging(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "loadgen-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		att := atomic.AddInt32(&attempts, 1)
		if att == 1 {
			// First attempt returns 503 to test retry
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"id":"job-%d","state":"QUEUED"}`, att)
	}))
	defer server.Close()

	cfg := Config{
		Count:       3,
		Rate:        0,
		Seed:        12345,
		Lane:        "steadfast",
		APIBaseURL:  server.URL,
		TenantKey:   "test-key",
		OutDir:      tempDir,
		RunID:       "test-run-01",
	}

	lg := NewLoadGenerator(cfg, nil)
	if err := lg.Run(context.Background()); err != nil {
		t.Fatalf("loadgen run failed: %v", err)
	}

	// Verify intent file has 3 records
	intentPath := filepath.Join(tempDir, "intent-steadfast.ndjson")
	iFile, err := os.Open(intentPath)
	if err != nil {
		t.Fatalf("failed to open intent file: %v", err)
	}
	defer func() { _ = iFile.Close() }()

	var intentCount int
	scanner := bufio.NewScanner(iFile)
	for scanner.Scan() {
		var rec IntentRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("invalid intent json: %v", err)
		}
		intentCount++
	}
	if intentCount != 3 {
		t.Fatalf("expected 3 intent records, got %d", intentCount)
	}

	// Verify ack file has 3 records
	ackPath := filepath.Join(tempDir, "acks-steadfast.ndjson")
	aFile, err := os.Open(ackPath)
	if err != nil {
		t.Fatalf("failed to open ack file: %v", err)
	}
	defer func() { _ = aFile.Close() }()

	var ackCount int
	aScanner := bufio.NewScanner(aFile)
	for aScanner.Scan() {
		var rec AckRecord
		if err := json.Unmarshal(aScanner.Bytes(), &rec); err != nil {
			t.Fatalf("invalid ack json: %v", err)
		}
		if rec.JobID == "" {
			t.Fatalf("missing job_id in ack record")
		}
		ackCount++
	}
	if ackCount != 3 {
		t.Fatalf("expected 3 ack records, got %d", ackCount)
	}
}
