package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func bestShotPreparation(t *testing.T, s *Store, ids []string) PhotoAuthoredPreparation {
	t.Helper()
	prep, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets})
	require.NoError(t, err)
	return prep
}
func bestShotRequest(t *testing.T, s *Store, ids []string) PhotoStackDecision {
	t.Helper()
	prep := bestShotPreparation(t, s, ids)
	settings, err := s.PhotoSettings(t.Context())
	require.NoError(t, err)
	request := PhotoStackDecision{AssetIDs: ids, BurstGapSeconds: settings.BurstGapSeconds, Decision: "all", CurrentID: ids[0]}
	for _, t := range prep.Targets {
		request.Targets = append(request.Targets, PhotoAuthoredEditTarget{PhotoAuthoredTargetRevision: t.PhotoAuthoredTargetRevision})
	}
	for _, p := range prep.Proposals {
		request.Responses = append(request.Responses, PhotoBestShotResponse{AssetID: p.AssetID, Flag: p.Flag, Response: "accepted"})
	}
	return request
}
func seedBestShots(t *testing.T, s *Store, ids []string) {
	t.Helper()
	prep, err := s.PreparePhotoAuthored(t.Context(), PhotoSetSelection{AssetIDs: ids})
	require.NoError(t, err)
	for i, target := range prep.Targets {
		node, err := s.NodeByID(t.Context(), target.NodeID)
		require.NoError(t, err)
		require.NoError(t, s.PublishPhotoQualitySignals(t.Context(), qualityTarget(node), document.PhotoQualitySignals{Focus: 0.9 - float64(i)*0.3}, ""))
	}
}
func dismissBestShots(t *testing.T, s *Store, ids []string) PhotoAuthoredReceipt {
	t.Helper()
	request := bestShotRequest(t, s, ids)
	require.NotEmpty(t, request.Responses)
	for i := range request.Responses {
		request.Responses[i].Response = "dismissed"
	}
	receipt, err := s.DecidePhotoStack(t.Context(), request)
	require.NoError(t, err)
	return receipt
}

