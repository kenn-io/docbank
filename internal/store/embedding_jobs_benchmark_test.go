package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

// BenchmarkClaimNextEmbeddingWork grows retained sets in the queued job's
// vector space while keeping the queue fixed at one eligible job.
func BenchmarkClaimNextEmbeddingWork(b *testing.B) {
	for _, sets := range []int{1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("sets=%d", sets), func(b *testing.B) {
			ctx := b.Context()
			s, err := Open(ctx, filepath.Join(b.TempDir(), "docbank.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			profile := embeddingCatalogProfile(b)
			var retained, pending Node
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				if err := ensureProcessingProfileTx(ctx, tx, profile); err != nil {
					return err
				}
				var err error
				retained, _, err = s.createFileTx(ctx, tx, s.RootID(), "retained.txt", catalogSourceHash, 20, "text/plain")
				if err != nil {
					return err
				}
				pending, _, err = s.createFileTx(ctx, tx, s.RootID(), "pending.txt", catalogSourceHash, 20, "text/plain")
				return err
			}))
			record := embeddingSetFixture(ctx, s, retained.CurrentVersionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(b, s.StageEmbeddingSet(ctx, record))
			// Reuse the vector payload across retained generations of one source.
			// Bulk inserts keep setup bounded without disabling foreign keys.
			require.NoError(b, s.withStorageTx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<$1)
					INSERT INTO embedding_input_generations
					SELECT printf('%064x',i),generation_blob_hash,generation_encoded_size,generation_checksum,
					source_version_id,profile_fingerprint,evidence_fingerprint,tokenizer_fingerprint,
					chunk_policy_fingerprint,formatter_fingerprint,attachment_context_fingerprint,attachment_id,input_count,created_at
					FROM seq CROSS JOIN embedding_input_generations WHERE generation_id=$2`, sets-1, record.InputGeneration.ID)
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO embedding_generation_inputs
					SELECT g.generation_id,i.input_id,i.input_order,i.rendered_checksum
					FROM embedding_input_generations g CROSS JOIN embedding_generation_inputs i
					WHERE g.generation_id<>$1 AND i.generation_id=$1`, record.InputGeneration.ID)
				if err != nil {
					return err
				}
				_, err = tx.ExecContext(ctx, `INSERT INTO embedding_sets
					SELECT g.generation_id,s.vault_uid,s.binding_id,s.input_kind,s.content_version_id,
					s.profile_fingerprint,s.embedding_input_fingerprint,s.vector_space_id,g.generation_id,s.vector_set_id,s.created_at
					FROM embedding_input_generations g CROSS JOIN embedding_sets s
					WHERE g.generation_id<>$1 AND s.embedding_set_id=$2`, record.InputGeneration.ID, record.ID)
				return err
			}))
			requestRecord := embeddingSetFixture(ctx, s, pending.CurrentVersionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			binding, _, err := embeddingBindingFromProfile(profile, "optional")
			require.NoError(b, err)
			consent := ProviderOperationAuthorizationRequest{
				Principal: "operator:benchmark", Scope: "embedding:optional",
				ProfileFingerprint: profile.Fingerprint, DisclosureFingerprint: binding.DisclosureFingerprint,
				InputClasses: []string{string(binding.InputKind)}, RetainedArtifactClasses: []string{"embedding_vector_set"},
			}
			_, err = s.GrantConsent(ctx, ProcessingConsentGrantRequest{
				Principal: consent.Principal, Scope: consent.Scope, ProfileFingerprint: consent.ProfileFingerprint,
				DisclosureFingerprint: consent.DisclosureFingerprint, InputClasses: consent.InputClasses,
				RetainedArtifactClasses: consent.RetainedArtifactClasses,
			})
			require.NoError(b, err)
			job, err := s.EnqueueEmbeddingJob(ctx, EmbeddingJobRequest{
				ContentVersionID: pending.CurrentVersionID, Profile: profile, BindingID: binding.Name,
				Descriptor: requestRecord.VectorSpace.Descriptor, InputGeneration: requestRecord.InputGeneration, Authorization: consent,
			})
			require.NoError(b, err)
			at := time.Now().UTC()
			fingerprints := []string{requestRecord.VectorSpace.Descriptor.Fingerprint}
			b.ReportAllocs()
			for b.Loop() {
				claim, work, found, err := s.ClaimNextEmbeddingWork(ctx, "benchmark-worker", at, time.Minute, fingerprints)
				if err != nil || !found || claim.AttemptID != job.ID || work.InputGeneration.ID != requestRecord.InputGeneration.ID {
					b.Fatalf("claim=%+v generation=%s found=%v err=%v", claim, work.InputGeneration.ID, found, err)
				}
				b.StopTimer()
				require.NoError(b, s.ReleaseEmbeddingWork(ctx, claim, at))
				b.StartTimer()
			}
		})
	}
}
