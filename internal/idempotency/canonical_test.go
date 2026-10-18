package idempotency

import (
	"testing"
)

func TestCanonicalizeJSONKeyOrdering(t *testing.T) {
	p1 := []byte(`{"zebra": 100, "apple": "pie", "banana": true}`)
	p2 := []byte(`{"apple": "pie", "banana": true, "zebra": 100}`)
	p3 := []byte("{\n  \"banana\": true,\n  \"zebra\": 100,\n  \"apple\": \"pie\"\n}")

	c1, err := CanonicalizeJSON(p1)
	if err != nil {
		t.Fatalf("CanonicalizeJSON(p1) failed: %v", err)
	}
	c2, err := CanonicalizeJSON(p2)
	if err != nil {
		t.Fatalf("CanonicalizeJSON(p2) failed: %v", err)
	}
	c3, err := CanonicalizeJSON(p3)
	if err != nil {
		t.Fatalf("CanonicalizeJSON(p3) failed: %v", err)
	}

	if string(c1) != string(c2) || string(c1) != string(c3) {
		t.Fatalf("Canonical JSON mismatch: c1=%s, c2=%s, c3=%s", c1, c2, c3)
	}

	h1 := HashPayload(c1)
	h2 := HashPayload(c2)
	h3 := HashPayload(c3)

	if h1 != h2 || h1 != h3 {
		t.Fatalf("Hash mismatch for identical semantic payloads: %s vs %s vs %s", h1, h2, h3)
	}
}

func TestCanonicalizeJSONNested(t *testing.T) {
	p1 := []byte(`{"outer": {"z": 1, "a": 2}, "list": [1, 2, 3]}`)
	p2 := []byte(`{"list": [1, 2, 3], "outer": {"a": 2, "z": 1}}`)

	c1, err := CanonicalizeJSON(p1)
	if err != nil {
		t.Fatalf("CanonicalizeJSON(p1) failed: %v", err)
	}
	c2, err := CanonicalizeJSON(p2)
	if err != nil {
		t.Fatalf("CanonicalizeJSON(p2) failed: %v", err)
	}

	if string(c1) != string(c2) {
		t.Fatalf("Nested JSON mismatch: c1=%s, c2=%s", c1, c2)
	}
	if HashPayload(c1) != HashPayload(c2) {
		t.Fatalf("Nested hash mismatch")
	}
}

func TestCanonicalizeJSONInvalid(t *testing.T) {
	if _, err := CanonicalizeJSON([]byte("")); err == nil {
		t.Fatalf("Expected error on empty payload, got nil")
	}
	if _, err := CanonicalizeJSON([]byte("{not_json}")); err == nil {
		t.Fatalf("Expected error on invalid JSON, got nil")
	}
}
