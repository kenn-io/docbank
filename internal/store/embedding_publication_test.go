package store

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
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

func TestEmbeddingPurgeFencesWorkBeforeStaging(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := "version"
		if all {
			name = "all"
		}
		t.Run(name, func(t *testing.T) {
			s, versionID, profile, _ := newEmbeddingCatalogFixture(t)
			record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputOriginalFile, "optional", "")
			request := PurgeRequest{ContentVersionIDs: []string{versionID}}
			if all {
				request = PurgeRequest{All: true}
			}
			_, err := s.PurgeDerivatives(t.Context(), request)
			require.NoError(t, err)
			require.ErrorContains(t, s.StageEmbeddingSet(t.Context(), record), "purge")
		})
	}
}

func TestEmbeddingPurgeFencesUnstagedChunkBindings(t *testing.T) {
	for _, selector := range []string{"attachment", "build"} {
		t.Run(selector, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			var buildID string
			require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachmentID).Scan(&buildID))
			request := PurgeRequest{AttachmentIDs: []string{attachmentID}}
			if selector == "build" {
				request = PurgeRequest{BuildIDs: []string{buildID}}
			}
			_, err := s.PurgeDerivatives(t.Context(), request)
			require.NoError(t, err)
			// An unstaged binding must have a durable fence that an explicit
			// rebuild can supersede, even after its attachment has been removed.
			require.NoError(t, s.AuthorizeEmbeddingRebuild(t.Context(), EmbeddingRebuildAuthorization{
				Key:                          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk},
				ProcessingProfileFingerprint: profile.Fingerprint, SetID: testSHA256([]byte("unstaged-rebuild-set")),
				AuthorizedAt: nowRFC3339(),
			}))
		})
	}
}

func TestEmbeddingScopedPurgeClearsAndFencesFailures(t *testing.T) {
	for _, selector := range []string{"attachment", "build", "version", "all"} {
		t.Run(selector, func(t *testing.T) {
			s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
			var buildID string
			require.NoError(t, s.db.QueryRow(`SELECT build_id FROM rendition_attachments WHERE attachment_id=?`, attachmentID).Scan(&buildID))
			chunk := EmbeddingFailureRecord{
				ContentVersionID: versionID, ProcessingProfileFingerprint: profile.Fingerprint,
				BindingID: "chunk", InputKind: document.EmbeddingInputRenditionChunk,
				FailureCode: EmbeddingFailureProviderUnavailable, FailedAt: embeddingCatalogTime,
			}
			direct := chunk
			direct.BindingID, direct.InputKind = "optional", document.EmbeddingInputOriginalFile
			for _, failure := range []EmbeddingFailureRecord{chunk, direct} {
				require.NoError(t, s.RecordEmbeddingFailure(t.Context(), failure))
			}
			requests := map[string]PurgeRequest{
				"attachment": {AttachmentIDs: []string{attachmentID}},
				"build":      {BuildIDs: []string{buildID}},
				"version":    {ContentVersionIDs: []string{versionID}},
				"all":        {All: true},
			}
			// A failed attempt need not have staged a set. Its attachment/profile
			// scope must still be cleared and fenced by the purge.
			_, err := s.PurgeDerivatives(t.Context(), requests[selector])
			require.NoError(t, err)
			var chunkFailures int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_failures
				WHERE content_version_id=? AND profile_fingerprint=? AND binding_id='chunk'`,
				versionID, profile.Fingerprint).Scan(&chunkFailures))
			assert.Zero(t, chunkFailures)
			require.ErrorContains(t, s.RecordEmbeddingFailure(t.Context(), chunk), "purge suppression")
			var directFailures int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM embedding_failures
				WHERE content_version_id=? AND profile_fingerprint=? AND binding_id='optional'`,
				versionID, profile.Fingerprint).Scan(&directFailures))
			if selector == "attachment" || selector == "build" {
				assert.Equal(t, 1, directFailures, "scoped rendition purge must leave original-file failures alone")
				require.NoError(t, s.RecordEmbeddingFailure(t.Context(), direct))
			} else {
				assert.Zero(t, directFailures)
				require.ErrorContains(t, s.RecordEmbeddingFailure(t.Context(), direct), "purge suppression")
			}
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(t.Context(), &exported))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(t.Context(), &exported))
			require.ErrorContains(t, restored.RecordEmbeddingFailure(t.Context(), chunk), "purge suppression")
			require.NoError(t, restored.AuthorizeEmbeddingRebuild(t.Context(), EmbeddingRebuildAuthorization{
				Key:                          EmbeddingHeadKey{ContentVersionID: versionID, BindingID: chunk.BindingID, InputKind: chunk.InputKind},
				ProcessingProfileFingerprint: profile.Fingerprint, SetID: testSHA256([]byte("failure-rebuild-set")),
				AuthorizedAt: nowRFC3339(),
			}))
			chunk.FailedAt = nowRFC3339()
			require.NoError(t, restored.RecordEmbeddingFailure(t.Context(), chunk))
		})
	}
}

