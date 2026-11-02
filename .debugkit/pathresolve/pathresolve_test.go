package pathresolve_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/pathresolve"
)

func TestEvalSymlinksMatchesFilepath(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0o755))

	paths := []string{dir, filepath.Join(dir, "a"), nested, filepath.Join(dir, "a", ".")}
	for _, path := range paths {
		want, err := filepath.EvalSymlinks(path)
		require.NoError(t, err)

		got, err := pathresolve.EvalSymlinks(path)
		require.NoError(t, err)
		assert.Equal(t, want, got, "resolving %s", path)
	}
}

func TestEvalSymlinksMissingPathReportsFilepathError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")

	_, wantErr := filepath.EvalSymlinks(missing)
	require.Error(t, wantErr)

	got, err := pathresolve.EvalSymlinks(missing)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Equal(t, wantErr, err)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestEvalSymlinksNonDirectoryElementFails(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	got, err := pathresolve.EvalSymlinks(filepath.Join(file, "child"))
	require.Error(t, err)
	assert.Empty(t, got)
}

func TestEvalSymlinksAllowMissingFollowsLinkChain(t *testing.T) {
	dir := t.TempDir()
	canonical, err := pathresolve.EvalSymlinks(dir)
	require.NoError(t, err)
	target := filepath.Join(dir, "missing", "nested")
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	_, err = fslink.LinkDir(target, first)
	require.NoError(t, err)
	_, err = fslink.LinkDir(first, second)
	require.NoError(t, err)
	for _, path := range []string{target, first, second} {
		got, err := pathresolve.EvalSymlinksAllowMissing(filepath.Join(path, "config"))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(canonical, "missing", "nested", "config"), got)
	}
	assert.NoDirExists(t, filepath.Join(dir, "missing"))
}

func TestEvalSymlinksAllowMissingRejectsInvalidPaths(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for _, suffix := range []string{"/missing", "/../missing"} {
		got, err := pathresolve.EvalSymlinksAllowMissing(file + suffix)
		require.Error(t, err)
		assert.Empty(t, got)
	}
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	_, err := fslink.LinkDir(second, first)
	require.NoError(t, err)
	_, err = fslink.LinkDir(first, second)
	require.NoError(t, err)
	for _, path := range []string{first, filepath.Join(first, "config")} {
		got, err := pathresolve.EvalSymlinksAllowMissing(path)
		require.Error(t, err)
		assert.Empty(t, got)
	}
}

func TestEvalSymlinksResolvesLinksBeforeParent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "destination", "nested")
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "destination", "config"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"), nil, 0o600))
	link := filepath.Join(dir, "route")
	_, err := fslink.LinkDir(target, link)
	require.NoError(t, err)
	canonical, err := pathresolve.EvalSymlinks(dir)
	require.NoError(t, err)
	want := filepath.Join(canonical, "destination", "config")
	// Keep the parent component intact so resolution must traverse route first.
	raw := link + string(filepath.Separator) + ".." + string(filepath.Separator) + "config"
	got, err := pathresolve.EvalSymlinksAllowMissing(raw)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	t.Chdir(dir)
	got, err = pathresolve.EvalSymlinksAllowMissing("route/../config")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	got, err = pathresolve.EvalSymlinks(raw)
	require.NoError(t, err)
	strictWant := want
	if runtime.GOOS == "windows" {
		strictWant = filepath.Join(canonical, "config")
	}
	assert.Equal(t, strictWant, got)
	// The public resolver must identify the file that the OS actually opens.
	rawInfo, err := os.Stat(raw)
	require.NoError(t, err)
	resolvedInfo, err := os.Stat(got)
	require.NoError(t, err)
	assert.True(t, os.SameFile(rawInfo, resolvedInfo), "raw path and resolved path must identify the same file")
	for name, target := range map[string]string{"absolute": raw, "relative": "route/../config"} {
		t.Run(name, func(t *testing.T) {
			if runtime.GOOS == "windows" && name == "absolute" {
				// Windows cleans absolute link targets at creation. Put the
				// parent traversal in a relative target behind the absolute link.
				intermediate := filepath.Join(dir, "intermediate")
				if err := os.Symlink("route/../config", intermediate); err != nil {
					t.Skipf("cannot create file symlink: %v", err)
				}
				t.Cleanup(func() { require.NoError(t, os.Remove(intermediate)) })
				target = intermediate
			}
			outer := filepath.Join(dir, "outer")
			if err := os.Symlink(target, outer); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("cannot create file symlink: %v", err)
				}
				require.NoError(t, err)
			}
			t.Cleanup(func() { require.NoError(t, os.Remove(outer)) })
			got, err := pathresolve.EvalSymlinksAllowMissing(outer)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

func TestEvalSymlinksAllowMissingRejectsParentAfterMissing(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "route")
	_, err := fslink.LinkDir(filepath.Join(dir, "missing", "nested"), link)
	require.NoError(t, err)
	t.Chdir(dir)
	for _, path := range []string{"missing/../config", "route/../config", link + "/../config"} {
		got, err := pathresolve.EvalSymlinksAllowMissing(path)
		require.Error(t, err, "cannot resolve a parent through a missing component: %s", path)
		assert.Empty(t, got)
	}
}
