package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

type photoStackGenerationQuerier struct {
	*sql.DB

	beforeLoad func()
}

func (q photoStackGenerationQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if q.beforeLoad != nil {
		q.beforeLoad()
	}
	return q.DB.QueryRowContext(ctx, query, args...)
}

func TestPhotoStackPagesReuseOpenedGeneration(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	record, vector := f.publish(t)
	index := newCursorIndexFixture(f)
	index.add(record, vector)
	index.generation(t, "reused-image-generation")
	generationReads := 0
	q := &photoStackGenerationQuerier{DB: f.s.db, beforeLoad: func() { generationReads++ }}
	first, err := f.s.newPhotoStackVectorReader(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, first.indexes, 1)
	second, err := f.s.newPhotoStackVectorReader(t.Context(), q)
	require.NoError(t, err)
	require.Same(t, first.indexes[0], second.indexes[0], "the immutable generation is reused")
	require.Equal(t, 1, generationReads)
	index.generation(t, "replacement-image-generation")
	replacement, err := f.s.newPhotoStackVectorReader(t.Context(), q)
	require.NoError(t, err)
	require.Len(t, replacement.indexes, 1)
	require.NotSame(t, first.indexes[0], replacement.indexes[0])
	require.Equal(t, 2, generationReads)
	require.Len(t, f.s.photoStackVectors, 1, "retain only the current generation per space")
	type result struct {
		reader *photoStackVectorReader
		err    error
	}
	results := make(chan result, 8)
	start := make(chan struct{})
	for range 8 {
		go func() {
			<-start
			reader, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
			results <- result{reader, err}
		}()
	}
	close(start)
	var readers []*photoStackVectorReader
	for range 8 {
		got := <-results
		require.NoError(t, got.err)
		readers = append(readers, got.reader)
	}
	for _, reader := range readers {
		require.Len(t, reader.indexes, 1)
		require.Equal(t, map[string]string{index.space: browseHash("replacement-image-generation")}, reader.heads)
		require.Same(t, readers[0].indexes[0], reader.indexes[0])
	}
	t.Run("cancellation", func(t *testing.T) {
		index.generation(t, "reader-errors")
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reads := 0
		q := photoStackGenerationQuerier{DB: f.s.db, beforeLoad: func() {
			reads++
			cancel()
		}}
		reader, err := f.s.newPhotoStackVectorReader(ctx, q)
		require.Equal(t, 1, reads)
		require.Nil(t, reader)
		require.ErrorIs(t, err, context.Canceled)
	})
	require.NoError(t, f.s.Close())
	require.Empty(t, f.s.photoStackVectors)
}

func TestPhotoStackDecisionKeepsUnchangedStackAcrossIndexPublication(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	ids := burstPhotos(t, f.s, 2)
	index := newCursorIndexFixture(f)
	for _, id := range ids {
		index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	}
	index.generation(t, "unchanged-before")
	targets := preparedPhotoTargets(t.Context(), t, f.s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	vectors, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic unrelated photo")
	require.NoError(t, err)
	unrelated := stackPhoto(t, f.s, run, "/other/unrelated.jpg", "2024-06-11T12:00:00Z", document.SourceMetadataPrecisionSecond)
	index.publish(t, unrelated, []float64{1, 0, 0, 0, 0, 0, 0, 0})
	decision := PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, Decision: "current", CurrentID: ids[0]}
	index.generation(t, "unchanged-after")
	_, err = f.s.decidePhotoStack(t.Context(), decision, vectors)
	require.NoError(t, err)
	flags, err := f.s.LookupPhotoFlags(t.Context(), ids)
	require.NoError(t, err)
	require.ElementsMatch(t, []PhotoFlagRow{{AssetID: ids[0], Flag: "pick"}, {AssetID: ids[1], Flag: "reject"}}, flags)
}

