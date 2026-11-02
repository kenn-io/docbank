// Package gitworktree creates isolated git worktrees and captures patches.
//
// Temporary worktrees are created detached, with hooks disabled during
// creation, and cleaned up on partial failures. Submodule initialization keeps
// file:// protocol allowances limited to the top-level .gitmodules owned by
// the repository under test, avoiding recursive trust of nested .gitmodules.
package gitworktree

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// Worktree is a temporary linked worktree. Call Close when finished.
type Worktree struct {
	// Dir is the linked worktree directory.
	Dir string
	// Repo is the parent repository used for git worktree removal.
	Repo string
	// BaseSHA is the HEAD SHA resolved immediately after creation.
	BaseSHA string
	runner  gitcmd.Runner
}

// Options configures Create.
type Options struct {
	// ParentDir is passed to os.MkdirTemp for the worktree directory.
	ParentDir string
	// Prefix is passed to os.MkdirTemp. A package default is used when empty.
	Prefix string
	// InitSubmodules initializes submodules after worktree creation.
	InitSubmodules bool
	// PullLFS runs git lfs pull when git-lfs is installed for the checkout.
	PullLFS bool
	// Runner overrides the git runner. When zero, gitcmd.New is used.
	Runner gitcmd.Runner
}

// Create creates a detached worktree at ref. Hooks are disabled for the
// worktree add so user hooks do not run in internal automation checkouts.
func Create(ctx context.Context, repoPath, ref string, opts Options) (_ *Worktree, err error) {
	if ref == "" {
		return nil, errors.New("ref must not be empty")
	}
	runner := opts.Runner
	if runner.Env == nil {
		runner = gitcmd.New()
	}
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "kit-worktree-"
	}
	worktreeDir, err := os.MkdirTemp(opts.ParentDir, prefix)
	if err != nil {
		return nil, err
	}
	wt := &Worktree{Dir: worktreeDir, Repo: repoPath, runner: runner}
	defer func() {
		if err != nil {
			_ = wt.cleanup(context.Background())
		}
	}()

	addRunner := runner.WithConfig("core.hooksPath", os.DevNull)
	if _, _, err := addRunner.Run(ctx, repoPath, nil, "worktree", "add", "--detach", worktreeDir, ref); err != nil {
		_ = os.RemoveAll(worktreeDir)
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	if opts.InitSubmodules {
		if err := wt.InitSubmodules(ctx); err != nil {
			return nil, err
		}
	}
	if opts.PullLFS {
		wt.PullLFS(ctx)
	}
	wt.BaseSHA = wt.ResolveBaseSHA(ctx)
	return wt, nil
}

// Close removes the linked worktree and its directory.
func (w *Worktree) Close(ctx context.Context) error {
	return w.cleanup(ctx)
}

func (w *Worktree) cleanup(ctx context.Context) error {
	_, _, gitErr := w.runner.Run(ctx, w.Repo, nil, "worktree", "remove", "--force", w.Dir)
	rmErr := os.RemoveAll(w.Dir)
	if gitErr != nil {
		return gitErr
	}
	return rmErr
}

