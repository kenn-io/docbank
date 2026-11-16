package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func checkVectorReaderMemoTracksNativeWrites(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	for _, change := range []string{"embedding replace", "trash restore", "content replace", "purge", "photo excluded", "photo hidden", "photo detach"} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			var record EmbeddingSetRecord
			var node Node
			if change == "photo excluded" || change == "photo hidden" || change == "photo detach" {
				f := seedImageEmbeddingCatalog(t, s, document.EmbeddingInputOriginalFile, document.EmbeddingInputRenditionChunk, document.EmbeddingInputImage)
				record, _ = f.publish(t)
				node = f.node
			} else {
				s, record, _ = publishedVectorIndexFixture(t, s)
				version, err := s.ContentVersionByID(t.Context(), record.ContentVersionID)
				require.NoError(t, err)
				node, err = s.NodeByID(t.Context(), version.NodeID)
				require.NoError(t, err)
			}
			retained, err := s.CreateFile(t.Context(), s.RootID(), "retained-reader.pdf", hashVectorIndexTest("retained-reader"), 20, "application/pdf")
			require.NoError(t, err)
			binding := record.BindingID
			if record.InputKind == document.EmbeddingInputImage {
				binding = "required"
			}
			other := embeddingSetFixture(t.Context(), s, retained.CurrentVersionID, record.ProcessingProfileFingerprint, document.EmbeddingInputOriginalFile, binding, "")
			other.VectorSpace.Descriptor = record.VectorSpace.Descriptor
			require.Equal(t, record.VectorSpace.ID, other.VectorSpace.ID)
			require.NoError(t, s.StageEmbeddingSet(t.Context(), other))
			require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
				FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: other.ContentVersionID, BindingID: other.BindingID, InputKind: other.InputKind},
				SetID: other.ID, VectorSpaceID: other.VectorSpace.ID, ProcessingProfileFingerprint: other.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
			}))
			source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
			require.NoError(t, err)
			require.Len(t, source.Members, 2)
			generation := vectorIndexGenerationFixture("warm-reader", source, []byte("index"), time.Now().UTC())
			require.NoError(t, putVectorIndexGenerationForTest(t, s, generation))
			lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "warm", time.Now(), time.Minute)
			require.NoError(t, err)
			require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, time.Now()))
			switch change {
			case "embedding replace":
				replacement := record
				replacement.ID = hashVectorIndexTest("replacement")
				replacement.InputGeneration.ID = hashVectorIndexTest("replacement-input")
				require.NoError(t, s.StageEmbeddingSet(t.Context(), replacement))
				require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
					FencingToken: 2, Key: EmbeddingHeadKey{ContentVersionID: replacement.ContentVersionID, BindingID: replacement.BindingID, InputKind: replacement.InputKind},
					SetID: replacement.ID, VectorSpaceID: replacement.VectorSpace.ID, ProcessingProfileFingerprint: replacement.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
				}))
			case "trash restore":
				_, _, err = s.Trash(t.Context(), node.ID, UnconditionalRev)
			case "content replace":
				_, _, err = s.ReplaceContent(t.Context(), node.ID, UnconditionalRev, hashVectorIndexTest("reader-new-content"), 20, "application/pdf")
			case "purge":
				_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{record.ContentVersionID}})
			default:
				asset, loadErr := s.PhotoAssetForNode(t.Context(), node.ID)
				require.NoError(t, loadErr)
				switch change {
				case "photo excluded":
					_, err = s.SetPhotoAssetExcluded(t.Context(), asset.ID, asset.Revision, true)
				case "photo hidden":
					require.NoError(t, s.SetupPhotoHidden(t.Context(), "synthetic-passcode"))
					_, err = s.SetPhotoAssetHidden(t.Context(), asset.ID, asset.Revision, true)
				case "photo detach":
					_, err = s.DetachPhotoFile(t.Context(), asset.ID, asset.Revision, *asset.DisplayFileID, PhotoDetachOptions{})
				}
			}
			require.NoError(t, err)
			_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "stale", time.Now(), time.Minute)
			require.ErrorIs(t, err, ErrVectorIndexSourceStale)
			after, err := s.CaptureVectorIndexSource(t.Context(), source.VectorSpaceID)
			require.NoError(t, err)
			require.NotEqual(t, source.ManifestChecksum, after.ManifestChecksum)
			if change == "trash restore" {
				require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("reader-after-trash", after, []byte("new index"), time.Now().UTC())))
				_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "after-trash", time.Now(), time.Minute)
				require.NoError(t, err)
				_, _, err = s.Restore(t.Context(), node.ID, UnconditionalRev)
				require.NoError(t, err)
				_, err = s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "after-restore", time.Now(), time.Minute)
				require.ErrorIs(t, err, ErrVectorIndexSourceStale)
				require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
			}
		})
	}
}

