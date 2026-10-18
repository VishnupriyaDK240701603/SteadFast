package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"steadfast/internal/store"
)

type contextKey string

const (
	tenantContextKey contextKey = "steadfast.tenant"
)

var (
	ErrUnauthorized = errors.New("unauthorized: missing or invalid authentication token")
)

type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

func WithTenant(ctx context.Context, t *store.Tenant) context.Context {
	return context.WithValue(ctx, tenantContextKey, t)
}

func TenantFromContext(ctx context.Context) (*store.Tenant, bool) {
	t, ok := ctx.Value(tenantContextKey).(*store.Tenant)
	return t, ok && t != nil
}

func ValidateAdminKey(provided, expected string) bool {
	if len(provided) == 0 || len(expected) == 0 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func WriteJSONError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
		},
	})
}

func RequireTenantAuth(s *store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			rawKey := strings.TrimPrefix(authHeader, "Bearer ")
			rawKey = strings.TrimSpace(rawKey)
			if rawKey == "" {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
				return
			}

			keyHash := HashAPIKey(rawKey)
			tenant, err := s.GetTenantByAPIKeyHash(r.Context(), keyHash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					// Invalid key: constant timing / generic error
					WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing authentication token")
					return
				}
				// Database unreachable / connection outage: Engine Law 1 & A1.2
				w.Header().Set("Retry-After", "1")
				WriteJSONError(w, http.StatusServiceUnavailable, "service_unavailable", "database unreachable")
				return
			}

			ctx := WithTenant(r.Context(), tenant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func RequireAdminAuth(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin key")
				return
			}

			rawKey := strings.TrimPrefix(authHeader, "Bearer ")
			if !ValidateAdminKey(strings.TrimSpace(rawKey), adminKey) {
				WriteJSONError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin key")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
