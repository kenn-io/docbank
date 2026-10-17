package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/phototest"
)

func putPhotoSuggestion(t *testing.T, s *Store, asset PhotoAsset, description document.PhotoDescription) PhotoSuggestion {
	t.Helper()
	ctx := t.Context()
	file := asset.Files[0]
	for _, candidate := range asset.Files {
		if candidate.ID == *asset.DisplayFileID {
			file = candidate
		}
	}
	node, err := s.NodeByID(ctx, file.NodeID)
	require.NoError(t, err)
	_, err = phototest.SeedSuggestion(ctx, s.db, node.CurrentVersionID, description)
	require.NoError(t, err)
	values, err := s.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Len(t, values, 1)
	return values[0]
}
func TestPhotoSuggestionSearchRanksBelowAuthoredAndDismissRemovesMatch(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	machine := albumAsset(t, s, "machine.jpg")
	suggestion := putPhotoSuggestion(t, s, machine, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}, VisibleText: "QUAYSIDE"})
	authored := albumAsset(t, s, "authored.jpg")
	_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: authored.Files[0].ID, Revision: authored.Files[0].Revision, Patch: PhotoAuthoredPatch{Caption: new("harbor")}}})
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{"text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, page.Items, 2)
	require.Equal(t, authored.ID, page.Items[0].AssetID)
	require.False(t, page.Items[0].MatchedAISuggestion)
	require.Equal(t, machine.ID, page.Items[1].AssetID)
	require.True(t, page.Items[1].MatchedAISuggestion)
	page = browsePhotoPage(t, s, `{"text":"coast"}`)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].MatchedAISuggestion)
	page = browsePhotoPage(t, s, `{"text":"QUAYSIDE"}`)
	require.Len(t, page.Items, 1)
	require.Equal(t, machine.ID, page.Items[0].AssetID)
	require.True(t, page.Items[0].MatchedAISuggestion)
	page = browsePhotoPage(t, s, `{"text":"harbor coast"}`)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].MatchedAISuggestion)
	_, err = s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Revision: 1, Response: "dismissed"})
	require.NoError(t, err)
	require.Empty(t, browsePhotoPage(t, s, `{"text":"coast"}`).Items)
	require.Empty(t, browsePhotoPage(t, s, `{"text":"QUAYSIDE"}`).Items)
	page = browsePhotoPage(t, s, `{"text":"harbor"}`)
	require.Len(t, page.Items, 1)
	require.Equal(t, authored.ID, page.Items[0].AssetID)
}