func TestPhotoBestShotsDecisions(t *testing.T) {
	t.Parallel()
	for _, response := range []string{"accepted", "dismissed"} {
		t.Run(response, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			ids := burstPhotos(t, s, 3)
			seedBestShots(t, s, ids[:2])
			if response == "dismissed" {
				seedBestShots(t, s, ids[2:])
			}
			request := bestShotRequest(t, s, ids)
			if response == "accepted" {
				require.Len(t, request.Responses, 2)
			} else {
				require.Len(t, request.Responses, 3)
			}
			for i := range request.Responses {
				request.Responses[i].Response = response
			}
			receipt, err := s.DecidePhotoStack(ctx, request)
			require.NoError(t, err)
			require.NotEmpty(t, receipt.ReceiptID)
			var count int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_change_receipts WHERE receipt_id=?`, receipt.ReceiptID).Scan(&count))
			require.Equal(t, 1, count)
			require.Len(t, receipt.After, len(request.Responses))
			require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
			if response == "dismissed" {
				for _, target := range receipt.After {
					require.Empty(t, target.Values.Flag)
					require.NotZero(t, target.Values.Confirmed&PhotoConfirmedFlag)
				}
				_, err = s.db.Exec(`UPDATE photo_quality_signals SET focus=1.0-focus`)
				require.NoError(t, err)
				require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
			}
			_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			require.Len(t, bestShotPreparation(t, s, ids).Proposals, len(request.Responses))
		})
	}
}
func TestPhotoBestShotsReadsAndStaleOffers(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 3)
	seedBestShots(t, s, ids)
	events := watchPhotoChanges(t, s)
	request := bestShotRequest(t, s, ids)
	request.Responses[0].Response = "dismissed"
	_, err := s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrInvalidPhotoAsset)
	request.Responses[0].Response = "accepted"
	subset := bestShotPreparation(t, s, ids[:2])
	require.Len(t, subset.Proposals, 2)
	require.Len(t, bestShotPreparation(t, s, ids).Proposals, 3)
	require.Empty(t, events)
	var count int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_suggestions`).Scan(&count))
	require.Zero(t, count)
	// Fresh measurements with the same badges are still the displayed choice.
	_, err = s.db.Exec(`UPDATE photo_quality_signals SET focus=focus*0.9`)
	require.NoError(t, err)
	receipt, err := s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	request = bestShotRequest(t, s, ids)
	_, err = s.db.Exec(`UPDATE photo_quality_signals SET focus=1.0-focus`)
	require.NoError(t, err)
	_, err = s.DecidePhotoStack(ctx, request)
	require.ErrorIs(t, err, ErrStalePhotoStack)
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM photo_suggestions`).Scan(&count))
	require.Zero(t, count)
	for _, target := range bestShotPreparation(t, s, ids).Targets {
		require.Empty(t, target.Values.Flag)
	}
}
func TestPhotoBestShotsFailedEnrichmentPublishesNothing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	seedBestShots(t, s, ids)
	missing, err := newUUIDv4()
	require.NoError(t, err)
	stacks := []PhotoStack{{AssetIDs: ids}, {AssetIDs: []string{missing, ids[0]}}}
	require.Error(t, s.PhotoStackBestShots(t.Context(), stacks, false))
	require.Empty(t, stacks[0].Proposals)
	require.Empty(t, stacks[1].Proposals)
}
func TestPhotoBestShotsRejectedBestAndCompleteTargets(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 3)
	seedBestShots(t, s, ids)
	prep := bestShotPreparation(t, s, ids)
	picked := prep.Targets[len(prep.Targets)-1]
	owner, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{{PhotoAuthoredTargetRevision: picked.PhotoAuthoredTargetRevision}}, Patch: PhotoAuthoredPatch{Flag: new("pick")}})
	require.NoError(t, err)
	require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
	_, err = s.UndoPhotoAuthored(ctx, owner.ReceiptID)
	require.NoError(t, err)
	prep = bestShotPreparation(t, s, ids)
	winner := prep.Proposals[0].AssetID
	for _, target := range prep.Targets {
		if target.AssetID == winner {
			unflagged, err := s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}}, Patch: PhotoAuthoredPatch{Flag: new("")}})
			require.NoError(t, err)
			offers := bestShotPreparation(t, s, ids).Proposals
			require.Len(t, offers, 2)
			require.Equal(t, "pick", offers[0].Flag)
			require.NotEqual(t, winner, offers[0].AssetID)
			_, err = s.UndoPhotoAuthored(ctx, unflagged.ReceiptID)
			require.NoError(t, err)
		}
	}
	prep = bestShotPreparation(t, s, ids)
	for _, target := range prep.Targets {
		if target.AssetID == winner {
			_, err = s.EditPhotoAuthoredBatch(ctx, PhotoAuthoredBatch{Targets: []PhotoAuthoredEditTarget{{PhotoAuthoredTargetRevision: target.PhotoAuthoredTargetRevision}}, Patch: PhotoAuthoredPatch{Flag: new("reject")}})
			require.NoError(t, err)
		}
	}
	request := bestShotRequest(t, s, ids)
	require.Len(t, request.Responses, 2)
	_, err = s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	picks := 0
	for _, target := range bestShotPreparation(t, s, ids).Targets {
		if target.Values.Flag == "pick" {
			picks++
		}
	}
	require.Equal(t, 1, picks)
}
func TestPhotoBestShotsPairedCompletenessAndAudit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 2)
	first, err := s.PhotoAssetByID(ctx, ids[0])
	require.NoError(t, err)
	raw, err := s.CreateFile(ctx, s.RootID(), "frame-0000.cr2", fakeHash("d4"), 4, "application/octet-stream")
	require.NoError(t, err)
	_, err = s.AttachPhotoFile(ctx, first.ID, first.Revision, raw.ID, PhotoRoleRAW, nil)
	require.NoError(t, err)
	_, err = s.SetPhotoSettings(ctx, 1, PhotoSettingsUpdate{Preference: new("image")})
	require.NoError(t, err)
	seedBestShots(t, s, ids)
	request := bestShotRequest(t, s, ids)
	require.Len(t, request.Targets, 3)
	receipt, err := s.DecidePhotoStack(ctx, request)
	require.NoError(t, err)
	require.Len(t, receipt.After, 3)
	_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
	require.NoError(t, err)
	_, err = s.initializeAuditAuthority(ctx, s.RootID(), "api", nil)
	require.NoError(t, err)
	for _, response := range []string{"accepted", "dismissed"} {
		request = bestShotRequest(t, s, ids)
		require.Len(t, request.Responses, 2)
		for i := range request.Responses {
			request.Responses[i].Response = response
		}
		receipt, err = s.DecidePhotoStack(ctx, request)
		require.NoError(t, err)
		require.Len(t, receipt.After, 3)
		require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
		_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
		require.NoError(t, err)
	}
}
func TestPhotoBestShotsStaleFaces(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := t.Context()
	ids := burstPhotos(t, s, 3)
	seedBestShots(t, s, ids)
	prep := bestShotPreparation(t, s, ids)
	target := prep.Targets[0]
	id, err := newUUIDv4()
	require.NoError(t, err)
	face, _, err := s.CreatePhotoFace(ctx, target.CurrentContentVersionID, id, document.FaceBox{Width: 0.5, Height: 0.5}, "")
	require.NoError(t, err)
	// Whole-photo measurements remain visible, but the stale-face frame is unmeasured for comparison.
	offers := bestShotPreparation(t, s, ids).Proposals
	require.Len(t, offers, 2)
	for _, p := range offers {
		require.NotEqual(t, target.AssetID, p.AssetID)
	}
	node, err := s.NodeByID(ctx, target.NodeID)
	require.NoError(t, err)
	_, inputs, err := s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	signals := document.PhotoQualitySignals{Focus: 0.9, FaceReports: []document.PhotoFaceReport{{FaceID: face.FaceID, Sharpness: new(0.7), Prominence: 0.25}}}
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), signals, inputs))
	require.Len(t, bestShotPreparation(t, s, ids).Proposals, 3)
	_, err = s.db.Exec(`UPDATE photo_faces SET revision=revision+1 WHERE face_id=?`, face.FaceID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), signals, inputs))
	require.Len(t, bestShotPreparation(t, s, ids).Proposals, 3)
	_, err = s.db.Exec(`UPDATE photo_faces SET x=0.1,width=0.4,revision=revision+1 WHERE face_id=?`, face.FaceID)
	require.NoError(t, err)
	require.Len(t, bestShotPreparation(t, s, ids).Proposals, 2)
	_, inputs, err = s.PhotoQualityFaces(ctx, node.CurrentVersionID)
	require.NoError(t, err)
	require.NoError(t, s.PublishPhotoQualitySignals(ctx, qualityTarget(node), signals, inputs))
	require.Len(t, bestShotPreparation(t, s, ids).Proposals, 3)
}
func TestPhotoBestShotsNewMeasurement(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 3)
	seedBestShots(t, s, ids[:2])
	dismissBestShots(t, s, ids)
	seedBestShots(t, s, ids[2:])
	// A newly measured frame has no undecided peer to compare with.
	require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
	request := bestShotRequest(t, s, ids)
	request.Decision = "current"
	request.CurrentID = ids[2]
	_, err := s.DecidePhotoStack(t.Context(), request)
	require.NoError(t, err)
	for _, target := range bestShotPreparation(t, s, ids).Targets {
		if target.AssetID == ids[2] {
			require.Equal(t, "pick", target.Values.Flag)
		} else {
			require.Empty(t, target.Values.Flag)
			require.Empty(t, target.Values.FlagOwner)
		}
	}
}

func TestPhotoBestShotsDismissalUndoReplay(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ids := burstPhotos(t, s, 2)
	seedBestShots(t, s, ids)
	first := dismissBestShots(t, s, ids)
	_, err := s.UndoPhotoAuthored(t.Context(), first.ReceiptID)
	require.NoError(t, err)
	dismissBestShots(t, s, ids)
	_, err = s.UndoPhotoAuthored(t.Context(), first.ReceiptID)
	require.ErrorIs(t, err, ErrStaleRevision)
	require.Empty(t, bestShotPreparation(t, s, ids).Proposals)
}
