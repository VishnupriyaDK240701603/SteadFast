package idempotency

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrEmptyPayload = errors.New("payload cannot be empty")
	ErrInvalidJSON  = errors.New("payload must be valid JSON")
)

// CanonicalizeJSON parses raw JSON and marshals it with lexicographically sorted keys.
// Go's standard json.Marshal on map[string]any recursively sorts map keys.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	if len(raw) == 0 {
		return nil, ErrEmptyPayload
	}

	var val any
	if err := json.Unmarshal(raw, &val); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidJSON, err)
	}

	canonical, err := json.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal canonical JSON: %w", err)
	}

	return canonical, nil
}

// HashPayload computes the SHA-256 hash of canonical bytes and returns it as a lower-case hex string.
func HashPayload(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
