// Package gitmeta reads a small, bounded set of Git repository metadata.
package gitmeta

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	detectionTimeout = 2 * time.Second
	commandWaitDelay = 100 * time.Millisecond
	rootOutputLimit  = 64 * 1024
	headOutputLimit  = 128
)

// Metadata is the bounded Git state associated with a directory. The zero
// value means that no complete repository result was available. HeadSHA is
// empty for a valid repository with an unborn HEAD.
type Metadata struct {
	Root    string
	HeadSHA string
	Dirty   bool
}

// Detect returns repository root, HEAD commit ID, and dirty state for
// directory. It invokes Git directly, without a shell, and applies a short
// timeout to the complete detection operation. Outside a Git worktree, on
// cancellation, or when required metadata cannot be read safely, Detect
// returns the zero value.
//
// Git stderr is discarded. Stdout is either retained only in a tightly
// bounded collector for Root and HeadSHA, or reduced immediately to a boolean
// for dirty state.
func Detect(ctx context.Context, directory string) Metadata {
	ctx, cancel := context.WithTimeout(ctx, detectionTimeout)
	defer cancel()

	root, ok := gitText(
		ctx,
		rootOutputLimit,
		"-C", directory,
		"rev-parse", "--path-format=absolute", "--show-toplevel",
	)
	if !ok || root == "" || strings.IndexByte(root, 0) >= 0 {
		return Metadata{}
	}
	// Git normalizes --path-format=absolute output to forward slashes on
	// Windows. Convert it back to the host representation before using it as a
	// filesystem path or returning it to callers.
	root = filepath.Clean(filepath.FromSlash(root))

	head, headOK := gitText(
		ctx,
		headOutputLimit,
		"-C", root,
		"rev-parse", "--verify", "HEAD^{commit}",
	)
	if !headOK {
		if ctx.Err() != nil || !unbornHEAD(ctx, root) {
			return Metadata{}
		}
		head = ""
	} else if !validObjectID(head) {
		return Metadata{}
	}

	dirty, ok := gitDirty(ctx, root)
	if !ok {
		// Never misreport an indeterminate repository as clean.
		return Metadata{}
	}

	return Metadata{
		Root:    root,
		HeadSHA: head,
		Dirty:   dirty,
	}
}

func gitText(ctx context.Context, limit int, args ...string) (string, bool) {
	output := boundedOutput{remaining: limit}
	cmd := gitCommand(ctx, args...)
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil || output.overflow {
		return "", false
	}
	return trimLineEnding(string(output.data)), true
}

func gitDirty(ctx context.Context, root string) (bool, bool) {
	output := presenceWriter{}
	cmd := gitCommand(
		ctx,
		"-C", root,
		"status", "--porcelain=v1", "-z", "--untracked-files=normal",
	)
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return false, false
	}
	return output.present, true
}

func unbornHEAD(ctx context.Context, root string) bool {
	ref, ok := gitText(
		ctx,
		rootOutputLimit,
		"-C", root,
		"symbolic-ref", "--quiet", "--no-recurse", "HEAD",
	)
	return ok && strings.HasPrefix(ref, "refs/heads/") && len(ref) > len("refs/heads/")
}

func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	commandArgs := make([]string, 0, len(args)+3)
	commandArgs = append(commandArgs, "--no-optional-locks", "-c", "core.fsmonitor=false")
	commandArgs = append(commandArgs, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = gitEnvironment()
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

func gitEnvironment() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if redirectsRepository(key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func redirectsRepository(key string) bool {
	switch strings.ToUpper(key) {
	case "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_COMMON_DIR",
		"GIT_DIR",
		"GIT_INDEX_FILE",
		"GIT_OBJECT_DIRECTORY",
		"GIT_WORK_TREE":
		return true
	default:
		return false
	}
}

type boundedOutput struct {
	data      []byte
	remaining int
	overflow  bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.data = append(w.data, p[:w.remaining]...)
		w.remaining = 0
		w.overflow = true
		return len(p), nil
	}
	w.data = append(w.data, p...)
	w.remaining -= len(p)
	return len(p), nil
}

type presenceWriter struct {
	present bool
}

func (w *presenceWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.present = true
	}
	return len(p), nil
}

func trimLineEnding(value string) string {
	value = strings.TrimSuffix(value, "\n")
	if runtime.GOOS == "windows" {
		value = strings.TrimSuffix(value, "\r")
	}
	return value
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
