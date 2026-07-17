package handlers

import (
	"encoding/hex"
	"encoding/json"
	"testing"
)

func TestCanonicalParamsHash_OrderIndependent(t *testing.T) {
	a, err := canonicalParamsHash(json.RawMessage(`{"b":2,"a":1}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := canonicalParamsHash(json.RawMessage(`{"a":1,"b":2}`))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("key order changed the hash: %s != %s", a, b)
	}
}

func TestCanonicalParamsHash_IgnoresProcessorMarker(t *testing.T) {
	withMarker, _ := canonicalParamsHash(json.RawMessage(`{"a":1,"_processor":{"name":"x","version":"1"}}`))
	without, _ := canonicalParamsHash(json.RawMessage(`{"a":1}`))
	if withMarker != without {
		t.Fatalf("_processor marker leaked into hash: %s != %s", withMarker, without)
	}
}

func TestCanonicalParamsHash_DistinctParamsDiffer(t *testing.T) {
	a, _ := canonicalParamsHash(json.RawMessage(`{"lang":"en"}`))
	b, _ := canonicalParamsHash(json.RawMessage(`{"lang":"es"}`))
	if a == b {
		t.Fatal("different params hashed the same")
	}
}

func TestComputeCacheKey_StableAndSensitive(t *testing.T) {
	k1 := hex.EncodeToString(computeCacheKey("ih", "ph", "mv"))
	k2 := hex.EncodeToString(computeCacheKey("ih", "ph", "mv"))
	if k1 != k2 {
		t.Fatal("cache key not stable")
	}
	// Each component must influence the key.
	if hex.EncodeToString(computeCacheKey("IH", "ph", "mv")) == k1 {
		t.Fatal("input_hash not part of key")
	}
	if hex.EncodeToString(computeCacheKey("ih", "PH", "mv")) == k1 {
		t.Fatal("params_hash not part of key")
	}
	if hex.EncodeToString(computeCacheKey("ih", "ph", "MV")) == k1 {
		t.Fatal("model_version_id not part of key")
	}
	// The NUL separators must prevent boundary ambiguity.
	if hex.EncodeToString(computeCacheKey("ihph", "", "mv")) == k1 {
		t.Fatal("component boundary is ambiguous")
	}
}