func checkVectorReaderMemoReusesUnchangedMembership(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	f := seedImageEmbeddingCatalog(t, open(t))
	record, _ := f.publish(t)
	s := f.s
	source, err := s.CaptureVectorIndexSource(t.Context(), record.VectorSpace.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, putVectorIndexGenerationForTest(t, s, vectorIndexGenerationFixture("reader-memo", source, []byte("index"), now)))
	captures := countVectorSourceCaptures(t, s)
	acquire := func() {
		t.Helper()
		lease, err := s.AcquireVectorIndexGeneration(t.Context(), source.VectorSpaceID, "reader", now, time.Minute)
		require.NoError(t, err)
		require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), lease.ID, lease.FencingToken, now))
		require.EqualValues(t, 1, captures.Load(), "unchanged membership must not rescan vectors")
	}
	acquire()
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 2, Key: EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
	}))
	acquire()
	asset, err := s.PhotoAssetForNode(t.Context(), f.node.ID)
	require.NoError(t, err)
	_, err = s.SetPhotoDisplay(t.Context(), asset.ID, asset.Revision, asset.DisplayFileID)
	require.NoError(t, err)
	acquire()
	_, err = s.CreateFile(t.Context(), s.RootID(), "unrelated.txt", hashVectorIndexTest("unrelated"), 9, "text/plain")
	require.NoError(t, err)
	acquire()
	// A second vector space changes independently of the first one's reader cache.
	other := embeddingSetFixture(t.Context(), s, record.ContentVersionID, embeddingCatalogProfile(t).Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	require.NotEqual(t, record.VectorSpace.ID, other.VectorSpace.ID)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), other))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: other.ContentVersionID, BindingID: other.BindingID, InputKind: other.InputKind},
		SetID: other.ID, VectorSpaceID: other.VectorSpace.ID, ProcessingProfileFingerprint: other.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
	}))
	acquire()
}

