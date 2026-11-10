package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// BenchmarkSemanticSearchCoverage measures one search's store calls with a
// growing live scope and 10% embedded files. Fixture creation is excluded.
func BenchmarkSemanticSearchCoverage(b *testing.B) {
	for _, files := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, record, _ := newPublishedVectorIndexFixture(b)
			ctx := b.Context()
			profileFingerprint := record.ProcessingProfileFingerprint
			versionID := record.ContentVersionID
			versions := make([]string, 0, files/10-1)
			// The fixture includes one embedded file and one unembedded file.
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := 2; i < files; i++ {
					_, version, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("coverage-%06d.pdf", i), catalogSourceHash, 20, "application/pdf")
					if err != nil {
						return err
					}
					if i%10 == 0 {
						versions = append(versions, version.ID)
					}
				}
				return nil
			}))
			records := []EmbeddingSetRecord{record}
			for _, id := range versions {
				next := embeddingSetFixture(ctx, s, id, profileFingerprint, record.InputKind, record.BindingID, "")
				require.NoError(b, s.StageEmbeddingSet(ctx, next))
				require.NoError(b, s.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{
					FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: id,
						BindingID: next.BindingID, InputKind: next.InputKind},
					SetID: next.ID, VectorSpaceID: next.VectorSpace.ID,
					ProcessingProfileFingerprint: profileFingerprint, PublishedAt: embeddingCatalogTime,
				}))
				records = append(records, next)
			}
			var sets []document.VectorSetV1
			var setIDs []string
			for _, embedded := range records {
				set, _, err := document.DecodeVectorSetV1(embedded.VectorSet.Payload, document.VectorBounds{
					MaxRows: 100, MaxDimension: embedded.VectorSpace.Descriptor.Dimension,
					MaxBytes: len(embedded.VectorSet.Payload),
				})
				require.NoError(b, err)
				sets = append(sets, set)
				setIDs = append(setIDs, embedded.VectorSet.ID)
			}
			manifest, err := vectorindex.NewManifest(setIDs)
			require.NoError(b, err)
			generation, err := vectorindex.BuildGeneration(manifest, sets, vectorindex.Options{})
			require.NoError(b, err)
			source, err := s.CaptureVectorIndexSource(ctx, record.VectorSpace.ID)
			require.NoError(b, err)
			require.Len(b, source.Members, files/10)
			require.NoError(b, putVectorIndexGenerationForTest(b, s, VectorIndexGenerationRecord{
				ID: hashVectorIndexTest("semantic-coverage-benchmark"), VectorSpaceID: record.VectorSpace.ID,
				SourceManifestChecksum: source.ManifestChecksum, IndexManifestChecksum: generation.Metadata().Manifest.Checksum,
				Bytes: generation.Bytes(), RowCount: generation.Metadata().RowCount, BuiltAt: embeddingCatalogTime,
			}))
			neighbors := []vectorindex.Neighbor{{SetID: record.VectorSet.ID,
				InputKey:      record.InputGeneration.Inputs[0].ID,
				InputChecksum: record.InputGeneration.Inputs[0].RenderedChecksum, Score: 0.9}}
			version, err := s.ContentVersionByID(ctx, versionID)
			require.NoError(b, err)
			requested := []SearchCandidateIdentity{{NodeID: version.NodeID, ContentVersionID: versionID,
				Evidence: []SearchEvidenceIdentity{{NodeID: version.NodeID, ContentVersionID: versionID,
					Kind: "embedding", VectorSpaceID: record.VectorSpace.ID, EmbeddingSetID: record.ID,
					InputGenerationID: record.InputGeneration.ID, InputID: record.InputGeneration.Inputs[0].ID,
					InputKind: record.InputKind, SourceManifestChecksum: source.ManifestChecksum}}}}
			b.ReportAllocs()
			for b.Loop() {
				now := time.Now().UTC()
				authority, err := s.AcquireSemanticSearchAuthority(ctx, profileFingerprint, record.BindingID,
					"coverage-benchmark", now, time.Minute, SearchOptions{})
				require.NoError(b, err)
				resolution, resolveErr := s.ResolveSemanticCandidates(ctx, profileFingerprint, record.BindingID,
					record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 20, SearchOptions{}, nil)
				revalidation, revalidateErr := s.RevalidateSearchCandidates(ctx, requested, SearchOptions{}, profileFingerprint, record.BindingID)
				releaseErr := s.ReleaseVectorIndexGeneration(ctx, authority.Lease.ID, authority.Lease.FencingToken, now)
				require.NoError(b, resolveErr)
				require.NoError(b, revalidateErr)
				require.NoError(b, releaseErr)
				require.Len(b, resolution.Candidates, 1)
				require.Len(b, revalidation.Candidates, 1)
				require.Equal(b, &SearchCoverageSnapshot{ScopedDocuments: files, CompleteDocuments: files / 10}, revalidation.Coverage)
			}
		})
	}
}
