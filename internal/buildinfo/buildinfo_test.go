package buildinfo

import "testing"

func TestEffectiveVersionUsesInjectedVersion(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })
	Version = "v1.2.3"
	if got := EffectiveVersion(); got != "v1.2.3" {
		t.Fatalf("EffectiveVersion() = %q, want v1.2.3", got)
	}
}
