// Package gitrepo contains repository and ref helpers built on git.
//
// The functions in this package shell out through git/cmd's defensive Runner,
// so inherited git environment variables and global config do not silently
// affect repository discovery, ref checks, or worktree inspection.
package gitrepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	gitcmd "go.kenn.io/kit/git/cmd"
)

var runner = gitcmd.New()

var hexRefPattern = regexp.MustCompile(`\A[0-9A-Fa-f]+\z`)

// NormalizePath converts git output paths to native paths, including MSYS
// /c/Users style paths on Windows.
func NormalizePath(path string) string {
	path = strings.TrimSpace(path)
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' {
		if ((path[1] >= 'a' && path[1] <= 'z') || (path[1] >= 'A' && path[1] <= 'Z')) && path[2] == '/' {
			path = strings.ToUpper(string(path[1])) + ":" + path[2:]
		}
	}
	return filepath.FromSlash(path)
}

// Root returns the top-level checkout root for path.
func Root(ctx context.Context, path string) (string, error) {
	out, err := runner.Output(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	return NormalizePath(string(out)), nil
}

// GitDir returns the absolute path to the checkout's git directory.
func GitDir(ctx context.Context, path string) (string, error) {
	out, err := runner.Output(ctx, path, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-dir: %w", err)
	}
	gitDir := NormalizePath(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(path, gitDir)
	}
	return filepath.Clean(gitDir), nil
}

// MainRoot returns the main repository root, resolving through linked worktrees.
func MainRoot(ctx context.Context, path string) (string, error) {
	gitDirOut, err := runner.Output(ctx, path, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-dir: %w", err)
	}
	commonDirOut, err := runner.Output(ctx, path, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	gitDir := absGitPath(path, NormalizePath(string(gitDirOut)))
	commonDir := absGitPath(path, NormalizePath(string(commonDirOut)))
	if gitDir != commonDir {
		bareOut, err := runner.Output(ctx, "", "config", "--file", filepath.Join(commonDir, "config"), "--bool", "core.bare")
		if err == nil && strings.TrimSpace(string(bareOut)) == "true" {
			return Root(ctx, path)
		}
		if filepath.Base(commonDir) == ".git" {
			return filepath.Dir(commonDir), nil
		}
		out, err := runner.Output(ctx, "", "config", "--file", filepath.Join(commonDir, "config"), "core.worktree")
		if err != nil {
			return "", fmt.Errorf("git config core.worktree for submodule worktree: %w", err)
		}
		worktree := NormalizePath(string(out))
		if !filepath.IsAbs(worktree) {
			worktree = filepath.Join(commonDir, worktree)
		}
		return filepath.Clean(worktree), nil
	}
	return Root(ctx, path)
}

func absGitPath(base, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	return filepath.Clean(path)
}

// CurrentBranch returns the current local branch, or "" for detached HEAD.
func CurrentBranch(ctx context.Context, path string) string {
	out, err := runner.Output(ctx, path, "symbolic-ref", "HEAD")
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(out))
	return strings.TrimPrefix(ref, "refs/heads/")
}

// IsUnbornHead reports whether path is a repo whose HEAD points at a missing
// branch ref. Corrupt refs and non-repos return false.
func IsUnbornHead(ctx context.Context, path string) bool {
	out, err := runner.Output(ctx, path, "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return false
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" {
		return false
	}
	return runner.Command(ctx, path, "rev-parse", "--verify", ref).Run() != nil
}

// RefExists reports whether fullRef resolves in path.
func RefExists(ctx context.Context, path, fullRef string) bool {
	return runner.Command(ctx, path, "rev-parse", "--verify", "--quiet", fullRef).Run() == nil
}

// Resolve resolves ref to a SHA.
func Resolve(ctx context.Context, path, ref string) (string, error) {
	out, err := runner.Output(ctx, path, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// IsAncestor reports whether ancestor is reachable from descendant.
func IsAncestor(ctx context.Context, path, ancestor, descendant string) (bool, error) {
	_, _, err := runner.Run(ctx, path, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	if gitcmd.IsExitCode(err, 1) {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

// HasUncommittedChanges reports whether status --porcelain has any output.
func HasUncommittedChanges(ctx context.Context, path string) (bool, error) {
	out, err := runner.Output(ctx, path, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// WorktreePathForBranch returns the existing checkout path for branch when it
// is checked out in a non-stale worktree.
func WorktreePathForBranch(ctx context.Context, repoPath, branch string) (string, bool, error) {
	if branch == "" {
		return repoPath, true, nil
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := runner.Output(opCtx, repoPath, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, fmt.Errorf("git worktree list: %w", err)
	}
	type entry struct {
		path, branch string
	}
	var entries []entry
	var currentPath, currentBranch string
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentPath = NormalizePath(path)
			currentBranch = ""
		} else if ref, ok := strings.CutPrefix(line, "branch "); ok {
			currentBranch = strings.TrimPrefix(ref, "refs/heads/")
		} else if line == "" && currentPath != "" {
			if currentBranch != "" {
				entries = append(entries, entry{currentPath, currentBranch})
			}
			currentPath, currentBranch = "", ""
		}
	}
	if currentPath != "" && currentBranch != "" {
		entries = append(entries, entry{currentPath, currentBranch})
	}
	for _, e := range entries {
		if e.branch == branch {
			if _, err := os.Stat(e.path); err == nil {
				return e.path, true, nil
			}
		}
	}
	return repoPath, false, nil
}

// HooksPath returns the effective hooks directory, resolving relative paths
// against the main repository root so linked worktrees share hooks correctly.
//
// The exception is a relative core.hooksPath naming a directory tracked by
// git, such as a committed .githooks. Git resolves it from the root of the
// worktree running the hook, so each worktree uses the copy on its own branch.
func HooksPath(ctx context.Context, repoPath string) (string, error) {
	out, err := runner.Output(ctx, repoPath, "rev-parse", "--git-path", "hooks")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-path hooks: %w", err)
	}
	hooksPath := NormalizePath(string(out))
	if filepath.IsAbs(hooksPath) {
		return hooksPath, nil
	}
	// git reports the path relative to repoPath, which may be a
	// subdirectory; rebase it onto the worktree root.
	prefix, err := runner.Output(ctx, repoPath, "rev-parse", "--show-prefix")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-prefix: %w", err)
	}
	hooksPath = filepath.Join(NormalizePath(string(prefix)), hooksPath)
	if trackedHooksPath(ctx, repoPath) {
		root, err := Root(ctx, repoPath)
		if err != nil {
			return "", fmt.Errorf("resolve worktree root for hooks path: %w", err)
		}
		return filepath.Join(root, hooksPath), nil
	}
	root, err := MainRoot(ctx, repoPath)
	if err != nil {
		return "", fmt.Errorf("resolve main repo root for hooks path: %w", err)
	}
	return filepath.Join(root, hooksPath), nil
}

// EnsureAbsoluteHooksPath rewrites relative core.hooksPath to an absolute path
// rooted at the main repository, avoiding broken hooks in linked worktrees.
//
// A relative path naming a directory tracked by git is left alone. Its hooks
// are branch content and every worktree already runs its own checked-out
// copy; anchoring the shared value would make linked worktrees run the main
// checkout's hooks instead.
func EnsureAbsoluteHooksPath(ctx context.Context, repoPath string) error {
	out, err := runner.Output(ctx, repoPath, "config", "core.hooksPath")
	if err != nil {
		return nil //nolint:nilerr // git exits non-zero when core.hooksPath is unset, which means nothing to rewrite
	}
	raw := NormalizePath(string(out))
	if raw == "" || filepath.IsAbs(raw) || isGitTildePath(raw) {
		return nil
	}
	if trackedHooksPath(ctx, repoPath) {
		return nil
	}
	root, err := MainRoot(ctx, repoPath)
	if err != nil {
		return fmt.Errorf("resolve main repo root: %w", err)
	}
	if _, err := runner.Output(ctx, repoPath, "config", "--local", "core.hooksPath", filepath.Join(root, raw)); err != nil {
		return fmt.Errorf("update core.hooksPath to absolute: %w", err)
	}
	return nil
}

// trackedHooksPath reports whether core.hooksPath is a relative path naming a
// directory with files tracked in repoPath's worktree. Paths under .git or
// outside the working tree are never tracked.
func trackedHooksPath(ctx context.Context, repoPath string) bool {
	out, err := runner.Output(ctx, repoPath, "config", "core.hooksPath")
	if err != nil {
		return false
	}
	raw := NormalizePath(string(out))
	if raw == "" || filepath.IsAbs(raw) || isGitTildePath(raw) {
		return false
	}
	root, err := Root(ctx, repoPath)
	if err != nil {
		return false
	}
	// Git resolves a relative hooks path from the worktree root.
	rel := filepath.ToSlash(filepath.Clean(raw))
	files, err := runner.Output(ctx, root, "ls-files", "-z", "--", ":(literal)"+rel)
	return err == nil && len(files) > 0
}

func isGitTildePath(s string) bool {
	if s == "" || s[0] != '~' {
		return false
	}
	if len(s) == 1 {
		return true
	}
	c := s[1]
	if c == '/' || c == filepath.Separator {
		return true
	}
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// DefaultBranch detects origin/HEAD, then local main/master.
func DefaultBranch(ctx context.Context, repoPath string) (string, error) {
	out, err := runner.Output(ctx, repoPath, "symbolic-ref", "refs/remotes/origin/HEAD")
	if err == nil {
		branch := strings.TrimPrefix(strings.TrimSpace(string(out)), "refs/remotes/origin/")
		if branch != "" && RefExists(ctx, repoPath, "refs/remotes/origin/"+branch) {
			return "origin/" + branch, nil
		}
		if branch != "" && RefExists(ctx, repoPath, "refs/heads/"+branch) {
			return branch, nil
		}
	}
	for _, branch := range []string{"main", "master"} {
		if RefExists(ctx, repoPath, "refs/heads/"+branch) {
			return branch, nil
		}
	}
	return "", errors.New("could not detect default branch")
}

// ListRemotes returns configured remote names.
func ListRemotes(ctx context.Context, repoPath string) ([]string, error) {
	out, err := runner.Output(ctx, repoPath, "remote")
	if err != nil {
		return nil, fmt.Errorf("git remote: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// StripRemotePrefix strips the longest configured remote prefix from ref.
func StripRemotePrefix(ctx context.Context, repoPath, ref string) string {
	if !strings.Contains(ref, "/") {
		return ref
	}
	remotes, err := ListRemotes(ctx, repoPath)
	if err != nil {
		return ref
	}
	sort.Slice(remotes, func(i, j int) bool { return len(remotes[i]) > len(remotes[j]) })
	for _, remote := range remotes {
		if stripped, ok := strings.CutPrefix(ref, remote+"/"); ok {
			return stripped
		}
	}
	return ref
}

// ShortSHA returns git's default seven-character abbreviation when possible.
func ShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// LooksLikeSHA reports whether s looks like a 7-40 character hex commit id.
func LooksLikeSHA(s string) bool {
	return len(s) >= 7 && len(s) <= 40 && hexRefPattern.MatchString(s)
}
