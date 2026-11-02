//go:build unix || windows

package atomicfile_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/fslink"
)

func TestWriteFileCreatesAndReplacesWithoutLeavingStagingFiles(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	require.NoError(atomicfile.WriteFile(path, []byte("first")))
	require.Equal("first", readString(t, path))

	require.NoError(atomicfile.WriteFile(path, []byte("second")))
	assert.Equal(t, "second", readString(t, path))
	assert.Equal(t, []string{"config.json"}, entryNames(t, dir))
}

func TestWriteFileDoesNotCreateParentDirectories(t *testing.T) {
	dir := t.TempDir()

	err := atomicfile.WriteFile(filepath.Join(dir, "missing", "file"), []byte("data"))

	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Empty(t, entryNames(t, dir))
}

func TestWriteFileRefusesDirectoryTarget(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(os.Mkdir(target, 0o700))
	writeString(t, target, "inner", "kept")

	err := atomicfile.WriteFile(target, []byte("data"))

	require.Error(err)
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
	assert.Equal(t, "kept", readString(t, filepath.Join(target, "inner")))
}

func TestCommitRemovesStagingFileWhenTargetBecomesDirectory(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	file, err := atomicfile.Create(target)
	require.NoError(err)
	_, err = file.WriteString("data")
	require.NoError(err)
	require.NoError(os.Mkdir(target, 0o700))

	err = file.Commit()

	require.Error(err)
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
	assert.NoError(t, file.Abort())
}

func TestCreateAbortLeavesTargetAndDirectoryUnchanged(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := writeString(t, dir, "target", "old")
	file, err := atomicfile.Create(target)
	require.NoError(err)
	assert.Equal(t, target, file.Name())
	// atomicfile stages in the resolved directory, which can be spelled
	// differently from dir (/private/var on macOS, RUNNER~1 on Windows).
	dirInfo, err := os.Stat(dir)
	require.NoError(err)
	stagingInfo, err := os.Stat(filepath.Dir(file.TempName()))
	require.NoError(err)
	require.True(os.SameFile(dirInfo, stagingInfo), "staged in %s, want %s", file.TempName(), dir)
	require.True(strings.HasPrefix(filepath.Base(file.TempName()), ".target.tmp-"), file.TempName())
	_, err = file.WriteString("new")
	require.NoError(err)

	require.NoError(file.Abort())

	assert.Equal(t, "old", readString(t, target))
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
	require.ErrorContains(file.Commit(), "already committed or aborted")
	assert.Equal(t, "old", readString(t, target))
}

func TestCommitThenAbortKeepsCommittedContent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	file, err := atomicfile.Create(target)
	require.NoError(err)
	_, err = file.ReadFrom(strings.NewReader("streamed"))
	require.NoError(err)

	require.NoError(file.Commit())
	require.NoError(file.Abort())

	assert.Equal(t, "streamed", readString(t, target))
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
	require.ErrorContains(file.Commit(), "already committed or aborted")
}

func TestWithStagingDirStagesThereAndPublishesAtTarget(t *testing.T) {
	require := require.New(t)
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	targetDir := filepath.Join(root, "out")
	require.NoError(os.Mkdir(staging, 0o700))
	require.NoError(os.Mkdir(targetDir, 0o700))
	target := filepath.Join(targetDir, "target")
	file, err := atomicfile.Create(target, atomicfile.WithStagingDir(staging))
	require.NoError(err)
	defer func() { _ = file.Abort() }()
	require.Equal(staging, filepath.Dir(file.TempName()))
	_, err = file.WriteString("data")
	require.NoError(err)

	require.NoError(file.Commit())

	assert.Equal(t, "data", readString(t, target))
	assert.Empty(t, entryNames(t, staging))
}

func TestWithPrivateRejectsPermissionOptions(t *testing.T) {
	for name, opt := range map[string]atomicfile.Option{
		"WithPerm":         atomicfile.WithPerm(0o600),
		"WithPreserveMode": atomicfile.WithPreserveMode(),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()

			err := atomicfile.WriteFile(filepath.Join(dir, "target"), []byte("data"), atomicfile.WithPrivate(), opt)

			require.Error(t, err)
			assert.Empty(t, entryNames(t, dir))
		})
	}
}

func TestWriteFileRefusesFinalSymlinkByDefault(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	realFile := writeString(t, dir, "real", "original")
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, "real", link)

	err := atomicfile.WriteFile(link, []byte("new"))

	require.ErrorIs(err, fslink.ErrIsLink)
	kind, err := fslink.Classify(link)
	require.NoError(err)
	assert.Equal(t, fslink.Symlink, kind)
	assert.Equal(t, "original", readString(t, realFile))
	assert.ElementsMatch(t, []string{"link", "real"}, entryNames(t, dir))
}

func TestCommitRefusesSymlinkCreatedAfterCreate(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	writeString(t, dir, "real", "original")
	link := filepath.Join(dir, "link")
	file, err := atomicfile.Create(link)
	require.NoError(err)
	_, err = file.WriteString("new")
	require.NoError(err)
	symlinkOrSkip(t, "real", link)

	err = file.Commit()

	require.ErrorIs(err, fslink.ErrIsLink)
	dest, err := os.Readlink(link)
	require.NoError(err)
	assert.Equal(t, "real", dest)
	assert.Equal(t, "original", readString(t, filepath.Join(dir, "real")))
	assert.ElementsMatch(t, []string{"link", "real"}, entryNames(t, dir))
}

