//go:build windows

package pathresolve_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pathresolve"
)

// mkdirJunction creates an NTFS directory junction at link pointing to target.
// A junction is the reparse point form os.Lstat reports as neither a symlink
// nor a directory, so it exercises resolution that filepath.EvalSymlinks
// cannot walk through. Tests are skipped when the host refuses to create one.
func mkdirJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Skipf("cannot create directory junction: %v: %s", err, out)
	}
}

func TestEvalSymlinksTraversesJunction(t *testing.T) {
	target := t.TempDir()
	nested := filepath.Join(target, "nested", "leaf")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	link := filepath.Join(t.TempDir(), "junction")
	mkdirJunction(t, link, target)

	got, err := pathresolve.EvalSymlinks(filepath.Join(link, "nested", "leaf"))
	require.NoError(t, err)
	// The junction resolves to target and filepath.EvalSymlinks still
	// canonicalizes the result, so compare against the resolved target rather
	// than the raw TempDir path, which Windows may report in 8.3 form.
	want, err := filepath.EvalSymlinks(nested)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestEvalSymlinksResolvesTrailingJunction pins the resolution to the file
// location rather than to where in the path the junction sits. Plain
// filepath.EvalSymlinks leaves a trailing junction unresolved and fails below
// one, so the two spellings of one directory would otherwise disagree.
func TestEvalSymlinksResolvesTrailingJunction(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	mkdirJunction(t, link, target)

	got, err := pathresolve.EvalSymlinks(link)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestEvalSymlinksJunctionAboveMissingElement(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "junction")
	mkdirJunction(t, link, target)

	got, err := pathresolve.EvalSymlinks(filepath.Join(link, "missing"))
	require.Error(t, err)
	assert.Empty(t, got)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// TestEvalSymlinksRootedSymlinkTarget covers a symlink whose target is rooted
// but has no volume, such as `\shared`. Windows resolves it on the link's
// volume, so a same-named directory below the link's own directory is a decoy
// the result must not land on.
func TestEvalSymlinksRootedSymlinkTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.Mkdir(target, 0o755))
	rooted := target[len(filepath.VolumeName(target)):]

	linkDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(linkDir, rooted), 0o755))
	link := filepath.Join(linkDir, "link")
	if err := os.Symlink(rooted, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	got, err := pathresolve.EvalSymlinks(link)
	require.NoError(t, err)
	want, err := filepath.EvalSymlinks(target)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestEvalSymlinksReparsePointCycle covers two junctions that refer to each
// other. The walk is bounded, so the call returns instead of spinning, and it
// returns what filepath.EvalSymlinks reports for the path it was given rather
// than a partial rewrite.
func TestEvalSymlinksReparsePointCycle(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	mkdirJunction(t, first, second)
	mkdirJunction(t, second, first)

	want, err := filepath.EvalSymlinks(first)
	require.NoError(t, err)

	got, err := pathresolve.EvalSymlinks(first)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	got, err = pathresolve.EvalSymlinks(filepath.Join(first, "missing"))
	require.Error(t, err)
	assert.Empty(t, got)
}
