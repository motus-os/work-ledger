package store

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzCanonicalJSON(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"b":2,"a":1}`),
		[]byte(`[true,null,"text",1.50]`),
		[]byte(`{"duplicate":1,"duplicate":2}`),
		{0xff},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		canonical, err := canonicalJSON(json.RawMessage(input))
		if err != nil {
			return
		}
		if len(canonical) > MaxEventPayloadBytes {
			t.Fatalf("canonical output exceeds limit: %d", len(canonical))
		}
		if !json.Valid(canonical) {
			t.Fatalf("canonical output is invalid JSON: %q", canonical)
		}
		again, err := canonicalJSON(canonical)
		if err != nil {
			t.Fatalf("canonical output was rejected: %v", err)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatalf("canonicalization is not idempotent: %q then %q", canonical, again)
		}
	})
}
