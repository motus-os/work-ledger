//go:build windows

package capture

import "testing"

func TestResumeFailedUsesDWORDWidth(t *testing.T) {
	if got, want := resumeFailed, uintptr(^uint32(0)); got != want {
		t.Fatalf("ResumeThread failure sentinel = %#x, want DWORD %#x", got, want)
	}
}
