package gitmeta

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDetectCleanAndDirtyRepositories(t *testing.T) {
	isolateGitConfig(t)

	t.Run("clean from nested directory", func(t *testing.T) {
		root, head := committedRepository(t, "repo")
		nested := filepath.Join(root, "nested", "directory")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}

		want := Metadata{Root: root, HeadSHA: head}
		if got := Detect(context.Background(), nested); got != want {
			t.Fatalf("Detect() = %#v, want %#v", got, want)
		}
	})

	t.Run("modified tracked file", func(t *testing.T) {
		root, head := committedRepository(t, "repo")
		if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("changed\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		want := Metadata{Root: root, HeadSHA: head, Dirty: true}
		if got := Detect(context.Background(), root); got != want {
			t.Fatalf("Detect() = %#v, want %#v", got, want)
		}
	})

	t.Run("private untracked filename", func(t *testing.T) {
		root, head := committedRepository(t, "repo")
		privateName := "private-token-do-not-retain\nfile"
		if runtime.GOOS == "windows" {
			privateName = "private-token-do-not-retain file"
		}
		if err := os.WriteFile(filepath.Join(root, privateName), []byte("private\n"), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		want := Metadata{Root: root, HeadSHA: head, Dirty: true}
		if got := Detect(context.Background(), root); got != want {
			t.Fatalf("Detect() = %#v, want %#v", got, want)
		}
	})
}

func TestDetectUnbornHEAD(t *testing.T) {
	isolateGitConfig(t)
	root := initializedRepository(t, "repo")

	want := Metadata{Root: root}
	if got := Detect(context.Background(), root); got != want {
		t.Fatalf("Detect() = %#v, want %#v", got, want)
	}

	if err := os.WriteFile(filepath.Join(root, "first"), []byte("staged\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	runGit(t, root, "add", "first")
	want.Dirty = true
	if got := Detect(context.Background(), root); got != want {
		t.Fatalf("Detect() with staged file = %#v, want %#v", got, want)
	}
}

func TestDetectRejectsNonBranchSymbolicHEAD(t *testing.T) {
	isolateGitConfig(t)

	for _, target := range []string{"refs/tags/missing", "refs/remotes/origin/missing"} {
		t.Run(target, func(t *testing.T) {
			root := initializedRepository(t, "repo")
			gitDir := commandLine(runGit(t, root, "rev-parse", "--absolute-git-dir"))
			if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: "+target+"\n"), 0o600); err != nil {
				t.Fatalf("WriteFile(HEAD) error = %v", err)
			}

			if got := Detect(context.Background(), root); got != (Metadata{}) {
				t.Fatalf("Detect() = %#v, want zero metadata for non-branch HEAD", got)
			}
		})
	}
}

func TestDetectRejectsNonRepository(t *testing.T) {
	isolateGitConfig(t)
	directory := filepath.Join(t.TempDir(), "not-a-repository")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	if got := Detect(context.Background(), directory); got != (Metadata{}) {
		t.Fatalf("Detect() = %#v, want zero metadata", got)
	}
}

func TestDetectHonorsCanceledContext(t *testing.T) {
	isolateGitConfig(t)
	root, _ := committedRepository(t, "repo")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := Detect(ctx, root); got != (Metadata{}) {
		t.Fatalf("Detect() = %#v, want zero metadata", got)
	}
}

func TestGitTextCancellationDoesNotWaitForDescendantPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper requires a POSIX shell")
	}
	isolateGitConfig(t)
	root := initializedRepository(t, "repo")
	helperDir := t.TempDir()
	helper := filepath.Join(helperDir, "leak-pipe")
	marker := filepath.Join(helperDir, "started")
	t.Setenv("GITMETA_TEST_MARKER", marker)
	if err := os.WriteFile(helper, []byte("#!/bin/sh\n: > \"$GITMETA_TEST_MARKER\"\n/bin/sleep 1 &\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(helper) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	result := make(chan bool, 1)
	go func() {
		_, ok := gitText(
			ctx,
			32,
			"-C", root,
			"-c", "alias.gitmeta-leak=!"+shellQuote(helper),
			"gitmeta-leak",
		)
		result <- ok
	}()

	waitForFile(t, marker, time.Second)
	started := time.Now()
	cancel()
	ok := <-result
	elapsed := time.Since(started)
	if ok {
		t.Error("gitText() succeeded after its context was canceled")
	}
	if elapsed > 700*time.Millisecond {
		t.Errorf("gitText() returned %v after %v; descendant-held pipes were not bounded", ok, elapsed)
	}
}

func TestDetectIgnoresInheritedRepositoryLocation(t *testing.T) {
	isolateGitConfig(t)
	targetRoot, targetHead := committedRepository(t, "target")
	otherRoot, _ := committedRepository(t, "other")
	otherGitDir := commandLine(runGit(t, otherRoot, "rev-parse", "--absolute-git-dir"))

	t.Setenv("GIT_DIR", otherGitDir)
	t.Setenv("GIT_WORK_TREE", otherRoot)
	want := Metadata{Root: targetRoot, HeadSHA: targetHead}
	if got := Detect(context.Background(), targetRoot); got != want {
		t.Fatalf("Detect() = %#v, want %#v; inherited Git environment redirected detection", got, want)
	}
}

func TestDetectDoesNotRunConfiguredFSMonitor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test hook requires a POSIX shell")
	}
	isolateGitConfig(t)
	root, head := committedRepository(t, "repo")
	helperDir := t.TempDir()
	hook := filepath.Join(helperDir, "fsmonitor")
	marker := filepath.Join(helperDir, "called")
	t.Setenv("GITMETA_TEST_FSMONITOR_MARKER", marker)
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n: > \"$GITMETA_TEST_FSMONITOR_MARKER\"\nprintf '\\0'\n"), 0o700); err != nil {
		t.Fatalf("WriteFile(hook) error = %v", err)
	}
	runGit(t, root, "config", "core.fsmonitor", hook)

	// Prove this Git build executes the configured hook before asserting that
	// Detect suppresses it.
	runGit(t, root, "status", "--porcelain=v1", "-z", "--untracked-files=normal")
	if _, err := os.Stat(marker); err != nil {
		if os.IsNotExist(err) {
			t.Skip("installed Git does not invoke configured fsmonitor hooks")
		}
		t.Fatalf("Stat(marker) error = %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("Remove(marker) error = %v", err)
	}

	want := Metadata{Root: root, HeadSHA: head}
	if got := Detect(context.Background(), root); got != want {
		t.Fatalf("Detect() = %#v, want %#v", got, want)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Detect() executed repository-configured fsmonitor hook: Stat() error = %v", err)
	}
}