func TestEmbeddingWritesRequirePhysicalBlobAuthority(t *testing.T) {
	for _, operation := range []string{"stage", "publish"} {
		for _, missing := range []string{"source", "vectors", "generation", "evidence"} {
			t.Run(operation+"/"+missing, func(t *testing.T) {
				s, versionID, profile, attachmentID := newEmbeddingCatalogFixture(t)
				record := embeddingSetFixture(s, versionID, profile.Fingerprint, document.EmbeddingInputRenditionChunk, "chunk", attachmentID)
				if operation == "publish" {
					require.NoError(t, s.StageEmbeddingSet(t.Context(), record))
				}
				var exported bytes.Buffer
				require.NoError(t, s.ExportMetadata(t.Context(), &exported))
				restored := newTestStore(t)
				require.NoError(t, restored.ImportMetadata(t.Context(), &exported))
				blobs := map[string]string{"source": catalogSourceHash, "vectors": record.VectorSet.PayloadBlobHash,
					"generation": record.InputGeneration.GenerationBlobHash, "evidence": testSHA256(record.InputGeneration.EvidenceJSON)}
				var size int64
				require.NoError(t, restored.db.QueryRow(`SELECT size FROM blobs WHERE hash=?`, blobs[missing]).Scan(&size))
				hash, err := packstore.ParseHash(blobs[missing])
				require.NoError(t, err)
				packID := pack.NewPackID()
				catalog := NewPackCatalog(restored)
				require.NoError(t, catalog.RecordPack(t.Context(), packstore.PackRecord{
					PackID: packID, EntryCount: 1, StoredBytes: size + pack.MinEntryOffset,
					CreatedAt: time.Now().UTC(),
				}, []packstore.Adoption{{Entry: packstore.IndexEntry{
					Hash: hash, PackID: packID, Offset: pack.MinEntryOffset, StoredLen: size, RawLen: size,
				}}}))
				require.NoError(t, catalog.DeleteIndexEntry(t.Context(), hash))
				_, err = restored.PhysicalContent(t.Context(), blobs[missing])
				require.ErrorIs(t, err, ErrPhysicalAuthorityMissing)
				write := func() error {
					if operation == "stage" {
						return restored.StageEmbeddingSet(t.Context(), record)
					}
					return restored.PublishEmbeddingHead(t.Context(), EmbeddingHeadRecord{
						Key:   EmbeddingHeadKey{ContentVersionID: versionID, BindingID: record.BindingID, InputKind: record.InputKind},
						SetID: record.ID, VectorSpaceID: record.VectorSpace.ID,
						ProcessingProfileFingerprint: profile.Fingerprint, PublishedAt: embeddingCatalogTime,
					})
				}
				require.ErrorIs(t, write(), ErrPhysicalAuthorityMissing)
				_, err = restored.RepairBlobAuthority(t.Context(), blobs[missing], size, BlobPhysical{Encoding: "raw", StoredBytes: size})
				require.NoError(t, err)
				require.NoError(t, write())
			})
		}
	}
}
