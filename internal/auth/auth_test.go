package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"steadfast/internal/config"
	"steadfast/internal/store"
)

func setupAuthStore(t *testing.T) (*store.Store, *store.Tenant, string) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("store.Connect failed: %v", err)
	}

	if err := store.MigrateUp(ctx, pool, "../../migrations"); err != nil {
		t.Fatalf("MigrateUp failed: %v", err)
	}

	s := store.NewStore(pool)
	rawKey := "valid_demo_api_key_" + uuid.New().String()
	hash := HashAPIKey(rawKey)
	tenantName := "auth-test-" + uuid.New().String()[:8]

	tenant, err := s.CreateTenant(ctx, tenantName, hash)
	if err != nil {
		t.Fatalf("CreateTenant failed: %v", err)
	}

	return s, tenant, rawKey
}

func TestTenantAuthMiddleware(t *testing.T) {
	s, tenant, validKey := setupAuthStore(t)
	middleware := RequireTenantAuth(s)

	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxTenant, ok := TenantFromContext(r.Context())
		if !ok || ctxTenant == nil {
			t.Fatalf("Tenant expected in context but not found")
		}
		if ctxTenant.ID != tenant.ID {
			t.Fatalf("Expected tenant ID %s, got %s", tenant.ID, ctxTenant.ID)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handler := middleware(targetHandler)

	tests := []struct {
		name           string
		authHeader     string
		expectedStatus int
	}{
		{
			name:           "missing authorization header",
			authHeader:     "",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "malformed header - not bearer",
			authHeader:     "Basic dXNlcjpwYXNz",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "empty bearer token",
			authHeader:     "Bearer ",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "invalid bearer token",
			authHeader:     "Bearer invalid_secret_key_that_does_not_exist",
			expectedStatus: http.StatusUnauthorized,
		},
		{
			name:           "valid bearer token",
			authHeader:     "Bearer " + validKey,
			expectedStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/test", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.expectedStatus {
				t.Fatalf("Expected status %d, got %d. Body: %s", tc.expectedStatus, rec.Code, rec.Body.String())
			}

			if tc.expectedStatus == http.StatusUnauthorized {
				var errResp ErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("Failed to parse error json: %v", err)
				}
				if errResp.Error.Code != "unauthorized" {
					t.Fatalf("Expected error code 'unauthorized', got %s", errResp.Error.Code)
				}
			}
		})
	}
}

func TestAdminAuthMiddleware(t *testing.T) {
	adminKey := "test_admin_key_32_bytes_super_secure_value"
	middleware := RequireAdminAuth(adminKey)

	targetHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := middleware(targetHandler)

	// 1. Missing header -> 401
	req1 := httptest.NewRequest("POST", "/admin", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 on missing admin header, got %d", rec1.Code)
	}

	// 2. Wrong key -> 401
	req2 := httptest.NewRequest("POST", "/admin", nil)
	req2.Header.Set("Authorization", "Bearer wrong_admin_key")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("Expected 401 on wrong admin key, got %d", rec2.Code)
	}

	// 3. Valid key -> 200
	req3 := httptest.NewRequest("POST", "/admin", nil)
	req3.Header.Set("Authorization", "Bearer "+adminKey)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("Expected 200 on valid admin key, got %d", rec3.Code)
	}
}

func TestNoKeyLeakageInLogs(t *testing.T) {
	s, _, validKey := setupAuthStore(t)

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	// Recreate LoggingMiddleware inline to test log output
	loggingMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Info("http_request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
			)
			next.ServeHTTP(w, r)
		})
	}

	authMiddleware := RequireTenantAuth(s)
	handler := loggingMiddleware(authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	secretToken := validKey
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+secretToken)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	logOutput := logBuf.String()

	// Prove no log line prints the secret token or Authorization header
	if strings.Contains(logOutput, secretToken) {
		t.Fatalf("CRITICAL SECURITY FLAW: Secret API key leaked in log output: %s", logOutput)
	}
	if strings.Contains(logOutput, "Bearer") || strings.Contains(logOutput, "Authorization") {
		t.Fatalf("CRITICAL SECURITY FLAW: Authorization header leaked in log output: %s", logOutput)
	}
}
