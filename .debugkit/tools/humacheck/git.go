package humacheck

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	gitenv "go.kenn.io/kit/git/env"
)

func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	// Bind git to dir rather than to a GIT_DIR/GIT_INDEX_FILE inherited from
	// a parent hook, so the checker always judges the repository it was asked about.
	cmd.Env = append(gitenv.StripInherited(os.Environ()), "GIT_TERMINAL_PROMPT=0")
	return cmd
}

// gitTopLevel returns the repository root containing dir, or ok=false when
// dir is not inside a Git work tree. Other Git failures and cancellation
// are returned as errors.
func gitTopLevel(ctx context.Context, dir string) (root string, ok bool, err error) {
	cmd := gitCommand(ctx, dir, "rev-parse", "--show-toplevel")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		if _, isExit := errors.AsType[*exec.ExitError](err); isExit && strings.Contains(stderr.String(), "not a git repository") {
			return "", false, nil
		}
		if errors.Is(err, exec.ErrNotFound) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git rev-parse --show-toplevel: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return filepath.Clean(strings.TrimSpace(string(out))), true, nil
}

// trackedFiles lists the files in the Git index relative to root with
// forward slashes. The index is authoritative so staged files count on
// pre-commit.
func trackedFiles(ctx context.Context, root string) ([]string, error) {
	cmd := gitCommand(ctx, root, "ls-files", "-z", "--cached")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for name := range bytes.SplitSeq(out, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		files = append(files, string(name))
	}
	slices.Sort(files)
	return files, nil
}

// unstagedChanges lists tracked files whose working-tree copy differs from
// the index, including files deleted from the working tree.
func unstagedChanges(ctx context.Context, root string) ([]string, error) {
	cmd := gitCommand(ctx, root, "diff-files", "--name-only", "-z")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("git diff-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for name := range bytes.SplitSeq(out, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		files = append(files, string(name))
	}
	slices.Sort(files)
	return files, nil
}

// walkFiles enumerates a directory tree for the non-repository fallback.
func walkFiles(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && excludedDir(rel+"/x") {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	slices.Sort(files)
	return files, nil
}

// indexFS serves file contents from the Git index through one long-lived
// `git cat-file --batch` process, so repository rules see exactly what a
// commit would contain rather than the working tree.
type indexFS struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	err    error
}

func newIndexFS(ctx context.Context, root string) (*indexFS, error) {
	cmd := gitCommand(ctx, root, "cat-file", "--batch")
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("git cat-file --batch: %w", err)
	}
	return &indexFS{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}, nil
}

// Open reads the index blob for name (repo-relative, forward slashes).
func (x *indexFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return nil, x.err
	}
	if _, err := fmt.Fprintf(x.stdin, ":%s\n", name); err != nil {
		x.err = fmt.Errorf("git cat-file request: %w", err)
		return nil, x.err
	}
	header, err := x.stdout.ReadString('\n')
	if err != nil {
		x.err = fmt.Errorf("git cat-file response: %w", err)
		return nil, x.err
	}
	fields := strings.Fields(header)
	if len(fields) == 2 && fields[1] == "missing" {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if len(fields) != 3 {
		x.err = fmt.Errorf("git cat-file: unexpected header %q", strings.TrimSpace(header))
		return nil, x.err
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		x.err = fmt.Errorf("git cat-file: bad size in %q", strings.TrimSpace(header))
		return nil, x.err
	}
	// Whole blobs are materialized: the rules read entire files, and the
	// batch protocol has no way to skip a body.
	data := make([]byte, size+1) // trailing newline
	if _, err := io.ReadFull(x.stdout, data); err != nil {
		x.err = fmt.Errorf("git cat-file body: %w", err)
		return nil, x.err
	}
	return &memFile{name: name, reader: bytes.NewReader(data[:size]), size: size}, nil
}

// Close ends the cat-file process.
func (x *indexFS) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	_ = x.stdin.Close()
	return x.cmd.Wait()
}

type memFile struct {
	name   string
	reader *bytes.Reader
	size   int64
}

func (f *memFile) Read(p []byte) (int, error) { return f.reader.Read(p) }
func (f *memFile) Close() error               { return nil }
func (f *memFile) Stat() (fs.FileInfo, error) { return f, nil }

func (f *memFile) Name() string       { return filepath.Base(f.name) }
func (f *memFile) Size() int64        { return f.size }
func (f *memFile) Mode() fs.FileMode  { return 0o644 }
func (f *memFile) ModTime() time.Time { return time.Time{} }
func (f *memFile) IsDir() bool        { return false }
func (f *memFile) Sys() any           { return nil }
