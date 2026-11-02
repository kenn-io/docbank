package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

// BenchmarkSemanticSearchCoverage measures one search's store calls with one
// embedded document and a growing live scope. Fixture creation is excluded.
func BenchmarkSemanticSearchCoverage(b *testing.B) {
	for _, files := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s, err := Open(b.Context(), filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			_, versionID, profile, _ := seedEmbeddingCatalog(b, s)
			ctx := b.Context()
			record := embeddingSetFixture(ctx, s, versionID, profile.Fingerprint,
				document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(b, s.StageEmbeddingSet(ctx, record))
			require.NoError(b, s.PublishEmbeddingHead(ctx, EmbeddingHeadRecord{
				FencingToken: 1, Key: EmbeddingHeadKey{ContentVersionID: versionID,
					BindingID: record.BindingID, InputKind: record.InputKind},
				SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
				ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
			}))
			// The catalog fixture already includes one unembedded live file.
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				for i := 1; i < files; i++ {
					if _, _, err := s.createFileTx(ctx, tx, s.RootID(),
						fmt.Sprintf("unembedded-%06d.pdf", i), catalogSourceHash, 20, "application/pdf"); err != nil {
						return err
					}
				}
				return nil
			}))
			set, _, err := document.DecodeVectorSetV1(record.VectorSet.Payload, document.VectorBounds{
				MaxRows: 100, MaxDimension: record.VectorSpace.Descriptor.Dimension,
				MaxBytes: len(record.VectorSet.Payload),
			})
			require.NoError(b, err)
			manifest, err := vectorindex.NewManifest([]string{record.VectorSet.ID})
			require.NoError(b, err)
			generation, err := vectorindex.BuildGeneration(manifest, []document.VectorSetV1{set}, vectorindex.Options{})
			require.NoError(b, err)
			source, err := s.CaptureVectorIndexSource(ctx, record.VectorSpace.ID)
			require.NoError(b, err)
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
				authority, err := s.AcquireSemanticSearchAuthority(ctx, profile.Fingerprint, record.BindingID,
					"coverage-benchmark", now, time.Minute, SearchOptions{})
				require.NoError(b, err)
				resolution, resolveErr := s.ResolveSemanticCandidates(ctx, profile.Fingerprint, record.BindingID,
					record.InputKind, record.VectorSpace.ID, source.ManifestChecksum, neighbors, 20, SearchOptions{}, nil)
				revalidation, revalidateErr := s.RevalidateSearchCandidates(ctx, requested, SearchOptions{}, profile.Fingerprint, record.BindingID)
				releaseErr := s.ReleaseVectorIndexGeneration(ctx, authority.Lease.ID, authority.Lease.FencingToken, now)
				require.NoError(b, resolveErr)
				require.NoError(b, revalidateErr)
				require.NoError(b, releaseErr)
				require.Len(b, resolution.Candidates, 1)
				require.Len(b, revalidation.Candidates, 1)
				require.Equal(b, &SearchCoverageSnapshot{ScopedDocuments: files + 1, CompleteDocuments: 1}, revalidation.Coverage)
			}
		})
	}
}
