package ids

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	seen := make(map[string]bool)
	for range 1000 {
		id, err := New("run_")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "run_") || len(id) != len("run_")+32 {
			t.Fatalf("New() = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate ID %q", id)
		}
		seen[id] = true
	}
	for _, prefix := range []string{"", "run", "Run_", "bad-prefix_", "_"} {
		if _, err := New(prefix); err == nil {
			t.Fatalf("New(%q) succeeded", prefix)
		}
	}
}
