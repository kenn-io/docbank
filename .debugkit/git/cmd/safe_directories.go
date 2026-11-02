package gitcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/kit/internal/contextio"
)

type safeDirectoryScope struct {
	paths       []string
	fingerprint [32]byte
	values      []string
	// valid reports that fingerprint and values describe the scope's file.
	valid bool
	// readWithGit makes the next read ask Git directly, because the scope has
	// includes or its last read failed.
	readWithGit bool
}

type safeDirectoryCache struct {
	mu      sync.Mutex
	current *safeDirectorySnapshot
}

// Admission bounds unfinished trust-cache filesystem checks to one per process.
var safeDirectoryFilesystem = &safeDirectoryFilesystemAdmission{}

type safeDirectoryFilesystemAdmission struct {
	mu         sync.Mutex
	done       chan struct{}
	ownerEnded <-chan struct{}
}

// evaluateFilesystem runs evaluate under the process-wide admission and a
// probe timeout. It returns false when the check was abandoned; evaluate may
// then still be running, so callers must not read anything it writes.
func evaluateFilesystem(ctx context.Context, evaluate func(context.Context)) bool {
	ctx, cancel := context.WithTimeout(ctx, safeDirectoryProbeTimeout)
	defer cancel()
	admission := safeDirectoryFilesystem
	for ctx.Err() == nil {
		admission.mu.Lock()
		if ctx.Err() != nil {
			admission.mu.Unlock()
			break
		}
		pending, ended := admission.done, admission.ownerEnded
		if pending == nil {
			done := make(chan struct{})
			admission.done, admission.ownerEnded = done, ctx.Done()
			admission.mu.Unlock()
			go func() {
				evaluate(ctx)
				admission.mu.Lock()
				admission.done, admission.ownerEnded = nil, nil
				close(done)
				admission.mu.Unlock()
			}()
			select {
			case <-done:
				return ctx.Err() == nil
			case <-ctx.Done():
				return false
			}
		}
		admission.mu.Unlock()
		select {
		case <-pending:
			continue
		case <-ended:
			select {
			case <-pending:
				continue
			default:
				return false
			}
		case <-ctx.Done():
			return false
		}
	}
	return false
}

type safeDirectorySnapshot struct {
	identity   [32]byte
	executable os.FileInfo
	scopes     map[string]safeDirectoryScope
}

// trustObservation is the per-call view of what a cached snapshot depends on.
type trustObservation struct {
	identity       [32]byte
	executable     os.FileInfo
	sameExecutable bool
	homeAvailable  bool
	homeApplicable bool
}

func safeDirectoryScopes(env []string) []string {
	if gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
		return []string{"global"}
	}
	return []string{"system", "global"}
}

func configPathsReusable(env []string) bool {
	if _, set := envValue(env, "GIT_CONFIG"); set {
		return false
	}
	if !gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
		system, _ := envValue(env, "GIT_CONFIG_SYSTEM")
		if strings.ContainsAny(system, "\r\n") {
			return false
		}
	}
	global, override := envValue(env, "GIT_CONFIG_GLOBAL")
	paths := []string{global}
	if !override {
		home, _ := envValue(env, "HOME")
		xdg, _ := envValue(env, "XDG_CONFIG_HOME")
		paths = []string{home, xdg}
	}
	for _, path := range paths {
		if strings.ContainsAny(path, "\r\n") || hasParentComponent(path) {
			return false
		}
	}
	return true
}

func hasParentComponent(path string) bool {
	isSeparator := func(r rune) bool {
		return r == '/' || os.PathSeparator == '\\' && r == '\\'
	}
	return slices.Contains(strings.FieldsFunc(path, isSeparator), "..")
}

func isLineBreak(r rune) bool {
	return r == '\n' || r == '\r'
}

func windowsHomeAvailable(ctx context.Context, env []string) (available, applicable bool) {
	_, homeSet := envValue(env, "HOME")
	_, globalSet := envValue(env, "GIT_CONFIG_GLOBAL")
	drive, driveSet := envValue(env, "HOMEDRIVE")
	path, pathSet := envValue(env, "HOMEPATH")
	if runtime.GOOS != "windows" || homeSet || globalSet || !driveSet || !pathSet {
		return false, false
	}
	if ctx.Err() != nil {
		return false, true
	}
	info, err := os.Stat(drive + path)
	if err != nil {
		return false, true
	}
	return info.IsDir(), true
}