func TestPhotoSuggestionSearchSurvivesVacuum(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	removed := albumAsset(t, s, "removed.jpg")
	first := putPhotoSuggestion(t, s, removed, document.PhotoDescription{Caption: "A meadow.", Tags: []string{}})
	retained := albumAsset(t, s, "retained.jpg")
	putPhotoSuggestion(t, s, retained, document.PhotoDescription{Caption: "A harbor.", Tags: []string{}})
	_, err := s.db.ExecContext(ctx, `DELETE FROM photo_suggestions WHERE suggestion_id=?`, first.ID)
	require.NoError(t, err)
	before := browsePhotoPage(t, s, `{"text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, before.Items, 1)
	require.Equal(t, retained.ID, before.Items[0].AssetID)
	_, err = s.db.ExecContext(ctx, `VACUUM`)
	require.NoError(t, err)
	after := browsePhotoPage(t, s, `{"text":"harbor","sort":{"field":"relevance","direction":"desc"}}`)
	require.Len(t, after.Items, 1)
	require.Equal(t, retained.ID, after.Items[0].AssetID)
	require.True(t, after.Items[0].MatchedAISuggestion)
}
func TestHiddenPhotoSuggestionRequiresUnlock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "hidden.jpg")
	suggestion := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A photo.", Tags: []string{}})
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	_, err := s.SetPhotoAssetHidden(ctx, asset.ID, asset.Revision, true)
	require.NoError(t, err)
	_, err = s.PhotoSuggestions(ctx, asset.ID)
	require.ErrorIs(t, err, ErrHiddenLocked)
	_, err = s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Revision: 1, Response: "dismissed"})
	require.ErrorIs(t, err, ErrHiddenLocked)
	token, _, err := s.UnlockPhotoHidden(ctx, "synthetic-passcode")
	require.NoError(t, err)
	_, err = s.RespondPhotoSuggestion(WithPhotoHiddenToken(ctx, token), suggestion.ID, PhotoSuggestionResponse{Revision: 1, Response: "dismissed"})
	require.NoError(t, err)
}

func TestPhotoSuggestionResponsesExtendAuditHistory(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "audited.jpg")
	pending := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A coast.", Tags: []string{"coast"}})
	plan, err := s.PreviewInitialAudit(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	_, err = s.EnableInitialAudit(ctx, plan)
	require.NoError(t, err)
	_, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Caption: true, Tags: []string{"coast"}})
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	before, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	values, err := s.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Revision: values[0].Revision, Response: "dismissed"})
	require.NoError(t, err)
	after, err := s.NodeByID(ctx, before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Revision, after.Revision)
	require.NoError(t, s.ValidateMetadata(ctx))
	assertAuditMetadataRoundTrip(t, s)
}

func suggestionTarget(t *testing.T, s *Store, asset PhotoAsset) *PhotoAuthoredEditTarget {
	t.Helper()
	prepared, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: []string{asset.ID}})
	require.NoError(t, err)
	for _, target := range prepared.Targets {
		if target.FileID == *asset.DisplayFileID {
			return &target.PhotoAuthoredEditTarget
		}
	}
	t.Fatal("display photo missing from preparation")
	return nil
}

func TestPhotoSuggestionAcceptanceUsesReviewedRevisions(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"caption", "asset", "node", "missing_file", "wrong_asset"} {
		t.Run(change, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "reviewed.jpg")
			pending := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "An older caption."})
			target := suggestionTarget(t, s, asset)
			switch change {
			case "missing_file":
				target.FileID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
			case "wrong_asset":
				target.AssetID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
			case "caption":
				_, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{*target}, Patch: PhotoAuthoredPatch{Caption: new("A newer caption.")}})
				require.NoError(t, err)
			case "asset":
				_, err := s.SetPhotoAssetExcluded(ctx, asset.ID, asset.Revision, true)
				require.NoError(t, err)
			case "node":
				tag, err := s.CreateTag(ctx, "reviewed")
				require.NoError(t, err)
				_, err = s.AssignTag(ctx, tag.ID, asset.Files[0].NodeID, target.NodeRevision)
				require.NoError(t, err)
			}
			_, err := s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*target}, Revision: 1, Response: "accepted", Caption: true})
			require.ErrorIs(t, err, ErrPhotoSuggestionConflict)
			values, err := s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Equal(t, "pending", values[0].Response)
			current, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			if change == "caption" {
				require.Equal(t, "A newer caption.", current.Files[0].Caption)
			}
		})
	}
}

func TestPhotoSuggestionSupersededVersionRetiresOnlyPending(t *testing.T) {
	t.Parallel()
	for _, response := range []string{"pending", "accepted", "dismissed"} {
		t.Run(response, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "replaced.jpg")
			proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{}, VisibleText: "QUAYSIDE"})
			if response != "pending" {
				request := PhotoSuggestionResponse{Revision: proposal.Revision, Response: response}
				if response == "accepted" {
					request.Caption = true
					request.Targets = []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}
				}
				_, err := s.RespondPhotoSuggestion(ctx, proposal.ID, request)
				require.NoError(t, err)
			}
			node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
			require.NoError(t, err)
			_, _, err = s.ReplaceContent(ctx, node.ID, node.Revision, fakeHash("ab"), 10, "image/jpeg")
			require.NoError(t, err)
			var remaining int
			require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_suggestions WHERE suggestion_id=?`, proposal.ID).Scan(&remaining))
			if response == "pending" {
				require.Zero(t, remaining, "superseded pending text must leave retained storage")
				require.NoError(t, s.db.QueryRowContext(ctx, `SELECT count(*) FROM photo_suggestion_fts WHERE photo_suggestion_fts MATCH 'QUAYSIDE'`).Scan(&remaining))
				require.Zero(t, remaining)
			} else {
				require.Equal(t, 1, remaining, "source-version owner decisions remain retained")
			}
		})
	}
}

func TestPhotoSuggestionDecisionSurvivesDisplaySwap(t *testing.T) {
	t.Parallel()
	for _, response := range []string{"accepted", "dismissed"} {
		t.Run(response, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := authoredPair(t, s)
			jpeg := fileByRole(asset.Files, PhotoRoleImage)
			raw := fileByRole(asset.Files, PhotoRoleRAW)
			asset, err := s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpeg.ID)
			require.NoError(t, err)
			node, err := s.NodeByID(ctx, raw.NodeID)
			require.NoError(t, err)
			recipe, err := document.BuiltInVisualPreviewRecipe("grid")
			require.NoError(t, err)
			_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, fakeHash("c3"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
			require.NoError(t, err)
			proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}, VisibleText: "QUAYSIDE"})
			request := PhotoSuggestionResponse{Revision: proposal.Revision, Response: response}
			if response == "accepted" {
				request.Caption = true
				request.Targets = []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}
			}
			decision, err := s.RespondPhotoSuggestion(ctx, proposal.ID, request)
			require.NoError(t, err)
			asset, err = s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &raw.ID)
			require.NoError(t, err)

			values, err := s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Empty(t, values)
			require.Empty(t, browsePhotoPage(t, s, `{"text":"QUAYSIDE"}`).Items)
			targets, err := s.PhotoDescriptionTargets(ctx, strings.Repeat("a", 64), "", 50)
			require.NoError(t, err)
			require.Len(t, targets, 1)
			require.Equal(t, node.CurrentVersionID, targets[0].ID)
			rawProposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A RAW harbor.", Tags: []string{}, VisibleText: "RAWQUAY"})
			values, err = s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Equal(t, []PhotoSuggestion{rawProposal}, values)
			asset, err = s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpeg.ID)
			require.NoError(t, err)
			values, err = s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Len(t, values, 1)
			require.Equal(t, decision.ID, values[0].ID)
			require.Equal(t, response, values[0].Response)
			require.Empty(t, browsePhotoPage(t, s, `{"text":"RAWQUAY"}`).Items)
			var exported bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &exported))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
			values, err = restored.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Len(t, values, 1)
			require.Equal(t, decision.ID, values[0].ID)
			require.Equal(t, response, values[0].Response)
		})
	}
}