func TestDetectDoesNotModifyIndex(t *testing.T) {
	isolateGitConfig(t)
	t.Setenv("GIT_OPTIONAL_LOCKS", "1")
	root, head := committedRepository(t, "repo")
	indexPath := commandLine(runGit(t, root, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	before, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("ReadFile(index) error = %v", err)
	}

	tracked := filepath.Join(root, "tracked")
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(tracked, future, future); err != nil {
		t.Fatalf("Chtimes() error = %v", err)
	}

	want := Metadata{Root: root, HeadSHA: head}
	if got := Detect(context.Background(), root); got != want {
		t.Fatalf("Detect() = %#v, want %#v", got, want)
	}
	after, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("ReadFile(index after Detect) error = %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Detect() refreshed and rewrote the repository index")
	}
}

func TestDetectPreservesTrailingCarriageReturnInRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain carriage returns")
	}
	isolateGitConfig(t)
	root, head := committedRepository(t, "repo\r")

	want := Metadata{Root: root, HeadSHA: head}
	if got := Detect(context.Background(), root); got != want {
		t.Fatalf("Detect() = %#v, want %#v", got, want)
	}
}

func TestBoundedOutputRejectsOverflow(t *testing.T) {
	output := boundedOutput{remaining: 32}
	privateOutput := bytes.Repeat([]byte("private-path\x00"), 1<<12)
	written, err := output.Write(privateOutput)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if written != len(privateOutput) {
		t.Fatalf("Write() = %d, want %d", written, len(privateOutput))
	}
	if !output.overflow || len(output.data) != 32 || output.remaining != 0 {
		t.Fatalf("boundedOutput = %#v, want 32 retained bytes and overflow", output)
	}

	if _, err := output.Write(privateOutput); err != nil {
		t.Fatalf("second Write() error = %v", err)
	}
	if len(output.data) != 32 {
		t.Fatalf("second Write() retained %d bytes, want 32", len(output.data))
	}
}

func TestGitTextRejectsOversizedCommandOutput(t *testing.T) {
	isolateGitConfig(t)
	root, _ := committedRepository(t, "repo")
	large := bytes.Repeat([]byte("private payload\n"), 1<<14)
	if err := os.WriteFile(filepath.Join(root, "large"), large, 0o600); err != nil {
		t.Fatalf("WriteFile(large) error = %v", err)
	}
	runGit(t, root, "add", "large")
	runGit(t, root, "-c", "user.name=Gitmeta Test", "-c", "user.email=gitmeta@example.invalid", "-c", "commit.gpgSign=false", "commit", "--quiet", "--no-verify", "-m", "large")

	if got, ok := gitText(context.Background(), 64, "-C", root, "show", "HEAD:large"); ok || got != "" {
		t.Fatalf("gitText() = %q, %v, want empty false for oversized output", got, ok)
	}
}

func TestPresenceWriterDoesNotRetainStatusPayload(t *testing.T) {
	typeOfWriter := reflect.TypeOf(presenceWriter{})
	if typeOfWriter.NumField() != 1 || typeOfWriter.Field(0).Type.Kind() != reflect.Bool {
		t.Fatalf("presenceWriter fields retain more than the presence bit: %v", typeOfWriter)
	}

	writer := presenceWriter{}
	privateOutput := bytes.Repeat([]byte("private/file/name\x00"), 1<<14)
	written, err := writer.Write(privateOutput)
	if err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if written != len(privateOutput) || !writer.present {
		t.Fatalf("Write() = %d, present = %v", written, writer.present)
	}
}

func initializedRepository(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir(repository) error = %v", err)
	}
	runGit(t, root, "init", "--quiet")
	return canonicalPath(t, root)
}

func committedRepository(t *testing.T, name string) (string, string) {
	t.Helper()
	root := initializedRepository(t, name)
	if err := os.WriteFile(filepath.Join(root, "tracked"), []byte("tracked\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(tracked) error = %v", err)
	}
	runGit(t, root, "add", "tracked")
	runGit(t, root, "-c", "user.name=Gitmeta Test", "-c", "user.email=gitmeta@example.invalid", "-c", "commit.gpgSign=false", "commit", "--quiet", "--no-verify", "-m", "initial")
	head := commandLine(runGit(t, root, "rev-parse", "--verify", "HEAD"))
	return root, head
}

func runGit(t *testing.T, directory string, args ...string) []byte {
	t.Helper()
	commandArgs := append([]string{"-C", directory}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func commandLine(output []byte) string {
	return strings.TrimSuffix(strings.TrimSuffix(string(output), "\n"), "\r")
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("Abs(%q) error = %v", path, err)
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q) error = %v", absolute, err)
	}
	return canonical
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatalf("Stat(%q) error = %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", path)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
