package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestVisualPreviewPublicationIsExactVersionAndIdempotent(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "photo.raw", fakeHash("11"), 12, "image/x-raw")
	require.NoError(t, err)
	canonical := readyVisualPreview(t, node.BlobHash, fakeHash("22"), 9)
	physical := &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9}

	first, err := s.PublishVisualPreview(t.Context(), node.CurrentVersionID, canonical, physical)
	require.NoError(t, err)
	retry, err := s.PublishVisualPreview(t.Context(), node.CurrentVersionID, canonical, physical)
	require.NoError(t, err)
	assert.Equal(t, first.GenerationID, retry.GenerationID)

	view, err := s.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewReady, view.Generation.Preview.State)
	assert.Equal(t, fakeHash("22"), view.Generation.Preview.Output.BlobSHA256)

	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	for _, blob := range unreachable {
		assert.NotEqual(t, fakeHash("22"), blob.Hash)
	}

	changed := readyVisualPreview(t, node.BlobHash, fakeHash("23"), 9)
	_, err = s.PublishVisualPreview(t.Context(), node.CurrentVersionID, changed, physical)
	require.ErrorContains(t, err, "already has a different result")
	_, err = s.BlobInfo(t.Context(), fakeHash("23"))
	require.ErrorIs(t, err, ErrNotFound, "conflicting output membership must roll back")
}

func TestVisualPreviewMetadataRoundTrip(t *testing.T) {
	source := newTestStore(t)
	node, err := source.CreateFile(t.Context(), source.RootID(), "image.jpg", fakeHash("31"), 12, "image/jpeg")
	require.NoError(t, err)
	canonical := readyVisualPreview(t, node.BlobHash, fakeHash("32"), 7)
	_, err = source.PublishVisualPreview(t.Context(), node.CurrentVersionID, canonical,
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 7})
	require.NoError(t, err)

	var exported bytes.Buffer
	snapshot, err := source.BeginMetadataSnapshot(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.ExportBackup(t.Context(), &exported))
	require.NoError(t, snapshot.Close())
	target := newTestStore(t)
	require.NoError(t, target.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	view, err := target.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, fakeHash("32"), view.Generation.Preview.Output.BlobSHA256)
}

func TestVisualPreviewRecordsDeterministicFailureWithoutBlobAuthority(t *testing.T) {
	s := newTestStore(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "notes.txt", fakeHash("41"), 5, "text/plain")
	require.NoError(t, err)
	recipe := visualPreviewRecipe()
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
		ContractVersion: document.VisualPreviewContractV1, SourceSHA256: node.BlobHash,
		Recipe: recipe, State: document.VisualPreviewUnsupported,
		Failure: &document.VisualPreviewFailureV1{Code: "unsupported_media_type", Detail: "text/plain is not visual media"},
	})
	require.NoError(t, err)
	_, err = s.PublishVisualPreview(t.Context(), node.CurrentVersionID, canonical, nil)
	require.NoError(t, err)
	view, err := s.ContentVersionVisualPreview(t.Context(), node.CurrentVersionID)
	require.NoError(t, err)
	assert.Equal(t, document.VisualPreviewUnsupported, view.Generation.Preview.State)
}

func TestVisualPreviewLifecycleFollowsExactContentVersion(t *testing.T) {
	s := newTestStore(t)
	created, err := s.CreateFile(t.Context(), s.RootID(), "image.jpg", fakeHash("61"), 6, "image/jpeg")
	require.NoError(t, err)
	canonical := readyVisualPreview(t, created.BlobHash, fakeHash("62"), 8)
	_, err = s.PublishVisualPreview(t.Context(), created.CurrentVersionID, canonical,
		&BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 8})
	require.NoError(t, err)
	priorVersionID := created.CurrentVersionID
	updated, _, err := s.ReplaceContent(t.Context(), created.ID, created.Revision,
		fakeHash("63"), 7, "image/jpeg")
	require.NoError(t, err)
	_, err = s.PruneContentVersions(t.Context(), created.ID, updated.Revision,
		VersionPruneSelector{VersionIDs: []string{priorVersionID}}, true)
	require.NoError(t, err)

	_, err = s.ContentVersionVisualPreview(t.Context(), priorVersionID)
	require.ErrorIs(t, err, ErrNotFound)
	unreachable, err := s.UnreachableBlobs(t.Context())
	require.NoError(t, err)
	assert.Contains(t, unreachable, BlobInfo{Hash: fakeHash("62"), Size: 8})
}

func readyVisualPreview(t *testing.T, source, output string, size int64) []byte {
	t.Helper()
	canonical, _, err := document.MarshalVisualPreviewV1(document.VisualPreviewV1{
		ContractVersion: document.VisualPreviewContractV1, SourceSHA256: source,
		Recipe: visualPreviewRecipe(), State: document.VisualPreviewReady,
		Output: &document.VisualPreviewOutputV1{BlobSHA256: output, Size: size,
			MediaType: "image/jpeg", Width: 1600, Height: 900},
	})
	require.NoError(t, err)
	return canonical
}

func visualPreviewRecipe() document.VisualPreviewRecipeV1 {
	return document.VisualPreviewRecipeV1{ContractVersion: document.VisualPreviewContractV1,
		MaxEdgePixels: 2048, OutputMediaType: "image/jpeg", OrientationPolicy: "apply",
		ColorPolicy: "srgb", FramePolicy: "primary", ProcessorFingerprint: fakeHash("51")}
}
