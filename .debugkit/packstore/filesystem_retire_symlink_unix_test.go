//go:build unix

package packstore

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestFilesystemBackendRetireRejectsSymlinkedLooseShard(t *testing.T) {
	require := require.New(t)
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	content := []byte("outside loose object")
	hash := hashForTest(content)
	external := t.TempDir()
	externalPath := filepath.Join(external, hash.String())
	require.NoError(os.WriteFile(externalPath, content, 0o600))
	require.NoError(os.Symlink(external, filepath.Join(backend.Layout().Root(), hash.String()[:2])))

	err := backend.Retire(t.Context(), ObjectRef{
		LooseHash: hash, LooseEncoding: LooseEncodingRaw,
	})

	require.ErrorContains(err, "unsafe filesystem directory")
	got, readErr := os.ReadFile(externalPath)
	require.NoError(readErr)
	assert.Equal(t, content, got)
}

func TestFilesystemBackendRetireRejectsSymlinkedPackShard(t *testing.T) {
	require := require.New(t)
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	packID := pack.NewPackID()
	external := t.TempDir()
	externalPath := filepath.Join(external, packID+PackExt)
	require.NoError(os.WriteFile(externalPath, []byte("outside pack"), 0o600))
	require.NoError(os.Mkdir(filepath.Join(backend.Layout().Root(), "packs"), 0o700))
	require.NoError(os.Symlink(external, filepath.Join(
		backend.Layout().Root(), "packs", packID[:2],
	)))

	err := backend.Retire(t.Context(), ObjectRef{PackID: packID})

	require.ErrorContains(err, "unsafe filesystem directory")
	got, readErr := os.ReadFile(externalPath)
	require.NoError(readErr)
	assert.Equal(t, []byte("outside pack"), got)
}

func TestFilesystemBackendPublishPackRejectsSymlinkedPackDirectory(t *testing.T) {
	require := require.New(t)
	backend := attachedFilesystemBackend(t, "archive", "epoch-1")
	external := t.TempDir()
	require.NoError(os.Symlink(external, filepath.Join(backend.Layout().Root(), "packs")))
	path, packID, _ := buildBackendPackSource(t, []byte("confined pack publication"))
	source, err := os.Open(path)
	require.NoError(err)

	_, err = backend.PublishPack(t.Context(), packID, source, PublishOptions{})
	require.NoError(source.Close())

	require.ErrorContains(err, "unsafe filesystem directory")
	entries, readErr := os.ReadDir(external)
	require.NoError(readErr)
	assert.Empty(t, entries)
}

func TestFilesystemBackendPublishLooseStaysBoundToOwnedSymlinkRoot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	base := t.TempDir()
	ownedRoot := filepath.Join(base, "owned")
	foreignRoot := filepath.Join(base, "foreign")
	require.NoError(os.Mkdir(ownedRoot, 0o700))
	require.NoError(os.Mkdir(foreignRoot, 0o700))
	link := filepath.Join(base, "store")
	require.NoError(os.Symlink(ownedRoot, link))
	linkedLayout, err := NewLayout(link, LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	backend, err := NewFilesystemBackend(linkedLayout, FilesystemBackendOptions{})
	require.NoError(err)
	t.Cleanup(func() { require.NoError(backend.Close()) })
	owner := Ownership{
		Format: OwnershipFormatV1, Vault: "test-vault", Store: "archive", Epoch: "epoch-1",
	}
	require.NoError(backend.ReplaceOwnership(t.Context(), owner, nil))

	content := []byte("pinned ownership namespace")
	hash := hashForTest(content)
	source := &swapRootReader{
		Reader: bytes.NewReader(content), link: link, replacement: foreignRoot,
	}
	_, err = backend.PublishLoose(
		t.Context(), hash, source,
		PublishOptions{ExpectedSize: int64(len(content)), SizeKnown: true},
	)

	require.NoError(err)
	ownedLayout, err := NewLayout(ownedRoot, LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	foreignLayout, err := NewLayout(foreignRoot, LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	assert.FileExists(ownedLayout.LoosePath(hash))
	assert.NoFileExists(foreignLayout.LoosePath(hash))
}

type swapRootReader struct {
	*bytes.Reader
	link        string
	replacement string
	swapped     bool
}

func (r *swapRootReader) Read(buffer []byte) (int, error) {
	if !r.swapped {
		r.swapped = true
		next := r.link + ".next"
		if err := os.Symlink(r.replacement, next); err != nil {
			return 0, err
		}
		if err := os.Rename(next, r.link); err != nil {
			return 0, err
		}
	}
	return r.Reader.Read(buffer)
}