func TestPhotoStackDecisionRejectsGenerationThatSplitsStack(t *testing.T) {
	t.Parallel()
	f := newImageEmbeddingCatalogFixture(t)
	run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic image-linked stack")
	require.NoError(t, err)
	ids := []string{
		stackPhoto(t, f.s, run, "/shots/one.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond),
		stackPhoto(t, f.s, run, "/shots/two.jpg", "2024-06-10T12:00:10Z", document.SourceMetadataPrecisionSecond),
	}
	index := newCursorIndexFixture(f)
	var record EmbeddingSetRecord
	for _, id := range ids {
		asset, err := f.s.PhotoAssetByID(t.Context(), id)
		require.NoError(t, err)
		node, err := f.s.NodeByID(t.Context(), asset.Files[0].NodeID)
		require.NoError(t, err)
		var vector document.VectorSetV1
		record, vector = f.forNode(t, node).publish(t, []float64{1, 0, 0, 0, 0, 0, 0, 0})
		index.add(record, vector)
	}
	index.generation(t, "decision-before")
	require.Equal(t, [][]int{{0, 1}}, stackIndexes(t, f.s, ids))
	targets := preparedPhotoTargets(t.Context(), t, f.s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	vectors, err := f.s.newPhotoStackVectorReader(t.Context(), f.s.db)
	require.NoError(t, err)
	before, err := f.s.LookupPhotoFlags(t.Context(), ids)
	require.NoError(t, err)
	index.vectors[1].Vectors = [][]float32{{0, 1, 0, 0, 0, 0, 0, 0}}
	payload, checksum, err := document.EncodeVectorSetV1(index.vectors[1])
	require.NoError(t, err)
	record.ID = browseHash("orthogonal-image-vector-set")
	record.VectorSet.ID = checksum
	record.VectorSet.PayloadChecksum = checksum
	record.VectorSet.PayloadBlobHash = testSHA256(payload)
	record.VectorSet.Payload = payload
	require.NoError(t, f.s.withStorageTx(t.Context(), func(tx *sql.Tx) error {
		return f.s.EnsureBlobTx(tx, record.VectorSet.PayloadBlobHash, int64(len(payload)))
	}))
	require.NoError(t, f.s.StageEmbeddingSet(t.Context(), record))
	require.NoError(t, f.s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: record.ContentVersionID, BindingID: record.BindingID, InputKind: record.InputKind},
		SetID: record.ID, VectorSpaceID: record.VectorSpace.ID, ProcessingProfileFingerprint: record.ProcessingProfileFingerprint,
		PublishedAt: embeddingCatalogTime, FencingToken: 2,
	}))
	index.checksums[1] = checksum
	index.generation(t, "decision-after")
	_, err = f.s.decidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: targets, Decision: "current", CurrentID: ids[0]}, vectors)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	after, err := f.s.LookupPhotoFlags(t.Context(), ids)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPhotoStackVectorsUseOnlyCosineCompatibleSpaces(t *testing.T) {
	t.Parallel()
	for _, metric := range []string{document.VectorMetricDotProduct, document.VectorMetricL2} {
		t.Run(metric, func(t *testing.T) {
			f := newImageEmbeddingCatalogFixture(t)
			f.request.Descriptor.Metric = metric
			var err error
			f.request.Descriptor, err = document.NewEmbeddingDescriptor(f.request.Descriptor)
			require.NoError(t, err)
			var profile document.ProcessingProfileV1
			require.NoError(t, json.Unmarshal(f.request.Profile.CanonicalProfile, &profile))
			for i := range profile.Embeddings {
				if profile.Embeddings[i].Name == f.binding.Name {
					profile.Embeddings[i].Metric = metric
					profile.Embeddings[i].Descriptor.Fingerprint = f.request.Descriptor.Fingerprint
				}
			}
			fingerprints := refingerprintImageEmbeddingProfile(t, &f, profile)
			_, err = f.s.GrantConsent(t.Context(), ProcessingConsentGrantRequest{Principal: f.request.Authorization.Principal, Scope: f.request.Authorization.Scope, ProfileFingerprint: fingerprints.Profile, DisclosureFingerprint: f.binding.DisclosureFingerprint, InputClasses: f.request.Authorization.InputClasses, RetainedArtifactClasses: f.request.Authorization.RetainedArtifactClasses})
			require.NoError(t, err)
			run, err := f.s.BeginIngest(t.Context(), "cli", "synthetic metric stacks")
			require.NoError(t, err)
			ids := []string{
				stackPhoto(t, f.s, run, "/metric/one.jpg", "2024-06-10T12:00:00Z", document.SourceMetadataPrecisionSecond),
				stackPhoto(t, f.s, run, "/metric/two.jpg", "2024-06-10T12:00:30Z", document.SourceMetadataPrecisionSecond),
			}
			index := newCursorIndexFixture(f)
			for _, id := range ids {
				index.publish(t, id, []float64{1, 0, 0, 0, 0, 0, 0, 0})
			}
			index.generation(t, "metric-generation")
			groups, _, _, _, err := f.s.PhotoStackGroups(t.Context(), ids, false, "")
			require.NoError(t, err)
			if metric == document.VectorMetricL2 {
				require.Empty(t, groups)
			} else {
				require.Equal(t, [][]string{ids}, groups)
			}
		})
	}
}

func refingerprintImageEmbeddingProfile(t *testing.T, f *imageEmbeddingCatalogFixture, profile document.ProcessingProfileV1) document.FingerprintSet {
	t.Helper()
	canonical, fingerprints, err := document.CanonicalProfile(profile)
	require.NoError(t, err)
	for _, binding := range profile.Embeddings {
		if binding.Name == f.binding.Name {
			f.binding = binding
		}
	}
	f.request.Profile.CanonicalProfile = canonical
	f.request.Profile.Fingerprint = fingerprints.Profile
	f.request.Profile.RenditionRequestFingerprint = fingerprints.RenditionRequest
	f.request.Profile.EvidenceLexicalFingerprint = fingerprints.EvidenceLexical
	f.request.Profile.RetentionDisclosureFingerprint = fingerprints.RetentionDisclosure
	f.request.Authorization.ProfileFingerprint = fingerprints.Profile
	f.request.InputGeneration.ProcessingProfileFingerprint = fingerprints.Profile
	return fingerprints
}
