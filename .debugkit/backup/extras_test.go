package backup

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

// msgvaultTokensSpec mirrors the token-capture layout the msgvault
// application uses, exercising the sensitive-dir and sensitive-glob paths.
func msgvaultTokensSpec() ExtrasSpec {
	return ExtrasSpec{
		Dirs:  []ExtrasDirSpec{{Name: "tokens", Sensitive: true}},
		Globs: []ExtrasGlobSpec{{Pattern: "client_secret*.json", Sensitive: true}},
	}
}

func TestCaptureExtrasDeletionsAndConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions", "sub"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"a":1}`), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "sub", "b.json"), []byte(`{"b":2}`), 0o600))
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("x = 1\n"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	captured, err := CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir,
		Spec: ExtrasSpec{
			Dirs:  []ExtrasDirSpec{{Name: "deletions"}},
			Files: []ExtrasFileSpec{{Path: cfgPath, RecordAs: "config.toml", Sensitive: true}},
		},
		AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.NotEmpty(captured.Tree)

	packs, entries, err := appender.Finish()
	require.NoError(err)
	require.NotEmpty(packs)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeID, err := pack.ParseBlobID(captured.Tree)
	require.NoError(err)
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	require.Len(tree.Entries, 3)
	assert.Equal("config.toml", tree.Entries[0].Path)
	assert.Equal("deletions/a.json", tree.Entries[1].Path)
	assert.Equal("deletions/sub/b.json", tree.Entries[2].Path)
	for _, e := range tree.Entries {
		blob, parseErr := pack.ParseBlobID(e.Blob)
		require.NoError(parseErr)
		content, readErr := r.ReadBlob(known, blob, nil, testPackExt)
		require.NoError(readErr)
		assert.Equal(e.Size, int64(len(content)), e.Path)
	}
}

// TestOpenExtrasNoFollowRejectsSymlinks pins that extras reads never resolve
// a symlink at any path component, even one whose target stays inside the
// root: extras bytes carry no expected hash, so a followed link would
// silently capture another in-DataDir file (tokens, client secrets) under a
// deletions path. The capture walk rejects pre-existing links; this reader
// is the guard for links raced in after the walk, so it must reject them
// independently.
func TestOpenExtrasNoFollowRejectsSymlinks(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dir, "real"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dir, "real", "f.json"), []byte("ok"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dir, "secret.json"), []byte("token bytes"), 0o600))
	if err := os.Symlink(filepath.Join(dir, "secret.json"), filepath.Join(dir, "real", "link.json")); err != nil {
		t.Skip("symlinks not supported on this platform")
	}
	require.NoError(os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "dirlink")))

	root, err := os.OpenRoot(dir)
	require.NoError(err)
	defer func() { _ = root.Close() }()

	file, _, err := openExtrasNoFollow(root, filepath.Join("real", "f.json"))
	require.NoError(err)
	content, err := io.ReadAll(file)
	require.NoError(err)
	require.NoError(file.Close())
	require.Equal([]byte("ok"), content)

	// A symlink leaf inside the root must be refused, not followed to its
	// in-root target.
	_, _, err = openExtrasNoFollow(root, filepath.Join("real", "link.json"))
	require.ErrorContains(err, "not a regular file")

	// A symlinked directory component inside the root must be refused too.
	_, _, err = openExtrasNoFollow(root, filepath.Join("dirlink", "f.json"))
	require.ErrorContains(err, "not a real directory")
}

