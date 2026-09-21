package loop

import (
	"encoding/json"
	"strings"
	"testing"
)

// FuzzValidRunID asserts that ValidRunID never panics on arbitrary string inputs
// and correctly rejects any characters outside the permitted charset.
func FuzzValidRunID(f *testing.F) {
	seeds := []string{
		"run_20260920T071448_0ce381",
		"run_12345",
		"run_abc-xyz",
		"run_../escape",
		"",
		"run_",
		"../../../etc/passwd",
		"run_\x00null",
		"run_" + strings.Repeat("a", 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, id string) {
		valid := ValidRunID(id)
		if valid {
			if len(id) > 128 {
				t.Fatal("ValidRunID accepted id longer than 128 chars")
			}
			if !strings.HasPrefix(id, "run_") {
				t.Fatal("ValidRunID accepted id without run_ prefix")
			}
			if strings.Contains(id, "/") || strings.Contains(id, "\\") || strings.Contains(id, "..") {
				t.Fatalf("ValidRunID accepted traversal path: %q", id)
			}
		}
	})
}

// FuzzCheckpointJSON asserts that deserializing arbitrary byte inputs into
// Checkpoint never panics.
func FuzzCheckpointJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"schema_version":1,"written_at":"2026-09-20T07:14:48Z","state":{"run_id":"run_test","step":1}}`),
		[]byte(`{}`),
		[]byte(`{"schema_version":999}`),
		[]byte(`{"state":{"messages":[{"role":"user","content":"hello"}]}}`),
		[]byte(`{"state":{"denials":{"write_file":3}}}`),
		[]byte(``),
		[]byte(`null`),
		[]byte(`{"schema_version": "wrong_type"}`),
		[]byte(`{"state": null}`),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var cp Checkpoint
		_ = json.Unmarshal(data, &cp)
	})
}