func TestPhotoSuggestionAcceptsFieldsInEitherOrder(t *testing.T) {
	t.Parallel()
	for _, captionFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("caption_first_%t", captionFirst), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "tags.jpg")
			pending := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A coast.", Tags: []string{"coast", "harbor"}, VisibleText: "QUAYSIDE"})
			var firstTags []string
			if !captionFirst {
				firstTags = pending.Value.Tags
			}
			accepted, err := s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Caption: captionFirst, Tags: firstTags})
			require.NoError(t, err)
			require.Equal(t, "accepted", accepted.Response)
			decisionReceipt, firstAuthoredReceipt := accepted.ReceiptID, accepted.AuthoredReceiptID
			var operation string
			require.NoError(t, s.db.QueryRow(`SELECT operation FROM photo_change_receipts WHERE receipt_id=?`, *decisionReceipt).Scan(&operation))
			require.Equal(t, "authored", operation)
			var receiptsBefore int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM photo_change_receipts`).Scan(&receiptsBefore))
			_, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: accepted.Revision, Response: "accepted", Caption: captionFirst, Tags: firstTags})
			require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse)
			require.ErrorContains(t, err, "selected fields are already applied")
			var receiptsAfter int
			require.NoError(t, s.db.QueryRow(`SELECT count(*) FROM photo_change_receipts`).Scan(&receiptsAfter))
			require.Equal(t, receiptsBefore, receiptsAfter)
			unchanged, err := s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Equal(t, accepted.Revision, unchanged[0].Revision)
			require.Equal(t, accepted.ReceiptID, unchanged[0].ReceiptID)
			accepted, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: accepted.Revision, Response: "accepted", Caption: !captionFirst, Tags: pending.Value.Tags})
			require.NoError(t, err)
			current, err := s.PhotoAssetByID(ctx, asset.ID)
			require.NoError(t, err)
			require.Equal(t, "A coast.", current.Files[0].Caption)
			tags, _, err := s.NodeTags(ctx, asset.Files[0].NodeID, 100, 0)
			require.NoError(t, err)
			require.Len(t, tags, 2)
			require.Equal(t, decisionReceipt, accepted.ReceiptID)
			require.NotEqual(t, firstAuthoredReceipt, accepted.AuthoredReceiptID)
			require.NotNil(t, accepted.AuthoredReceipt)
			require.Equal(t, accepted.AuthoredReceiptID, accepted.AuthoredReceipt.ReceiptID)
			require.Equal(t, "accepted", accepted.Response)
			_, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: accepted.Revision, Response: "accepted", Tags: []string{"harbor"}})
			require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse)
			require.ErrorContains(t, err, "selected fields are already applied")
		})
	}
}

func TestPhotoDescriptionTargetsFiltersBeforeLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	recipe, err := document.BuiltInVisualPreviewRecipe("grid")
	require.NoError(t, err)
	require.NoError(t, s.SetupPhotoHidden(ctx, "synthetic-passcode"))
	blocked := albumAsset(t, s, "hidden.jpg")
	original, err := s.NodeByID(ctx, blocked.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.SetPhotoAssetHidden(ctx, blocked.ID, blocked.Revision, true)
	require.NoError(t, err)
	for i := range 51 {
		node, err := s.CreateFile(ctx, s.RootID(), fmt.Sprintf("copy-%d.jpg", i), original.BlobHash, original.Size, "image/jpeg")
		require.NoError(t, err)
		duplicate, err := s.PhotoAssetForNode(ctx, node.ID)
		require.NoError(t, err)
		_, err = s.SetPhotoAssetHidden(ctx, duplicate.ID, duplicate.Revision, true)
		require.NoError(t, err)
		_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash(fmt.Sprintf("preview-%d", i)), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
		require.NoError(t, err)
	}
	eligible := albumAsset(t, s, "eligible.jpg")
	node, err := s.NodeByID(ctx, eligible.Files[0].NodeID)
	require.NoError(t, err)
	// Set a deterministic version identity before any preview references it.
	require.NoError(t, s.withStorageTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE content_versions SET version_id='ffffffff-ffff-4fff-bfff-ffffffffffff' WHERE version_id=?`, node.CurrentVersionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE nodes SET current_version_id='ffffffff-ffff-4fff-bfff-ffffffffffff' WHERE id=?`, node.ID)
		return err
	}))
	node.CurrentVersionID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
	_, err = s.PublishVisualPreviewGeneration(ctx, node.CurrentVersionID, readyVisualPreviewWithRecipe(t, node.BlobHash, browseHash("eligible-preview"), 9, recipe), &BlobPhysical{Encoding: looseEncodingRaw, StoredBytes: 9})
	require.NoError(t, err)
	targets, err := s.PhotoDescriptionTargets(ctx, strings.Repeat("b", 64), "", 50)
	require.NoError(t, err)
	require.Len(t, targets, 1)
	require.Equal(t, node.ID, targets[0].NodeID)
}

func TestPhotoSuggestionTrashEmptyRemovesExportedText(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "purged.jpg")
	suggestion := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "Purged proposal", Tags: []string{}})
	accepted, err := s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Caption: true})
	require.NoError(t, err)
	_, err = s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Revision: accepted.Revision, Response: "dismissed"})
	require.NoError(t, err)
	other := albumAsset(t, s, "retained.jpg")
	retained := putPhotoSuggestion(t, s, other, document.PhotoDescription{Caption: "Retained proposal", Tags: []string{}})
	_, err = s.RespondPhotoSuggestion(ctx, retained.ID, PhotoSuggestionResponse{Revision: 1, Response: "dismissed"})
	require.NoError(t, err)
	asset, err = s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	_, err = s.TrashPhotoAsset(ctx, asset.ID, asset.Revision)
	require.NoError(t, err)
	_, err = s.TrashEmpty(ctx, 0, true)
	require.NoError(t, err)
	require.NoError(t, s.ValidateMetadata(ctx))
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &exported))
	require.NotContains(t, exported.String(), "Purged proposal")
	require.Contains(t, exported.String(), "Retained proposal")
}

func TestPhotoSuggestionRejectsEmptyCaptionWithoutWriting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "empty-caption.jpg")
	_, err := editPhotoAuthored(ctx, s, []PhotoAuthoredTarget{{FileID: asset.Files[0].ID, Revision: asset.Files[0].Revision, Patch: PhotoAuthoredPatch{Caption: new("Owner caption")}}})
	require.NoError(t, err)
	value := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "", Tags: []string{"coast"}, VisibleText: ""})
	_, err = s.RespondPhotoSuggestion(ctx, value.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: value.Revision, Response: "accepted", Caption: true})
	require.ErrorIs(t, err, ErrInvalidPhotoSuggestionResponse)
	current, err := s.PhotoAssetByID(ctx, asset.ID)
	require.NoError(t, err)
	require.Equal(t, "Owner caption", current.Files[0].Caption)
	values, err := s.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	require.Equal(t, "pending", values[0].Response)
	require.Equal(t, int64(1), values[0].Revision)
	require.Nil(t, values[0].ReceiptID)
}

func TestPhotoSuggestionAcceptanceSelectedMembers(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"undo", "purge", "regroup"} {
		for _, scope := range []string{"raw", "image", "pair"} {
			if scope == "image" && action != "undo" {
				continue
			}
			t.Run(action+"/"+scope, func(t *testing.T) {
				s := newTestStore(t)
				ctx := t.Context()
				asset := authoredPair(t, s)
				jpeg := fileByRole(asset.Files, PhotoRoleImage)
				asset, err := s.SetPhotoDisplay(ctx, asset.ID, asset.Revision, &jpeg.ID)
				require.NoError(t, err)
				suggestion := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast"}})
				if action == "purge" && scope == "pair" {
					var pendingExport bytes.Buffer
					require.NoError(t, s.ExportMetadata(ctx, &pendingExport))
					require.NotContains(t, pendingExport.String(), `"type":"photo_suggestion"`)
				}
				prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, err)
				var selected []PhotoAuthoredEditTarget
				for _, target := range prepared.Targets {
					file := fileByRole(asset.Files, scope)
					if scope == "pair" || target.FileID == file.ID {
						selected = append(selected, target.PhotoAuthoredEditTarget)
					}
				}
				accepted, err := s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Targets: selected, Revision: 1, Response: "accepted", Caption: true, Tags: []string{"coast"}})
				require.NoError(t, err)
				require.NotEmpty(t, accepted.AuthoredReceiptID)
				require.Len(t, accepted.AuthoredReceipt.Before, len(selected))
				require.Len(t, accepted.AuthoredReceipt.After, len(selected))
				current, err := s.PhotoAssetByID(ctx, asset.ID)
				require.NoError(t, err)
				for _, file := range current.Files {
					want := scope == "pair" || file.Role == scope
					require.Equal(t, want, file.Caption == "A harbor.")
					tags, _, err := s.NodeTags(ctx, file.NodeID, 100, 0)
					require.NoError(t, err)
					require.Equal(t, want, len(tags) == 1)
				}

				switch action {
				case "undo":
					_, err = s.UndoPhotoAuthored(ctx, accepted.AuthoredReceiptID)
					require.NoError(t, err)
					current, err = s.PhotoAssetByID(ctx, asset.ID)
					require.NoError(t, err)
					for _, file := range current.Files {
						require.Empty(t, file.Caption)
						tags, _, err := s.NodeTags(ctx, file.NodeID, 100, 0)
						require.NoError(t, err)
						require.Empty(t, tags)
					}
				case "purge":
					raw := fileByRole(asset.Files, PhotoRoleRAW)
					current, err := s.PhotoAssetByID(ctx, asset.ID)
					require.NoError(t, err)
					_, err = s.DetachPhotoFile(ctx, asset.ID, current.Revision, raw.ID, PhotoDetachOptions{})
					require.NoError(t, err)
					node, err := s.NodeByID(ctx, raw.NodeID)
					require.NoError(t, err)
					_, _, err = s.Trash(ctx, node.ID, node.Revision)
					require.NoError(t, err)
					_, err = s.TrashEmpty(ctx, 0, true)
					require.NoError(t, err)
					_, err = s.NodeByID(ctx, raw.NodeID)
					require.ErrorIs(t, err, ErrNotFound)
					require.NoError(t, s.ValidateMetadata(ctx))
					var exported bytes.Buffer
					require.NoError(t, s.ExportMetadata(ctx, &exported))
					require.NotContains(t, exported.String(), accepted.AuthoredReceiptID)
					restored := newTestStore(t)
					require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(exported.Bytes())))
					for _, vault := range []*Store{s, restored} {
						require.NoError(t, vault.ValidateMetadata(ctx))
						current, err := vault.PhotoAssetByID(ctx, asset.ID)
						require.NoError(t, err)
						require.Len(t, current.Files, 1)
						require.Equal(t, jpeg.ID, current.Files[0].ID)
						values, err := vault.PhotoSuggestions(ctx, asset.ID)
						require.NoError(t, err)
						require.Len(t, values, 1)
						require.Equal(t, "accepted", values[0].Response)
						require.Equal(t, suggestion.ID, values[0].ID)
					}
				case "regroup":
					raw := fileByRole(asset.Files, PhotoRoleRAW)
					asset, err = s.PhotoAssetByID(ctx, asset.ID)
					require.NoError(t, err)
					_, err = s.DetachPhotoFile(ctx, asset.ID, asset.Revision, raw.ID, PhotoDetachOptions{})
					require.NoError(t, err)
					destination := albumAsset(t, s, "destination.jpg")
					destination, err = s.AttachPhotoFile(ctx, destination.ID, destination.Revision, raw.NodeID, PhotoRoleRAW, nil)
					require.NoError(t, err)
					prepared, err = s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{destination.ID}})
					require.NoError(t, err)
					for _, target := range prepared.Targets {
						if target.FileID == raw.ID {
							_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{target.PhotoAuthoredEditTarget}, Patch: PhotoAuthoredPatch{Caption: new("Regrouped capture")}})
							require.NoError(t, err)
						}
					}
					var metadata bytes.Buffer
					require.NoError(t, s.ExportMetadata(ctx, &metadata))
					restored := newTestStore(t)
					require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
					for _, catalog := range []*Store{s, restored} {
						page := browsePhotoPage(t, catalog, `{"text":"coast","sort":{"field":"relevance","direction":"desc"}}`)
						matches := map[string]bool{}
						for _, item := range page.Items {
							matches[item.AssetID] = item.MatchedAISuggestion
						}
						matchedAI, exists := matches[destination.ID]
						require.True(t, exists, "the regrouped RAW keeps its accepted tag searchable")
						require.False(t, matchedAI, "the accepted tag remains an authored match")
						require.Contains(t, matches, asset.ID)
						require.Equal(t, scope != "pair", matches[asset.ID], "acceptance provenance must remain specific to the edited files")
					}
				}
			})
		}
	}
}

func TestDerivativePurgeRemovesPendingPhotoSuggestions(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"version", "all", "attachment", "build", "other_build"} {
		t.Run(scope, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			asset := albumAsset(t, s, "pending.jpg")
			node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
			require.NoError(t, err)
			request := PurgeRequest{ContentVersionIDs: []string{node.CurrentVersionID}}
			if scope == "all" {
				request = PurgeRequest{All: true}
			}
			producer := strings.Repeat("a", 64)
			if scope == "attachment" || scope == "build" || scope == "other_build" {
				profile := catalogProcessingProfile(t, false)
				jobRequest := renditionJobTestRequest(node.CurrentVersionID, profile)
				jobRequest.ExecutionIdentity.Upload.SHA256 = node.BlobHash
				jobRequest.ExecutionIdentity.Authorization.SourceSHA256 = node.BlobHash
				grantRenditionJobConsent(t, s, jobRequest)
				job, waiter, err := s.EnqueueRenditionJob(ctx, jobRequest)
				require.NoError(t, err)
				if scope != "other_build" {
					producer = profile.RenditionRequestFingerprint
				}
				request = PurgeRequest{AttachmentIDs: []string{waiter.AttachmentID}}
				if scope == "build" || scope == "other_build" {
					request = PurgeRequest{BuildIDs: []string{job.ID}}
				}
			}
			before, err := s.DerivativePurgeFingerprint(ctx, request)
			require.NoError(t, err)
			pending := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "lighthouse", Tags: []string{"seaside"}, VisibleText: "QUAYSIDE"})
			_, err = s.db.ExecContext(ctx, `UPDATE photo_suggestions SET producer_fingerprint=? WHERE suggestion_id=?`, producer, pending.ID)
			require.NoError(t, err)
			selectedPending, err := s.DerivativePurgeFingerprint(ctx, request)
			require.NoError(t, err)
			if scope == "other_build" {
				require.Equal(t, before, selectedPending)
				_, err = s.PurgeDerivatives(ctx, request)
				require.NoError(t, err)
				values, err := s.PhotoSuggestions(ctx, asset.ID)
				require.NoError(t, err)
				require.Len(t, values, 1)
				require.Equal(t, pending.ID, values[0].ID)
				require.Len(t, browsePhotoPage(t, s, `{"text":"lighthouse"}`).Items, 1)
				return
			}
			other := albumAsset(t, s, "unselected.jpg")
			putPhotoSuggestion(t, s, other, document.PhotoDescription{Caption: "unselected"})
			if scope == "version" {
				unselected, err := s.DerivativePurgeFingerprint(ctx, request)
				require.NoError(t, err)
				require.Equal(t, selectedPending, unselected)
			}
			decided := albumAsset(t, s, "accepted.jpg")
			ownerTag, err := s.CreateTag(ctx, "owner")
			require.NoError(t, err)
			decidedNode, err := s.NodeByID(ctx, decided.Files[0].NodeID)
			require.NoError(t, err)
			_, err = s.AssignTag(ctx, ownerTag.ID, decidedNode.ID, decidedNode.Revision)
			require.NoError(t, err)
			accepted := putPhotoSuggestion(t, s, decided, document.PhotoDescription{Caption: "Owner caption", Tags: []string{"maritime"}, VisibleText: "BREAKWATER"})
			decision, err := s.RespondPhotoSuggestion(ctx, accepted.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, decided)}, Revision: 1, Response: "accepted", Caption: true})
			require.NoError(t, err)
			dismissed := albumAsset(t, s, "dismissed.jpg")
			proposal := putPhotoSuggestion(t, s, dismissed, document.PhotoDescription{Caption: "Dismissed"})
			_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Revision: 1, Response: "dismissed"})
			require.NoError(t, err)
			after, err := s.DerivativePurgeFingerprint(ctx, request)
			require.NoError(t, err)
			require.NotEqual(t, before, after)
			_, err = s.PurgeDerivatives(ctx, PurgeRequest{})
			require.NoError(t, err)
			values, err := s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Len(t, values, 1)
			_, err = s.PurgeDerivatives(ctx, request)
			require.NoError(t, err)
			values, err = s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Empty(t, values)
			for _, term := range []string{"lighthouse", "seaside", "QUAYSIDE"} {
				require.Empty(t, browsePhotoPage(t, s, fmt.Sprintf(`{"text":%q}`, term)).Items)
			}
			fingerprint, err := s.DerivativePurgeFingerprint(ctx, request)
			require.NoError(t, err)
			require.NotEqual(t, after, fingerprint)
			values, err = s.PhotoSuggestions(ctx, other.ID)
			require.NoError(t, err)
			if scope == "all" {
				require.Empty(t, values)
			} else {
				require.Len(t, values, 1)
			}
			current, err := s.PhotoAssetByID(ctx, decided.ID)
			require.NoError(t, err)
			require.Equal(t, "Owner caption", current.Files[0].Caption)
			values, err = s.PhotoSuggestions(ctx, decided.ID)
			require.NoError(t, err)
			require.Len(t, values, 1)
			require.Equal(t, "accepted", values[0].Response)
			require.Equal(t, decision.ReceiptID, values[0].ReceiptID)
			if scope == "all" {
				require.Equal(t, []string{"maritime"}, values[0].Value.Tags)
				require.Equal(t, "BREAKWATER", values[0].Value.VisibleText)
				for _, term := range []string{"maritime", "BREAKWATER"} {
					page := browsePhotoPage(t, s, fmt.Sprintf(`{"text":%q}`, term))
					require.Len(t, page.Items, 1)
					require.Equal(t, decided.ID, page.Items[0].AssetID)
					require.True(t, page.Items[0].MatchedAISuggestion)
				}
			}
			_, err = loadPhotoAuthoredReceipt(ctx, s.db, decision.AuthoredReceiptID)
			require.NoError(t, err)
			values, err = s.PhotoSuggestions(ctx, dismissed.ID)
			require.NoError(t, err)
			require.Equal(t, "dismissed", values[0].Response)
			tags, _, err := s.NodeTags(ctx, current.Files[0].NodeID, 100, 0)
			require.NoError(t, err)
			require.Len(t, tags, 1)
			require.Equal(t, "owner", tags[0].Name)
			_, err = s.NodeByID(ctx, node.ID)
			require.NoError(t, err)
			if scope == "version" {
				value := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "Selected"})
				_, err = s.RespondPhotoSuggestion(ctx, value.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Caption: true})
				require.NoError(t, err)
				accepted, err := s.DerivativePurgeFingerprint(ctx, request)
				require.NoError(t, err)
				require.Equal(t, before, accepted)
			}
		})
	}
}

func TestPhotoSuggestionSearchBadgesOnlyRequiredText(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"accepted_caption", "filename", "companion"} {
		t.Run(source, func(t *testing.T) {
			s := newTestStore(t)
			name := "harbor.jpg"
			if source == "accepted_caption" {
				name = "plain.jpg"
			}
			var asset PhotoAsset
			if source == "companion" {
				asset = authoredPair(t, s)
			} else {
				asset = albumAsset(t, s, name)
			}
			suggestion := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "harbor", Tags: []string{"sunset"}})
			if source == "accepted_caption" {
				_, err := s.RespondPhotoSuggestion(t.Context(), suggestion.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: suggestion.Revision, Response: "accepted", Caption: true})
				require.NoError(t, err)
				current, err := s.PhotoAssetByID(t.Context(), asset.ID)
				require.NoError(t, err)
				require.Equal(t, "harbor", current.Files[0].Caption)
				retained, err := s.PhotoSuggestions(t.Context(), asset.ID)
				require.NoError(t, err)
				require.Len(t, retained, 1)
				require.Equal(t, "accepted", retained[0].Response)
				require.Equal(t, suggestion.Value, retained[0].Value)
			}
			queries := []struct {
				text  string
				badge bool
			}{{"harbor", false}, {"harbor OR sunset", false}, {"harbor AND sunset", true}, {"", false}, {"extension:jpg", false}}
			if source == "companion" {
				queries = queries[:1]
				queries[0].text, queries[0].badge = "harbor AND extension:cr2", true
			}
			for _, sort := range []string{"name", "capture_time", "relevance"} {
				for _, query := range queries {
					page := browsePhotoPage(t, s, fmt.Sprintf(`{"syntax":"advanced","text":%q,"sort":{"field":%q,"direction":"desc"}}`, query.text, sort))
					require.Len(t, page.Items, 1)
					require.Equal(t, asset.ID, page.Items[0].AssetID)
					require.Equal(t, query.badge, page.Items[0].MatchedAISuggestion, "%s, %s", query.text, sort)
				}
			}
		})
	}
}

func TestPhotoSuggestionUndoReopensOnlyRevertedFields(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"pending", "dismissed", "companion"} {
		t.Run(state, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			var asset PhotoAsset
			if state == "companion" {
				asset = authoredPair(t, s)
			} else {
				asset = albumAsset(t, s, "undo.jpg")
				coast, err := s.CreateTag(ctx, "coast")
				require.NoError(t, err)
				_, err = s.AssignTag(ctx, coast.ID, asset.Files[0].NodeID, UnconditionalRev)
				require.NoError(t, err)
			}
			proposal := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A harbor.", Tags: []string{"coast", "boats", "water"}})
			accepted, err := s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Revision: proposal.Revision, Response: "accepted", Caption: true, Tags: []string{"coast", "boats"}, Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}})
			require.NoError(t, err)
			if state == "companion" {
				prepared, err := s.PreparePhotoAuthored(ctx, PhotoSetSelection{AssetIDs: []string{asset.ID}})
				require.NoError(t, err)
				require.Len(t, prepared.Targets, 2)
				for _, target := range prepared.Targets {
					if target.FileID == *asset.DisplayFileID {
						continue
					}
					accepted, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Revision: accepted.Revision, Response: "accepted", Caption: true, Tags: []string{"coast", "boats"}, Targets: []PhotoAuthoredEditTarget{target.PhotoAuthoredEditTarget}})
					require.NoError(t, err)
				}
			}
			if state == "dismissed" {
				_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Revision: accepted.Revision, Response: "dismissed"})
				require.NoError(t, err)
			}
			_, err = s.UndoPhotoAuthored(ctx, accepted.AuthoredReceiptID)
			require.NoError(t, err)
			values, err := s.PhotoSuggestions(ctx, asset.ID)
			require.NoError(t, err)
			require.Len(t, values, 1)
			if state == "companion" {
				current, err := s.PhotoAssetByID(ctx, asset.ID)
				require.NoError(t, err)
				for _, file := range current.Files {
					if file.ID == *asset.DisplayFileID {
						require.Equal(t, "A harbor.", file.Caption)
						tags, _, err := s.NodeTags(ctx, file.NodeID, 100, 0)
						require.NoError(t, err)
						require.Len(t, tags, 2)
						require.ElementsMatch(t, []string{"coast", "boats"}, []string{tags[0].Name, tags[1].Name})
					}
				}
				require.Greater(t, values[0].Revision, accepted.Revision)
				require.NoError(t, s.ValidateMetadata(ctx))
				return
			}
			if state == "dismissed" {
				require.Equal(t, "dismissed", values[0].Response)
				require.Empty(t, browsePhotoPage(t, s, `{"text":"harbor"}`).Items)
				require.Empty(t, browsePhotoPage(t, s, `{"text":"boats"}`).Items)
				return
			}
			for _, text := range []string{"harbor", "boats", "water"} {
				page := browsePhotoPage(t, s, `{"text":"`+text+`"}`)
				require.Len(t, page.Items, 1)
				require.True(t, page.Items[0].MatchedAISuggestion)
			}
			coastPage := browsePhotoPage(t, s, `{"text":"coast"}`)
			require.Len(t, coastPage.Items, 1)
			require.False(t, coastPage.Items[0].MatchedAISuggestion)
			_, err = s.RespondPhotoSuggestion(ctx, proposal.ID, PhotoSuggestionResponse{Revision: values[0].Revision, Response: "accepted", Caption: true, Tags: []string{"boats"}, Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}})
			require.NoError(t, err)
			page := browsePhotoPage(t, s, `{"text":"harbor"}`)
			require.Len(t, page.Items, 1)
			require.False(t, page.Items[0].MatchedAISuggestion)
			require.NoError(t, s.ValidateMetadata(ctx))
		})
	}
}

func TestPhotoSuggestionTagIdentity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		upper, title, ai string
		exact            bool
	}{
		{"COAST", "Coast", "coast", false}, {"COAST", "Coast", "coast", true},
		{"CAFÉ", "Café", "cafe\u0301", false}, {"CAFÉ", "Café", "cafe\u0301", true},
	} {
		t.Run(fmt.Sprintf("%s/exact_%t", tc.ai, tc.exact), func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			first, err := s.CreateTag(ctx, tc.upper)
			require.NoError(t, err)
			_, err = s.CreateTag(ctx, tc.title)
			require.NoError(t, err)
			want := first.ID
			if tc.exact {
				tag, err := s.CreateTag(ctx, tc.ai)
				require.NoError(t, err)
				want = tag.ID
			}
			asset := albumAsset(t, s, "tags.jpg")
			pending := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Tags: []string{tc.ai}})
			_, err = s.RespondPhotoSuggestion(ctx, pending.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Tags: []string{tc.ai}})
			require.NoError(t, err)
			tags, _, err := s.NodeTags(ctx, asset.Files[0].NodeID, 100, 0)
			require.NoError(t, err)
			require.Len(t, tags, 1)
			require.Equal(t, want, tags[0].ID)
		})
	}
}

func TestOnlyAcceptedSuggestionTagsSearchAsAuthored(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	asset := albumAsset(t, s, "review.jpg")
	manual, err := s.CreateTag(ctx, "manualword")
	require.NoError(t, err)
	node, err := s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, manual.ID, node.ID, node.Revision)
	require.NoError(t, err)
	require.Empty(t, browsePhotoPage(t, s, `{"text":"manualword"}`).Items)
	suggestion := putPhotoSuggestion(t, s, asset, document.PhotoDescription{Caption: "A panorama.", Tags: []string{"coast", "headland"}})
	accepted, err := s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: 1, Response: "accepted", Caption: true})
	require.NoError(t, err)
	coast, err := s.CreateTag(ctx, "coast")
	require.NoError(t, err)
	node, err = s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.AssignTag(ctx, coast.ID, node.ID, node.Revision)
	require.NoError(t, err)
	page := browsePhotoPage(t, s, `{"text":"coast"}`)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].MatchedAISuggestion, "caption acceptance must not accept an unselected tag")
	node, err = s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.UnassignTag(ctx, coast.ID, node.ID, node.Revision)
	require.NoError(t, err)
	_, err = s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: accepted.Revision, Response: "accepted", Tags: []string{"coast"}})
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{"text":"coast"}`)
	require.Len(t, page.Items, 1)
	require.False(t, page.Items[0].MatchedAISuggestion)
	coast, err = s.TagByID(ctx, coast.ID)
	require.NoError(t, err)
	coast, err = s.RenameTag(ctx, coast.ID, coast.Revision, "shoreline")
	require.NoError(t, err)
	current, err := s.PhotoSuggestions(ctx, asset.ID)
	require.NoError(t, err)
	accepted, err = s.RespondPhotoSuggestion(ctx, suggestion.ID, PhotoSuggestionResponse{Targets: []PhotoAuthoredEditTarget{*suggestionTarget(t, s, asset)}, Revision: current[0].Revision, Response: "accepted", Tags: []string{"headland"}})
	require.NoError(t, err)
	require.Len(t, accepted.AcceptedTags, 2)
	require.Contains(t, accepted.AcceptedTags, PhotoSuggestionAcceptedTag{FileID: asset.Files[0].ID, TagID: coast.ID})
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(ctx, &metadata))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(metadata.Bytes())))
	for _, catalog := range []*Store{s, restored} {
		page := browsePhotoPage(t, catalog, `{"text":"shoreline"}`)
		require.Len(t, page.Items, 1)
		require.False(t, page.Items[0].MatchedAISuggestion)
		page = browsePhotoPage(t, catalog, `{"text":"headland"}`)
		require.Len(t, page.Items, 1)
		require.False(t, page.Items[0].MatchedAISuggestion)
		values, err := catalog.PhotoSuggestions(ctx, asset.ID)
		require.NoError(t, err)
		require.Equal(t, accepted.AcceptedTags, values[0].AcceptedTags)
	}

	node, err = s.NodeByID(ctx, asset.Files[0].NodeID)
	require.NoError(t, err)
	_, err = s.UnassignTag(ctx, coast.ID, node.ID, node.Revision)
	require.NoError(t, err)
	page = browsePhotoPage(t, s, `{"text":"coast"}`)
	require.Len(t, page.Items, 1)
	require.True(t, page.Items[0].MatchedAISuggestion)
	require.Empty(t, browsePhotoPage(t, s, `{"text":"shoreline"}`).Items)
}