// TestCaptureExtrasRejectsCaseCollidingPaths pins capture/restore parity for
// record paths: restore refuses trees whose entries fold to one file on a
// case-insensitive filesystem (checkExtrasCollisions), so capture must refuse
// to produce such a tree rather than write a snapshot that verifies its way
// into an unrestorable archive. Files specs carry arbitrary record paths, so
// the collision is provoked without needing a case-sensitive filesystem.
func TestCaptureExtrasRejectsCaseCollidingPaths(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	b := filepath.Join(dir, "b.toml")
	require.NoError(os.WriteFile(a, []byte("x = 1\n"), 0o600))
	require.NoError(os.WriteFile(b, []byte("x = 2\n"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, err := CaptureExtras(t.Context(), ExtrasOptions{
		Spec: ExtrasSpec{Files: []ExtrasFileSpec{
			{Path: a, RecordAs: "config.toml"},
			{Path: b, RecordAs: "Config.TOML"},
		}},
	}, appender)
	require.ErrorContains(err, "collide")

	// An exact duplicate keeps its own, more precise message.
	_, err = CaptureExtras(t.Context(), ExtrasOptions{
		Spec: ExtrasSpec{Files: []ExtrasFileSpec{
			{Path: a, RecordAs: "config.toml"},
			{Path: b, RecordAs: "config.toml"},
		}},
	}, appender)
	require.ErrorContains(err, "selected more than once")
}

// TestCaptureExtrasRejectsReservedRecordPaths pins capture/restore parity
// for the reserved and Windows-aliasing path rules: restore refuses extras
// entries naming the database, its SQLite sidecars, the content tree, or any
// component ending in a dot or space, so capture must refuse to record such
// paths rather than publish a snapshot verify and restore then reject.
func TestCaptureExtrasRejectsReservedRecordPaths(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.json")
	require.NoError(os.WriteFile(src, []byte("{}"), 0o600))

	for path, wantErr := range map[string]string{
		"app.db":         "overlaps restored archive content",
		"APP.DB-journal": "overlaps restored archive content",
		"content/x.json": "overlaps restored archive content",
		"safe./x.json":   "component ending in a dot or space",
	} {
		appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
		_, err := CaptureExtras(t.Context(), ExtrasOptions{
			Spec:           ExtrasSpec{Files: []ExtrasFileSpec{{Path: src, RecordAs: path}}},
			ContentDirName: "content",
			DBFileName:     "app.db",
		}, appender)
		appender.Abort()
		require.ErrorContains(err, wantErr, "record path %q", path)
	}
}

func TestCaptureExtrasEmpty(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	captured, err := CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: t.TempDir(), Spec: ExtrasSpec{Dirs: []ExtrasDirSpec{{Name: "deletions"}}},
	}, appender)
	require.NoError(err)
	require.Empty(captured.Tree)

	// An empty spec selects nothing, whatever DataDir holds.
	captured, err = CaptureExtras(t.Context(), ExtrasOptions{DataDir: t.TempDir()}, appender)
	require.NoError(err)
	require.Empty(captured.Tree)
}

func TestCaptureExtrasTokensWithoutDataDir(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	// A client_secret file in the process's cwd must not be globbed (and the
	// absent data root must not be dereferenced) when no DataDir is set.
	cwd := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(cwd, "client_secret_x.json"), []byte("{}"), 0o600))
	t.Chdir(cwd)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	captured, err := CaptureExtras(t.Context(), ExtrasOptions{
		Spec:                  msgvaultTokensSpec(),
		AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.Empty(captured.Tree)
}

func TestCaptureExtrasTokensGuard(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "tokens"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "tokens", "t.json"), []byte("{}"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "client_secret_web.json"), []byte("{}"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, err := CaptureExtras(t.Context(), ExtrasOptions{DataDir: dataDir, Spec: msgvaultTokensSpec()}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "tokens")

	// A sensitive Files spec (config.toml carries API keys) fires the same
	// guard and names the record path it tripped on.
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("[server]\napi_key = \"secret\"\n"), 0o600))
	cfgSpec := ExtrasSpec{Files: []ExtrasFileSpec{{Path: cfgPath, RecordAs: "config.toml", Sensitive: true}}}
	_, err = CaptureExtras(t.Context(), ExtrasOptions{DataDir: dataDir, Spec: cfgSpec}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "config.toml")

	_, err = CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir, Spec: cfgSpec, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)

	captured, err := CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir, Spec: msgvaultTokensSpec(), AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.NotEmpty(captured.Tree)

	_, entries, err := appender.Finish()
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeID, err := pack.ParseBlobID(captured.Tree)
	require.NoError(err)
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	var paths []string
	for _, e := range tree.Entries {
		paths = append(paths, e.Path)
	}
	require.Equal([]string{"client_secret_web.json", "tokens/t.json"}, paths)
}