// read reuses include-free scopes while their root bytes stay unchanged;
// includes always run Git.
func (c *safeDirectoryCache) read(ctx context.Context, env []string, dir string) []string {
	if c == nil || !configPathsReusable(env) {
		return readSafeDirectories(ctx, env, dir)
	}
	c.mu.Lock()
	previous := c.current
	c.mu.Unlock()
	observed, ok := observeTrust(ctx, env, previous)
	if !ok {
		return readSafeDirectories(ctx, env, dir)
	}
	next := &safeDirectorySnapshot{identity: observed.identity, executable: observed.executable}
	var values []string
	if previous == nil {
		values = readSafeDirectories(ctx, env, dir)
	} else {
		next.scopes, values, ok = reuseSafeDirectoryScopes(ctx, env, dir, observed, previous)
		if !ok {
			return readSafeDirectories(ctx, env, dir)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if !observed.homeUnchanged(ctx, env) {
		return readSafeDirectories(ctx, env, dir)
	}
	c.mu.Lock()
	if c.current == previous && ctx.Err() == nil {
		c.current = next
	}
	c.mu.Unlock()
	return values
}

// reuseSafeDirectoryScopes reads every scope through the previous snapshot. It
// returns false when the snapshot does not apply or a check was abandoned.
func reuseSafeDirectoryScopes(
	ctx context.Context,
	env []string,
	dir string,
	observed trustObservation,
	previous *safeDirectorySnapshot,
) (map[string]safeDirectoryScope, []string, bool) {
	if observed.identity != previous.identity || !observed.sameExecutable {
		return nil, nil, false
	}
	scopes, ok := nextSafeDirectoryScopes(ctx, env, dir, observed, previous)
	if !ok {
		return nil, nil, false
	}
	var values []string
	for _, scope := range safeDirectoryScopes(env) {
		var entries []string
		scopes[scope], entries, ok = readCachedScope(ctx, env, dir, scope, scopes[scope])
		if !ok {
			return nil, nil, false
		}
		values = append(values, entries...)
	}
	return scopes, values, true
}

// observeTrust identifies the Git executable, environment, and Windows home
// selection that cached scopes depend on.
func observeTrust(
	ctx context.Context, env []string, previous *safeDirectorySnapshot,
) (trustObservation, bool) {
	var observed trustObservation
	var path string
	var err error
	if !evaluateFilesystem(ctx, func(checkCtx context.Context) {
		if checkCtx.Err() != nil {
			return
		}
		path = gitCommand(checkCtx, true).Path
		if checkCtx.Err() != nil {
			return
		}
		observed.executable, err = os.Stat(path)
		if checkCtx.Err() != nil || err != nil {
			return
		}
		observed.homeAvailable, observed.homeApplicable = windowsHomeAvailable(checkCtx, env)
		if checkCtx.Err() == nil && previous != nil {
			observed.sameExecutable = os.SameFile(observed.executable, previous.executable)
		}
	}) || err != nil || observed.executable == nil {
		return trustObservation{}, false
	}
	info := observed.executable
	observed.identity = sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s\x00%t",
		path, info.Size(), info.ModTime().UnixNano(), strings.Join(env, "\x00"), observed.homeAvailable))
	return observed, true
}

func (o trustObservation) homeUnchanged(ctx context.Context, env []string) bool {
	if !o.homeApplicable {
		return true
	}
	var available bool
	return evaluateFilesystem(ctx, func(checkCtx context.Context) {
		available, _ = windowsHomeAvailable(checkCtx, env)
	}) && available == o.homeAvailable
}

// nextSafeDirectoryScopes copies the previous scopes, discovering their file
// paths with git var on the first reuse.
func nextSafeDirectoryScopes(
	ctx context.Context,
	env []string,
	dir string,
	observed trustObservation,
	previous *safeDirectorySnapshot,
) (map[string]safeDirectoryScope, bool) {
	if previous.scopes != nil {
		return maps.Clone(previous.scopes), true
	}
	scopes := make(map[string]safeDirectoryScope)
	for _, scope := range safeDirectoryScopes(env) {
		out, err := safeDirectoryOutput(ctx, env, dir, "var", "GIT_CONFIG_"+strings.ToUpper(scope))
		if err != nil {
			return nil, false
		}
		paths := strings.FieldsFunc(string(out), isLineBreak)
		if scope == "global" && observed.homeApplicable &&
			!discoveredWindowsHome(env, observed.homeAvailable, paths) {
			return nil, false
		}
		scopes[scope] = safeDirectoryScope{paths: paths}
	}
	return scopes, true
}

// discoveredWindowsHome reports whether Git chose the home directory that
// Git for Windows selects from HOMEDRIVE/HOMEPATH or USERPROFILE.
func discoveredWindowsHome(env []string, homeAvailable bool, paths []string) bool {
	home, _ := envValue(env, "USERPROFILE")
	if homeAvailable {
		drive, _ := envValue(env, "HOMEDRIVE")
		path, _ := envValue(env, "HOMEPATH")
		home = drive + path
	}
	expected := filepath.Join(home, ".gitconfig")
	return filepath.IsAbs(expected) && len(paths) == 2 &&
		filepath.ToSlash(filepath.Clean(paths[1])) == filepath.ToSlash(filepath.Clean(expected))
}