func TestWriteFileWithFollowLinkWritesThroughSymlinkChain(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.Mkdir(filepath.Join(dir, "data"), 0o700))
	realFile := writeString(t, filepath.Join(dir, "data"), "real", "original")
	symlinkOrSkip(t, "real", filepath.Join(dir, "data", "inner"))
	outer := filepath.Join(dir, "outer")
	symlinkOrSkip(t, filepath.Join("data", "inner"), outer)

	require.NoError(atomicfile.WriteFile(outer, []byte("new"), atomicfile.WithFollowLink()))

	assert.Equal(t, "new", readString(t, realFile))
	for _, link := range []string{outer, filepath.Join(dir, "data", "inner")} {
		kind, err := fslink.Classify(link)
		require.NoError(err)
		assert.Equal(t, fslink.Symlink, kind, link)
	}
	assert.ElementsMatch(t, []string{"inner", "real"}, entryNames(t, filepath.Join(dir, "data")))
}

// A relative ".." destination climbs from the link's real parent, not from
// the path the caller reached it through.
func TestWriteFileWithFollowLinkResolvesDotDotFromRealParent(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	home := filepath.Join(dir, "home")
	require.NoError(os.MkdirAll(filepath.Join(realDir, "cfg"), 0o700))
	require.NoError(os.MkdirAll(filepath.Join(realDir, "shared"), 0o700))
	require.NoError(os.MkdirAll(filepath.Join(home, "shared"), 0o700))
	realFile := writeString(t, filepath.Join(realDir, "shared"), "s.json", "old")
	symlinkOrSkip(t, filepath.Join("..", "shared", "s.json"), filepath.Join(realDir, "cfg", "s.json"))
	_, err := fslink.LinkDir(filepath.Join(realDir, "cfg"), filepath.Join(home, "cfg"))
	require.NoError(err)

	require.NoError(atomicfile.WriteFile(filepath.Join(home, "cfg", "s.json"), []byte("new"), atomicfile.WithFollowLink()))

	assert.Equal(t, "new", readString(t, realFile))
	assert.Empty(t, entryNames(t, filepath.Join(home, "shared")))
	assert.ElementsMatch(t, []string{"s.json"}, entryNames(t, filepath.Join(realDir, "cfg")))
}

// Commit refuses to write the destination chosen at Create once the link
// has been retargeted, rather than writing a file the link no longer names.
func TestCommitWithFollowLinkRefusesRetargetedLink(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	a := writeString(t, dir, "a", "a-old")
	b := writeString(t, dir, "b", "b-old")
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, "a", link)
	file, err := atomicfile.Create(link, atomicfile.WithFollowLink())
	require.NoError(err)
	_, err = file.WriteString("new")
	require.NoError(err)
	require.NoError(os.Remove(link))
	symlinkOrSkip(t, "b", link)

	err = file.Commit()

	require.ErrorContains(err, "link changed")
	assert.Equal(t, "a-old", readString(t, a))
	assert.Equal(t, "b-old", readString(t, b))
	assert.ElementsMatch(t, []string{"a", "b", "link"}, entryNames(t, dir))
}

func TestWriteFileWithFollowLinkCreatesDanglingLinkTarget(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, "missing", link)

	require.NoError(atomicfile.WriteFile(link, []byte("new"), atomicfile.WithFollowLink()))

	assert.Equal(t, "new", readString(t, filepath.Join(dir, "missing")))
	kind, err := fslink.Classify(link)
	require.NoError(err)
	assert.Equal(t, fslink.Symlink, kind)
}

func TestWriteFileWithFollowLinkRejectsLinkCycle(t *testing.T) {
	dir := t.TempDir()
	symlinkOrSkip(t, "b", filepath.Join(dir, "a"))
	symlinkOrSkip(t, "a", filepath.Join(dir, "b"))

	err := atomicfile.WriteFile(filepath.Join(dir, "a"), []byte("new"), atomicfile.WithFollowLink())

	require.ErrorContains(t, err, "too many levels")
	assert.ElementsMatch(t, []string{"a", "b"}, entryNames(t, dir))
}

func TestWriteNewCreatesFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")

	require.NoError(t, atomicfile.WriteNew(target, []byte("data")))

	assert.Equal(t, "data", readString(t, target))
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
}

func TestWriteNewRefusesExistingFile(t *testing.T) {
	dir := t.TempDir()
	target := writeString(t, dir, "target", "old")

	err := atomicfile.WriteNew(target, []byte("new"))

	require.ErrorIs(t, err, fs.ErrExist)
	assert.Equal(t, "old", readString(t, target))
	assert.Equal(t, []string{"target"}, entryNames(t, dir))
}

func TestWriteNewRefusesDanglingSymlink(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	symlinkOrSkip(t, "missing", link)

	err := atomicfile.WriteNew(link, []byte("new"))

	require.ErrorIs(err, fs.ErrExist)
	dest, err := os.Readlink(link)
	require.NoError(err)
	assert.Equal(t, "missing", dest)
	assert.Equal(t, []string{"link"}, entryNames(t, dir))
}

func TestWriteNewRejectsFollowLink(t *testing.T) {
	dir := t.TempDir()

	err := atomicfile.WriteNew(filepath.Join(dir, "target"), []byte("data"), atomicfile.WithFollowLink())

	require.Error(t, err)
	assert.Empty(t, entryNames(t, dir))
}
