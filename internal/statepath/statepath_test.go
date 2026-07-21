package statepath

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolvePrecedence(t *testing.T) {
	cwd := t.TempDir()
	got, err := Resolve(context.Background(), cwd, "explicit", "environment")
	if err != nil || got != filepath.Join(cwd, "explicit") {
		t.Fatalf("explicit Resolve() = %q, %v", got, err)
	}
	got, err = Resolve(context.Background(), cwd, "", "environment")
	if err != nil || got != filepath.Join(cwd, "environment") {
		t.Fatalf("environment Resolve() = %q, %v", got, err)
	}
}

func TestResolveUsesGitRoot(t *testing.T) {
	root := t.TempDir()
	if err := exec.Command("git", "-C", root, "init", "--quiet").Run(); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Resolve(context.Background(), nested, "", "")
	physicalRoot, resolveErr := filepath.EvalSymlinks(root)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if err != nil || got != filepath.Join(physicalRoot, ".motus") {
		t.Fatalf("Resolve() = %q, %v; want Git root", got, err)
	}
}
