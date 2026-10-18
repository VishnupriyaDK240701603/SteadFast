package config

import "testing"

func TestMutationFlagsStrictlyForbiddenOutsideDemo(t *testing.T) {
	// Case 1: In demo mode, flags allowed
	cfgDemo := &Config{
		Env:            "demo",
		DisableFencing: true,
	}
	if err := cfgDemo.ValidateMutationFlags(); err != nil {
		t.Fatalf("Expected mutation flags allowed in demo mode, got error: %v", err)
	}

	// Case 2: In staging / prod, DisableFencing rejected
	cfgProdFencing := &Config{
		Env:            "production",
		DisableFencing: true,
	}
	if err := cfgProdFencing.ValidateMutationFlags(); err == nil {
		t.Fatalf("Expected error when DisableFencing is set in production, got nil")
	}

	// Case 3: In staging / prod, DisableIdempotency rejected
	cfgProdIdemp := &Config{
		Env:                "production",
		DisableIdempotency: true,
	}
	if err := cfgProdIdemp.ValidateMutationFlags(); err == nil {
		t.Fatalf("Expected error when DisableIdempotency is set in production, got nil")
	}

	// Case 4: In staging / prod with flags off, allowed
	cfgProdClean := &Config{
		Env:                "production",
		DisableFencing:     false,
		DisableIdempotency: false,
	}
	if err := cfgProdClean.ValidateMutationFlags(); err != nil {
		t.Fatalf("Expected clean production config to be valid, got: %v", err)
	}
}