func checkVectorReaderMemoUsesConsumerSnapshot(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s, record, source := publishedVectorIndexFixture(t, open(t))
	require.Equal(t, source.ManifestChecksum, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
	require.NoError(t, s.withReadTx(t.Context(), func(tx *sql.Tx) error {
		var count int
		require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM embedding_heads`).Scan(&count))
		require.Equal(t, 1, count)
		replacement := record
		replacement.ID = hashVectorIndexTest("snapshot-replacement")
		replacement.InputGeneration.ID = hashVectorIndexTest("snapshot-replacement-input")
		require.NoError(t, s.StageEmbeddingSet(t.Context(), replacement))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: 2, Key: EmbeddingHeadKey{ContentVersionID: replacement.ContentVersionID, BindingID: replacement.BindingID, InputKind: replacement.InputKind},
			SetID: replacement.ID, VectorSpaceID: replacement.VectorSpace.ID, ProcessingProfileFingerprint: replacement.ProcessingProfileFingerprint, PublishedAt: embeddingCatalogTime,
		}))
		after := memoVectorSourceChecksum(t, s, source.VectorSpaceID)
		require.NotEqual(t, source.ManifestChecksum, after)
		old, err := s.vectorIndexSourceChecksumTx(t.Context(), tx, source.VectorSpaceID)
		require.NoError(t, err)
		require.Equal(t, source.ManifestChecksum, old)
		captures := countVectorSourceCaptures(t, s)
		require.Equal(t, after, memoVectorSourceChecksum(t, s, source.VectorSpaceID))
		require.Zero(t, captures.Load(), "an old snapshot cannot overwrite the newer memo")
		return nil
	}))
}

func checkVectorReaderMemoTracksRenditionChanges(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	s, version, profile, attachment := seedEmbeddingCatalog(t, open(t))
	publish := func(record EmbeddingSetRecord, fence int64) {
		t.Helper()
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			FencingToken: fence, Key: EmbeddingHeadKey{ContentVersionID: version, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	original := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	chunk := embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachment)
	require.Equal(t, original.VectorSpace.ID, chunk.VectorSpace.ID)
	publish(original, 1)
	publish(chunk, 1)
	before, err := s.CaptureVectorIndexSource(t.Context(), original.VectorSpace.ID)
	require.NoError(t, err)
	require.Len(t, before.Members, 2)
	require.Equal(t, before.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
	build := catalogRenditionBuild(s, profile)
	build.ID = hashVectorIndexTest("reader-replacement-build")
	build.EvidenceChecksum = embeddingCatalogEvidence(t).Checksum
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	next := RenditionAttachmentRecord{ID: hashVectorIndexTest("reader-replacement-attachment"), VaultID: s.VaultID(), ContentVersionID: version, BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime}
	require.NoError(t, publishRenditionForTest(t, s, next, embeddingCatalogTime, hashVectorIndexTest("reader-replacement-lexical")))
	after, err := s.CaptureVectorIndexSource(t.Context(), before.VectorSpaceID)
	require.NoError(t, err)
	require.Len(t, after.Members, 1)
	require.NotEqual(t, before.ManifestChecksum, after.ManifestChecksum)
	require.Equal(t, after.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
	chunk = embeddingSetFixture(t.Context(), s, version, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", next.ID)
	publish(chunk, 2)
	before, err = s.CaptureVectorIndexSource(t.Context(), before.VectorSpaceID)
	require.NoError(t, err)
	require.Len(t, before.Members, 2)
	require.Equal(t, before.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
	_, err = s.PurgeDerivatives(t.Context(), PurgeRequest{AttachmentIDs: []string{next.ID}})
	require.NoError(t, err)
	after, err = s.CaptureVectorIndexSource(t.Context(), before.VectorSpaceID)
	require.NoError(t, err)
	require.Len(t, after.Members, 1)
	require.NotEqual(t, before.ManifestChecksum, after.ManifestChecksum)
	require.Equal(t, after.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
}

func checkVectorReaderMemoTracksPhotoDisplay(t *testing.T, open func(*testing.T) *Store) {
	t.Helper()
	t.Parallel()
	f := seedImageEmbeddingCatalog(t, open(t))
	s := f.s
	first, _ := f.publish(t)
	node, err := s.CreateFile(t.Context(), s.RootID(), "second-display.jpg", hashVectorIndexTest("second-display"), 20, "image/jpeg")
	require.NoError(t, err)
	second, _ := f.forNode(t, node).publish(t)
	asset, err := s.PhotoAssetForNode(t.Context(), f.node.ID)
	require.NoError(t, err)
	asset, err = s.SetPhotoDisplay(t.Context(), asset.ID, asset.Revision, asset.DisplayFileID)
	require.NoError(t, err)
	other, err := s.PhotoAssetForNode(t.Context(), node.ID)
	require.NoError(t, err)
	_, err = s.DetachPhotoFile(t.Context(), other.ID, other.Revision, *other.DisplayFileID, PhotoDetachOptions{})
	require.NoError(t, err)
	asset, err = s.AttachPhotoFile(t.Context(), asset.ID, asset.Revision, node.ID, PhotoRoleImage, nil)
	require.NoError(t, err)
	before, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	require.Len(t, before.Members, 1)
	require.Equal(t, first.ID, before.Members[0].EmbeddingSetID)
	require.Equal(t, before.ManifestChecksum, memoVectorSourceChecksum(t, s, before.VectorSpaceID))
	var secondFile string
	for _, file := range asset.Files {
		if file.NodeID == node.ID {
			secondFile = file.ID
		}
	}
	require.NotEmpty(t, secondFile)
	_, err = s.SetPhotoDisplay(t.Context(), asset.ID, asset.Revision, &secondFile)
	require.NoError(t, err)
	after, err := s.CaptureVectorIndexSource(t.Context(), first.VectorSpace.ID)
	require.NoError(t, err)
	require.Len(t, after.Members, 1)
	require.Equal(t, second.ID, after.Members[0].EmbeddingSetID)
	require.NotEqual(t, before.ManifestChecksum, after.ManifestChecksum)
	require.Equal(t, after.ManifestChecksum, memoVectorSourceChecksum(t, s, after.VectorSpaceID))
}
