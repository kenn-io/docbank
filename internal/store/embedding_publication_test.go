package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestRenditionPublicationRevokesStaleChunkEmbeddingHead(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	for _, record := range []EmbeddingSetRecord{chunk, direct} {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
		require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
			Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
			SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
			ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
		}))
	}
	build := catalogRenditionBuild(s, profile)
	build.ID = testSHA256([]byte("replacement-embedding-build"))
	build.CapturedArtifactPolicy = jsontext.Value(`{"roles":[{"max_count":1,"min_count":1,"role":"normalized_evidence"},{"max_count":1,"min_count":0,"role":"provider_markdown"},{"max_count":1,"min_count":1,"role":"sanitized_markdown"}],"version":1}`)
	build.CapturedArtifactPolicyFingerprint = testSHA256(build.CapturedArtifactPolicy)
	require.NoError(t, s.StageRenditionBuild(t.Context(), build))
	replacement := RenditionAttachmentRecord{
		ID: testSHA256([]byte("replacement-embedding-attachment")), VaultID: s.VaultID(), ContentVersionID: versionID,
		BuildID: build.ID, Profile: profile, AttachedAt: embeddingCatalogTime,
	}
	require.NoError(t, publishRenditionForTest(t, s, replacement, embeddingCatalogTime,
		testSHA256([]byte("replacement-embedding-lexical-generation"))))

	var headID string
	err := s.db.QueryRow(`SELECT embedding_set_id FROM embedding_heads WHERE embedding_set_id=?`, chunk.ID).Scan(&headID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	assert.Equal(t, direct.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, direct.BindingID, direct.InputKind))
	plan, err := s.DerivativeGCPlan(t.Context())
	require.NoError(t, err)
	require.Len(t, plan.EmbeddingSets, 1)
	assert.Equal(t, chunk.ID, plan.EmbeddingSets[0].SetID)
}

func TestEmbeddingPurgeRejectsStalePublication(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "deleted"
		if retained {
			name = "retained by lease"
		}
		t.Run(name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
			head := EmbeddingHeadRecord{
				Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
				SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
				ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
			}
			require.NoError(t, s.PublishEmbeddingHead(t.Context(), head))
			if retained {
				require.NoError(t, s.PutCurrentRenditionRoot(t.Context(), CurrentRenditionRoot{
					ID: "purged-embedding-reader", Kind: RenditionRootReaderLease,
					TargetKind: RenditionRootEmbeddingSet, TargetID: record.ID, FencingToken: 1,
					RecordedAt: embeddingCatalogTime, ExpiresAt: "2099-08-25T10:00:00.000000000Z",
				}))
				seedInitialAuditAuthority(t, s, s.RootID())
			}
			_, err := s.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
			require.NoError(t, err)
			require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "purge")
			require.Error(t, s.PublishEmbeddingHead(t.Context(), head))
			replacement := cloneEmbeddingSetRecord(record)
			replacement.ID = testSHA256([]byte("authorized-embedding-replacement"))
			replacement.InputGeneration.ID = testSHA256([]byte("replacement-original-input-generation"))
			require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), replacement), "purge",
				"a different set ID must not evade the binding's purge")

			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(t.Context(), &exported))
			require.ErrorContains(t, restored.StageEmbeddingSet(t.Context(), record), "purge")
			require.Error(t, restored.PublishEmbeddingHead(t.Context(), head))

			require.NoError(t, restored.AuthorizeEmbeddingRebuild(t.Context(), EmbeddingRebuildAuthorization{
				Key: head.Key, ProcessingProfileFingerprint: profile.Fingerprint,
				SetID: replacement.ID, AuthorizedAt: nowRFC3339(),
			}))
			require.ErrorContains(t, restored.StageEmbeddingSet(t.Context(), record), "purge",
				"authorizing the replacement must not authorize stale work")
			require.NoError(t, restored.StageEmbeddingSet(t.Context(), replacement))
			head.SetID = replacement.ID
			require.NoError(t, restored.PublishEmbeddingHead(t.Context(), head))
			assert.Equal(t, replacement.ID, embeddingHeadSetIDForTest(t, restored, versionID,
				profile.Fingerprint, head.Key.BindingID, head.Key.InputKind))
			require.NoError(t, restored.ValidateMetadata(t.Context()))
			_, err = restored.PurgeDerivatives(t.Context(), PurgeRequest{ContentVersionIDs: []string{versionID}})
			require.NoError(t, err)
			require.ErrorContains(t, restored.StageEmbeddingSet(t.Context(), replacement), "purge",
				"a later purge must revoke the earlier rebuild authorization")
		})
	}
}

func TestEmbeddingAttachmentPurgeLeavesDirectBindingPublishable(t *testing.T) {
	s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
	chunk := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
	direct := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
	for _, record := range []EmbeddingSetRecord{chunk, direct} {
		require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
	}
	_, err := s.PurgeDerivatives(t.Context(), PurgeRequest{AttachmentIDs: []string{attachmentID}})
	require.NoError(t, err)
	require.NoError(t, s.StageEmbeddingSet(t.Context(), direct))
	require.NoError(t, s.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
		Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: direct.BindingID, InputKind: direct.InputKind},
		SetID: direct.ID, VectorSpaceID: direct.VectorSpace.ID,
		ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
	}))
	assert.Equal(t, direct.ID, embeddingHeadSetIDForTest(t, s, versionID, profile.Fingerprint, direct.BindingID, direct.InputKind))
}