// ResolveBaseSHA resolves the worktree HEAD. It returns "" on failure.
func (w *Worktree) ResolveBaseSHA(ctx context.Context) string {
	out, err := w.runner.Output(ctx, w.Dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// PullLFS runs git lfs pull when git-lfs is available for the checkout.
func (w *Worktree) PullLFS(ctx context.Context) {
	if _, _, err := w.runner.Run(ctx, w.Dir, nil, "lfs", "env"); err == nil {
		_, _, _ = w.runner.Run(ctx, w.Dir, nil, "lfs", "pull")
	}
}

// InitSubmodules initializes submodules while limiting file:// protocol
// exposure. File protocol is enabled only for the top-level pass when the
// repository-owned .gitmodules requires it; recursive nested .gitmodules stay
// subject to git's default protocol policy.
func (w *Worktree) InitSubmodules(ctx context.Context) error {
	allowFileProtocol, err := UsesFileProtocolSubmodules(w.Dir)
	if err != nil {
		return fmt.Errorf("detect submodule protocol requirements: %w", err)
	}
	if err := w.submoduleUpdate(ctx, w.Dir, allowFileProtocol, false); err != nil {
		return err
	}
	paths, err := ListSubmodulePaths(ctx, w.runner, w.Dir)
	if err != nil || len(paths) == 0 {
		return err
	}
	for _, subPath := range paths {
		err := w.submoduleUpdate(ctx, filepath.Join(w.Dir, subPath), false, true)
		if err != nil && !IsFileProtocolError(err) {
			return err
		}
	}
	return nil
}

func (w *Worktree) submoduleUpdate(ctx context.Context, repoPath string, allowFileProtocol, recursive bool) error {
	runner := w.runner
	if allowFileProtocol {
		runner = runner.WithConfig("protocol.file.allow", "always")
	}
	args := []string{"submodule", "update", "--init"}
	if recursive {
		args = append(args, "--recursive")
	}
	if _, _, err := runner.Run(ctx, repoPath, nil, args...); err != nil {
		return fmt.Errorf("git submodule update: %w", err)
	}
	return nil
}

// ListSubmodulePaths returns registered submodule paths.
func ListSubmodulePaths(ctx context.Context, runner gitcmd.Runner, repoPath string) ([]string, error) {
	if runner.Env == nil {
		runner = gitcmd.New()
	}
	out, err := runner.Output(ctx, repoPath, "submodule", "foreach", "--quiet", "echo $sm_path")
	if err != nil {
		return nil, fmt.Errorf("git submodule foreach: %w", err)
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// IsFileProtocolError reports git's file transport denial.
func IsFileProtocolError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "transport 'file' not allowed") //nolint:kennlint // errtext: git stderr text is the only signal for this denial
}

// CapturePatch stages all changes and returns a binary patch against BaseSHA.
func (w *Worktree) CapturePatch(ctx context.Context) (string, error) {
	if _, _, err := w.runner.Run(ctx, w.Dir, nil, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add in worktree: %w", err)
	}
	if w.BaseSHA != "" {
		treeOut, err := w.runner.Output(ctx, w.Dir, "write-tree")
		if err == nil {
			tree := strings.TrimSpace(string(treeOut))
			diff, err := w.runner.Output(ctx, w.Dir, "diff-tree", "-p", "--binary", w.BaseSHA, tree)
			if err == nil && len(diff) > 0 {
				return string(diff), nil
			}
		}
	}
	diff, err := w.runner.Output(ctx, w.Dir, "diff", "--cached", "--binary")
	if err != nil {
		return "", fmt.Errorf("git diff in worktree: %w", err)
	}
	return string(diff), nil
}

// ApplyPatch applies a binary patch to repoPath.
func ApplyPatch(ctx context.Context, repoPath, patch string) error {
	return applyPatch(ctx, repoPath, patch, false)
}

// CheckPatch dry-runs patch application.
func CheckPatch(ctx context.Context, repoPath, patch string) error {
	return applyPatch(ctx, repoPath, patch, true)
}

// PatchConflictError indicates a patch does not apply cleanly.
type PatchConflictError struct {
	Detail string
}

func (e *PatchConflictError) Error() string {
	return "patch conflict: " + e.Detail
}

func applyPatch(ctx context.Context, repoPath, patch string, checkOnly bool) error {
	if patch == "" {
		return nil
	}
	args := []string{"apply"}
	if checkOnly {
		args = append(args, "--check")
	}
	args = append(args, "--binary", "-")
	_, stderr, err := gitcmd.New().Run(ctx, repoPath, strings.NewReader(patch), args...)
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(stderr))
	if checkOnly && isPatchConflict(msg) {
		return &PatchConflictError{Detail: msg}
	}
	if msg == "" {
		return err
	}
	if checkOnly {
		return fmt.Errorf("patch check failed: %s", msg)
	}
	return fmt.Errorf("git apply: %w", err)
}

func isPatchConflict(msg string) bool {
	return strings.Contains(msg, "patch failed") || strings.Contains(msg, "does not apply")
}

// UsesFileProtocolSubmodules scans .gitmodules files for local/file URLs.
func UsesFileProtocolSubmodules(repoPath string) (bool, error) {
	paths, err := findGitmodulesPaths(repoPath)
	if err != nil {
		return false, err
	}
	topLevel := filepath.Join(repoPath, ".gitmodules")
	for _, path := range paths {
		usesFile, err := gitmodulesUsesFileProtocol(path)
		if err != nil {
			if path == topLevel {
				return false, err
			}
			continue
		}
		if usesFile {
			return true, nil
		}
	}
	return false, nil
}

func gitmodulesUsesFileProtocol(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		url, ok := ParseGitmodulesURL(scanner.Text())
		if ok && IsFileProtocolURL(url) {
			return true, nil
		}
	}
	return false, scanner.Err()
}

// ParseGitmodulesURL parses a url = value line from .gitmodules.
func ParseGitmodulesURL(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
		return "", false
	}
	parts := strings.SplitN(line, "=", 2)
	if len(parts) != 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "url") {
		return "", false
	}
	url := strings.TrimSpace(parts[1])
	if unquoted, err := strconv.Unquote(url); err == nil {
		url = unquoted
	}
	return url, true
}

func findGitmodulesPaths(repoPath string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == repoPath {
				return err
			}
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == ".gitmodules" {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

// IsFileProtocolURL reports whether url names a local filesystem transport.
func IsFileProtocolURL(url string) bool {
	lower := strings.ToLower(url)
	if strings.HasPrefix(lower, "file:") {
		return true
	}
	if strings.HasPrefix(url, "/") || strings.HasPrefix(url, "./") || strings.HasPrefix(url, "../") {
		return true
	}
	if len(url) >= 2 && isAlpha(url[0]) && url[1] == ':' {
		return true
	}
	return strings.HasPrefix(url, `\\`)
}

func isAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
