package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"steadfast/sdk/worker"

	"github.com/google/uuid"
)

func TestChargeCustomerHandler_Success(t *testing.T) {
	jobID := uuid.New()
	var chargeReceived int32
	var capturedKey string
	var capturedToken string

	ledgerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/charge" {
			atomic.AddInt32(&chargeReceived, 1)
			capturedKey = r.Header.Get("Idempotency-Key")
			capturedToken = r.Header.Get("X-Fencing-Token")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"applied","order_id":"ord-123","amount_cents":4500}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ledgerServer.Close()

	handler := ChargeCustomerHandler(ledgerServer.URL, ledgerServer.Client())

	job := &worker.Job{
		ID:           jobID,
		Type:         "charge_customer",
		Payload:      json.RawMessage(`{"order_id":"ord-123","customer_id":"cust-1","amount_cents":4500}`),
		FencingToken: 7,
	}

	err := handler(context.Background(), job)
	if err != nil {
		t.Fatalf("expected handler to succeed, got %v", err)
	}

	if atomic.LoadInt32(&chargeReceived) != 1 {
		t.Fatalf("expected 1 charge request, got %d", atomic.LoadInt32(&chargeReceived))
	}
	expectedKey := "steadfast:" + jobID.String()
	if capturedKey != expectedKey {
		t.Fatalf("expected Idempotency-Key %q, got %q", expectedKey, capturedKey)
	}
	if capturedToken != "7" {
		t.Fatalf("expected X-Fencing-Token '7', got %q", capturedToken)
	}
}

func TestChargeCustomerHandler_MalformedPayload_Permanent(t *testing.T) {
	handler := ChargeCustomerHandler("http://127.0.0.1:9999", nil)
	job := &worker.Job{
		ID:      uuid.New(),
		Type:    "charge_customer",
		Payload: json.RawMessage(`{"bad json syntax`),
	}

	err := handler(context.Background(), job)
	if err == nil {
		t.Fatalf("expected error on malformed payload")
	}
	if worker.IsRetryable(err) {
		t.Fatalf("expected malformed payload error to be classified as permanent")
	}
}

func TestChargeCustomerHandler_Downstream4xx_Permanent(t *testing.T) {
	ledgerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_amount","message":"amount must be positive"}}`))
	}))
	defer ledgerServer.Close()

	handler := ChargeCustomerHandler(ledgerServer.URL, ledgerServer.Client())
	job := &worker.Job{
		ID:      uuid.New(),
		Type:    "charge_customer",
		Payload: json.RawMessage(`{"order_id":"ord-neg","customer_id":"cust-1","amount_cents":-50}`),
	}

	err := handler(context.Background(), job)
	if err == nil {
		t.Fatalf("expected error on downstream 400")
	}
	if worker.IsRetryable(err) {
		t.Fatalf("expected downstream 4xx error to be classified as permanent")
	}
}

func TestChargeCustomerHandler_Downstream5xx_Retryable(t *testing.T) {
	ledgerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error"}}`))
	}))
	defer ledgerServer.Close()

	handler := ChargeCustomerHandler(ledgerServer.URL, ledgerServer.Client())
	job := &worker.Job{
		ID:      uuid.New(),
		Type:    "charge_customer",
		Payload: json.RawMessage(`{"order_id":"ord-500","customer_id":"cust-1","amount_cents":100}`),
	}

	err := handler(context.Background(), job)
	if err == nil {
		t.Fatalf("expected error on downstream 500")
	}
	if !worker.IsRetryable(err) {
		t.Fatalf("expected downstream 5xx error to be classified as retryable")
	}
}
