package apikeys

import (
	"bytes"
	"strings"
	"testing"
)

// TestGenerateAPIKey_ShapeAndUniqueness verifies generated keys have the
// expected prefixes, are high-entropy, and don't collide.
func TestGenerateAPIKey_ShapeAndUniqueness(t *testing.T) {
	seenKeys := map[string]bool{}
	seenIDs := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, rawKey, err := GenerateAPIKey()
		if err != nil {
			t.Fatalf("GenerateAPIKey: %v", err)
		}
		if !strings.HasPrefix(rawKey, apiKeyPrefix) {
			t.Fatalf("raw key missing prefix %q: %q", apiKeyPrefix, rawKey)
		}
		if !strings.HasPrefix(id, apiKeyIDPrefix) {
			t.Fatalf("id missing prefix %q: %q", apiKeyIDPrefix, id)
		}
		// 32 random bytes base64url (no padding) -> 43 chars + prefix.
		if got := len(rawKey) - len(apiKeyPrefix); got < 40 {
			t.Fatalf("raw key secret too short: %d chars", got)
		}
		if seenKeys[rawKey] {
			t.Fatal("duplicate raw key generated")
		}
		if seenIDs[id] {
			t.Fatal("duplicate id generated")
		}
		seenKeys[rawKey] = true
		seenIDs[id] = true
	}
}

// TestHashAPIKey_DeterministicAndDistinct verifies the hash is stable for a
// given input and differs across inputs (so lookups work and distinct keys
// don't collide).
func TestHashAPIKey_DeterministicAndDistinct(t *testing.T) {
	h1 := hashAPIKey("fuse_sk_aaa")
	h2 := hashAPIKey("fuse_sk_aaa")
	h3 := hashAPIKey("fuse_sk_bbb")

	if !bytes.Equal(h1, h2) {
		t.Error("hash not deterministic for same input")
	}
	if bytes.Equal(h1, h3) {
		t.Error("hash collided for different inputs")
	}
	if len(h1) != 32 {
		t.Errorf("SHA-256 hash length: got %d, want 32", len(h1))
	}
	// The hash must not be the raw key.
	if string(h1) == "fuse_sk_aaa" {
		t.Error("hash equals raw key")
	}
}
