package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/vectorindex"
)

func TestSemanticSearchAuthorityHeadSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		kind  document.EmbeddingInputKind
		scope string
	}{
		{"original", document.EmbeddingInputOriginalFile, "all"},
		{"original selected version", document.EmbeddingInputOriginalFile, "version"},
		{"original tag SQL fallback", document.EmbeddingInputOriginalFile, "tag"},
		{"rendition SQL fallback", document.EmbeddingInputRenditionChunk, "all"},
		{"rendition selected version", document.EmbeddingInputRenditionChunk, "version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, tc.kind, 2, false)
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			now := time.Now().UTC()
			stored := vectorIndexGenerationFixture("atomic-snapshot", source, []byte("synthetic-index"), now)
			require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
			opts := SearchOptions{}
			wantRecords, scoped, complete := records, 3, 2
			if tc.scope != "all" {
				wantRecords, scoped, complete = records[:1], 1, 1
				if tc.scope == "version" {
					opts.ContentVersionIDs = []string{records[0].ContentVersionID}
				} else {
					tag, err := s.CreateTag(t.Context(), "atomic-selected")
					require.NoError(t, err)
					version, err := s.ContentVersionByID(t.Context(), records[0].ContentVersionID)
					require.NoError(t, err)
					_, err = s.AssignTag(t.Context(), tag.ID, version.NodeID, UnconditionalRev)
					require.NoError(t, err)
					opts.TagID = tag.ID
				}
			}
			authority, err := s.AcquireSemanticSearchAuthorityHead(t.Context(), profile.Fingerprint, records[0].BindingID, "atomic-snapshot", now, time.Minute, opts)
			require.NoError(t, err)
			require.Equal(t, source.ManifestChecksum, authority.Lease.Generation.SourceManifestChecksum)
			require.Equal(t, stored.ID, authority.Lease.Generation.ID)
			require.Empty(t, authority.Lease.Generation.Bytes)
			require.Equal(t, len(stored.Bytes), authority.Lease.Generation.ByteSize)
			require.Equal(t, tc.kind, authority.InputKind)
			require.Equal(t, scoped, authority.ScopedDocuments)
			require.Equal(t, complete, authority.CompleteDocuments)
			var wantANN []vectorindex.RowIdentity
			for _, record := range wantRecords {
				for _, input := range record.InputGeneration.Inputs {
					wantANN = append(wantANN, vectorindex.RowIdentity{SetID: record.VectorSet.ID, InputKey: input.ID, InputChecksum: input.RenderedChecksum})
				}
			}
			require.ElementsMatch(t, wantANN, authority.ANNRows)
			require.NoError(t, s.ReleaseVectorIndexGeneration(t.Context(), authority.Lease.ID, authority.Lease.FencingToken, now))
			var leases int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
			require.Zero(t, leases)
		})
	}
}

func TestSemanticSearchAuthorityHeadFailuresReturnNoAuthority(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"pre-cancel", "lease insert", "source stale", "descriptor", "invalid duration", "runtime descriptor"} {
		t.Run(mode, func(t *testing.T) {
			s, profile, records, _ := newSemanticPrefixFixture(t, document.EmbeddingInputOriginalFile, 1, false)
			source, err := s.CaptureVectorIndexSource(t.Context(), records[0].VectorSpace.ID)
			require.NoError(t, err)
			now := time.Now().UTC()
			stored := vectorIndexGenerationFixture("atomic-failure", source, []byte("synthetic-index"), now)
			require.NoError(t, putVectorIndexGenerationForTest(t, s, stored))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			duration := time.Minute
			opts := SearchOptions{}
			switch mode {
			case "pre-cancel":
				cancel()
			case "lease insert":
				_, err := s.db.Exec(`CREATE TRIGGER reject_atomic_lease BEFORE INSERT ON vector_index_reader_leases BEGIN SELECT RAISE(ABORT,'synthetic lease rejection'); END`)
				require.NoError(t, err)
			case "source stale":
				other := hashVectorIndexTest("atomic-stale-source")
				_, err := s.db.Exec(`UPDATE vector_index_generations SET source_manifest_checksum=? WHERE generation_id=?`, other, stored.ID)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE vector_index_heads SET source_manifest_checksum=? WHERE vector_space_id=?`, other, source.VectorSpaceID)
				require.NoError(t, err)
			case "descriptor":
				_, err := s.db.Exec(`DROP TRIGGER embedding_vector_spaces_immutable_update`)
				require.NoError(t, err)
				_, err = s.db.Exec(`UPDATE embedding_vector_spaces SET descriptor_json=? WHERE vector_space_id=?`, []byte(`{}`), source.VectorSpaceID)
				require.NoError(t, err)
			case "invalid duration":
				duration = 0
			case "runtime descriptor":
				descriptor := records[0].VectorSpace.Descriptor
				descriptor.Model = "synthetic-other-model"
				descriptor, err = document.NewEmbeddingDescriptor(descriptor)
				require.NoError(t, err)
				opts.EmbeddingDescriptor = &descriptor
			}
			authority, err := s.AcquireSemanticSearchAuthorityHead(ctx, profile.Fingerprint, records[0].BindingID, "atomic-failure", now, duration, opts)
			require.Error(t, err)
			if mode == "pre-cancel" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if mode == "source stale" {
				require.ErrorIs(t, err, ErrVectorIndexSourceStale)
			}
			if mode == "lease insert" {
				require.ErrorContains(t, err, "synthetic lease rejection")
			}
			if mode == "runtime descriptor" {
				require.ErrorContains(t, err, "query runtime differs from profile")
			}
			require.Zero(t, authority, "no partial generation, counts, ANN identities or lease may escape")
			var leases int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM vector_index_reader_leases`).Scan(&leases))
			require.Zero(t, leases)
		})
	}
}