// readCachedScope returns the scope's next cache state and its entries. It
// returns false when the filesystem check was abandoned.
func readCachedScope(
	ctx context.Context, env []string, dir, scope string, previous safeDirectoryScope,
) (safeDirectoryScope, []string, bool) {
	if !previous.readWithGit {
		var cached safeDirectoryScope
		var err error
		if !evaluateFilesystem(ctx, func(checkCtx context.Context) {
			cached, err = readSafeDirectorySnapshot(checkCtx, env, dir, scope, previous)
		}) {
			return safeDirectoryScope{}, nil, false
		}
		if err == nil && !cached.readWithGit {
			return cached, cached.values, true
		}
	}
	entries, includes, err := readSafeDirectoryScope(ctx, env, dir, scope)
	if err != nil && !IsExitCode(err, 1) {
		return safeDirectoryScope{paths: previous.paths, readWithGit: true}, nil, true
	}
	return safeDirectoryScope{paths: previous.paths, readWithGit: includes}, entries, true
}

const safeDirectoryKeys = `^(safe\.directory|include\.path|includeif\..*\.path)$`

func readSafeDirectoryScope(
	ctx context.Context, env []string, dir, scope string,
) ([]string, bool, error) {
	out, err := safeDirectoryOutput(ctx, env, dir,
		"config", "--"+scope, "--includes", "-z", "--get-regexp", safeDirectoryKeys)
	entries, includes := decodeSafeDirectoryOutput(out)
	return entries, includes, err
}

func decodeSafeDirectoryOutput(out []byte) ([]string, bool) {
	var entries []string
	includes := false
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		if key == "" {
			continue
		}
		if key == "safe.directory" {
			entries = append(entries, value)
		} else {
			includes = true
		}
	}
	return entries, includes
}

func safeDirectoryOutput(
	ctx context.Context, env []string, dir string, args ...string,
) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, safeDirectoryProbeTimeout)
	defer cancel()
	cmd := gitCommand(ctx, true, args...)
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.Output()
	if err != nil {
		return out, &GitError{Err: err}
	}
	return out, nil
}

func readSafeDirectorySnapshot(
	ctx context.Context, env []string, dir, scope string, previous safeDirectoryScope,
) (safeDirectoryScope, error) {
	next := safeDirectoryScope{paths: previous.paths}
	expected := 1
	if _, override := envValue(env, "GIT_CONFIG_GLOBAL"); scope == "global" && !override {
		expected = 2
	}
	if len(previous.paths) != expected {
		return next, errors.ErrUnsupported
	}
	var file *os.File
	var path string
	for _, candidate := range slices.Backward(previous.paths) {
		if err := ctx.Err(); err != nil {
			return next, err
		}
		path = candidate
		if !filepath.IsAbs(path) {
			return next, errors.ErrUnsupported
		}
		info, err := os.Stat(path)
		if ctx.Err() != nil {
			return next, ctx.Err()
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return next, err
		}
		if !info.Mode().IsRegular() {
			return next, fmt.Errorf("%s: not a regular file", path)
		}
		file, err = os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return next, err
		}
		break
	}
	if file == nil {
		next.valid = ctx.Err() == nil
		return next, ctx.Err()
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, &contextio.Reader{Context: ctx, Reader: file})
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	if err != nil {
		return next, err
	}
	fingerprint := fingerprintSafeDirectoryBytes(path, size, hash.Sum(nil))
	if previous.valid && previous.fingerprint == fingerprint {
		return previous, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return next, err
	}
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	parseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(parseCtx, true,
		"config", "--no-includes", "--file", "-", "-z", "--get-regexp", safeDirectoryKeys)
	cmd.Env, cmd.Dir = env, dir
	var output bytes.Buffer
	cmd.Stdout = &output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return next, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return next, err
	}
	hash.Reset()
	size, copyErr := io.Copy(stdin, io.TeeReader(&contextio.Reader{Context: ctx, Reader: file}, hash))
	copyErr = errors.Join(copyErr, file.Close(), stdin.Close())
	if copyErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	if copyErr != nil {
		return next, copyErr
	}
	if waitErr != nil && !IsExitCode(&GitError{Err: waitErr}, 1) {
		return next, waitErr
	}
	next.fingerprint = fingerprintSafeDirectoryBytes(path, size, hash.Sum(nil))
	next.values, next.readWithGit = decodeSafeDirectoryOutput(output.Bytes())
	next.valid = true
	return next, nil
}

func fingerprintSafeDirectoryBytes(path string, size int64, digest []byte) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%x", path, size, digest)))
}