// TestCaptureExtrasConfigSymlinkEscapeRefused pins the confined read for the
// config file: config.toml is read through a root at its own directory, so a
// symlink swapped in at ConfigPath that points outside that directory is
// refused rather than followed to an arbitrary host file. Before the fix the
// plain os.ReadFile followed it and embedded the target's bytes.
func TestCaptureExtrasConfigSymlinkEscapeRefused(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	require.NoError(os.WriteFile(secret, []byte("TOP SECRET config"), 0o600))

	cfgPath := filepath.Join(dataDir, "config.toml")
	if err := os.Symlink(secret, cfgPath); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, err := CaptureExtras(t.Context(), ExtrasOptions{
		DataDir:               dataDir,
		Spec:                  ExtrasSpec{Files: []ExtrasFileSpec{{Path: cfgPath, RecordAs: "config.toml", Sensitive: true}}},
		AllowPlaintextSecrets: true,
	}, appender)
	require.Error(err)

	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "TOP SECRET config")
	}
}

func TestCaptureExtrasRejectsSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a target file outside the data dir
	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	// Create a symlink inside deletions pointing to the outside file
	deletionsDir := filepath.Join(dataDir, "deletions")
	require.NoError(os.MkdirAll(deletionsDir, 0o700))
	symlinkPath := filepath.Join(deletionsDir, "link.txt")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		// Skip if symlinks are not supported on this platform
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, err = CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir, Spec: ExtrasSpec{Dirs: []ExtrasDirSpec{{Name: "deletions"}}},
	}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "deletions/link.txt")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "secret content")
	}
}

// TestCaptureExtrasRejectsGlobbedSymlinks pins the fix extending the walk's
// symlink rejection to filepath.Glob's client_secret*.json results, which
// never went through addDir's filepath.WalkDir callback and so bypassed the
// os.ReadFile-follows-symlinks hazard TestCaptureExtrasRejectsSymlinks
// covers for the deletions/tokens walk.
func TestCaptureExtrasRejectsGlobbedSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	symlinkPath := filepath.Join(dataDir, "client_secret_evil.json")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, err = CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir, Spec: msgvaultTokensSpec(), AllowPlaintextSecrets: true,
	}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "client_secret_evil.json")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded.
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "secret content")
	}
}

// TestCaptureExtrasTokensDataDirWithGlobMetacharacters pins that client-secret
// capture works when DataDir's path contains glob metacharacters. The old
// filepath.Glob(filepath.Join(DataDir, "client_secret*.json")) treated the
// bracket in "data[1]" as a character class, so the join never matched and the
// secret was silently dropped from the backup. Matching by basename through the
// confined root captures it.
func TestCaptureExtrasTokensDataDirWithGlobMetacharacters(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	// "data[1]" is a legal directory name on every platform but a glob
	// character class as a pattern prefix.
	dataDir := filepath.Join(t.TempDir(), "data[1]")
	require.NoError(os.MkdirAll(dataDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "client_secret_web.json"), []byte("{}"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	captured, err := CaptureExtras(t.Context(), ExtrasOptions{
		DataDir: dataDir, Spec: msgvaultTokensSpec(), AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.NotEmpty(captured.Tree)

	_, entries, err := appender.Finish()
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeID, err := pack.ParseBlobID(captured.Tree)
	require.NoError(err)
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	var paths []string
	for _, e := range tree.Entries {
		paths = append(paths, e.Path)
	}
	require.Equal([]string{"client_secret_web.json"}, paths)
}
