package store

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestPhotoBurstAuditEnrollmentAndRestore(t *testing.T) {
	t.Parallel()
	for _, enrollment := range []string{"before choice", "after choice", "partial scope"} {
		t.Run(enrollment, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			run, err := s.BeginIngest(ctx, "cli", "synthetic bursts")
			require.NoError(t, err)
			var ids []string
			for i := range 3 {
				ids = append(ids, stackPhoto(t, s, run, fmt.Sprintf("/shots/frame-%d.jpg", i), fmt.Sprintf("2024-06-10T12:00:0%dZ", i*2), document.SourceMetadataPrecisionSecond))
			}
			choose := func(s *Store, id string) PhotoAuthoredReceipt {
				r, err := s.DecidePhotoStack(ctx, PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: id, Decision: "representative"})
				require.NoError(t, err)
				return r
			}
			target := s.RootID()
			if enrollment == "partial scope" {
				folder, err := s.Mkdir(ctx, target, "Audited")
				require.NoError(t, err)
				target = folder.ID
			}
			var initial PhotoAuthoredReceipt
			if enrollment == "after choice" {
				initial = choose(s, ids[0])
			}
			seedInitialAuditAuthority(t, s, target)
			verify := func(s *Store) {
				require.NoError(t, s.ValidateMetadata(ctx))
				result, err := s.VerifyAudit(ctx, nil)
				require.NoError(t, err)
				require.True(t, result.Evidence.Enabled)
			}
			verify(s)
			if initial.ReceiptID != "" {
				_, err = s.UndoPhotoAuthored(ctx, initial.ReceiptID)
				require.NoError(t, err)
				verify(s)
			}
			receipt := choose(s, ids[1])
			page, err := s.ListPhotoAssets(ctx, captureRequest(t, "asc"), nil)
			require.NoError(t, err)
			require.Equal(t, ids[1], page.Stacks[0].RepresentativeID)
			for _, row := range page.Items {
				require.Empty(t, row.Flag)
			}
			verify(s)
			undo, err := s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
			require.NoError(t, err)
			verify(s)
			page, err = s.ListPhotoAssets(ctx, captureRequest(t, "asc"), nil)
			require.NoError(t, err)
			require.Equal(t, ids[0], page.Stacks[0].RepresentativeID)
			redo, err := s.UndoPhotoAuthored(ctx, undo.ReceiptID)
			require.NoError(t, err)
			verify(s)
			_, err = s.UndoPhotoAuthored(ctx, redo.ReceiptID)
			require.NoError(t, err)
			settings, err := s.SetPhotoSettings(ctx, 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(1)})
			require.NoError(t, err)
			page, err = s.ListPhotoAssets(ctx, captureRequest(t, "asc"), nil)
			require.NoError(t, err)
			require.Empty(t, page.Stacks)
			verify(s)
			var backup bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &backup))
			restored := newTestStore(t)
			require.NoError(t, restored.ImportMetadata(ctx, bytes.NewReader(backup.Bytes())))
			verify(restored)
			_, err = restored.SetPhotoSettings(ctx, settings.Revision, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(2)})
			require.NoError(t, err)
			_, err = restored.UndoPhotoAuthored(ctx, undo.ReceiptID)
			require.ErrorIs(t, err, ErrStaleRevision)
			verify(restored)
		})
	}
}

func TestPhotoBurstAuditRollback(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"choice", "undo", "gap", "decision"} {
		t.Run(action, func(t *testing.T) {
			s := newTestStore(t)
			ctx := t.Context()
			count := 2
			if action == "decision" {
				count = 3
			}
			ids := burstPhotos(t, s, count)
			seedInitialAuditAuthority(t, s, s.RootID())
			choose := func() (PhotoAuthoredReceipt, error) {
				return s.DecidePhotoStack(ctx, PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[1], Decision: "representative"})
			}
			var receipt PhotoAuthoredReceipt
			if action == "undo" {
				var err error
				receipt, err = choose()
				require.NoError(t, err)
			}
			var before, after bytes.Buffer
			require.NoError(t, s.ExportMetadata(ctx, &before))
			trigger := `CREATE TRIGGER reject_burst_audit BEFORE INSERT ON audit_records BEGIN SELECT RAISE(ABORT,'forced burst audit failure'); END`
			if action == "decision" {
				trigger = `CREATE TRIGGER reject_stack_audit BEFORE UPDATE ON audit_scopes BEGIN SELECT RAISE(ABORT,'forced stack audit failure'); END`
			}
			_, err := s.db.Exec(trigger)
			require.NoError(t, err)
			switch action {
			case "choice":
				_, err = choose()
			case "undo":
				_, err = s.UndoPhotoAuthored(ctx, receipt.ReceiptID)
			case "decision":
				_, err = s.DecidePhotoStack(ctx, PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(ctx, t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), Decision: "current", CurrentID: ids[1]})
			case "gap":
				_, err = s.SetPhotoSettings(ctx, 1, PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: new(5)})
			}
			if action == "decision" {
				require.ErrorContains(t, err, "forced stack audit failure")
				for _, id := range ids {
					asset, err := s.PhotoAssetByID(ctx, id)
					require.NoError(t, err)
					require.Empty(t, asset.Files[0].Flag)
				}
			} else {
				require.ErrorContains(t, err, "forced burst audit failure")
			}
			require.NoError(t, s.ExportMetadata(ctx, &after))
			require.Equal(t, before.String(), after.String(), "rank, sequence, settings, revisions, receipts and audit authority must roll back together")
		})
	}
}

func TestPhotoBurstAuditDetectsUnexplainedState(t *testing.T) {
	t.Parallel()
	for _, mutation := range []string{
		`UPDATE photo_assets SET representative_rank=2 WHERE representative_rank IS NOT NULL`,
		`UPDATE photo_library_settings SET representative_sequence=2`,
		`UPDATE photo_library_settings SET burst_gap_seconds=5`,
	} {
		t.Run(mutation, func(t *testing.T) {
			s := newTestStore(t)
			ids := burstPhotos(t, s, 2)
			seedInitialAuditAuthority(t, s, s.RootID())
			_, err := s.DecidePhotoStack(t.Context(), PhotoStackDecision{BurstGapSeconds: 2, AssetIDs: ids, Targets: preparedPhotoTargets(t.Context(), t, s, PhotoSetSelection{AssetIDs: ids, Limit: MaxPhotoStackTargets}), CurrentID: ids[1], Decision: "representative"})
			require.NoError(t, err)
			require.NoError(t, s.ValidateMetadata(t.Context()))
			_, err = s.db.Exec(mutation)
			require.NoError(t, err)
			require.Error(t, s.ValidateMetadata(t.Context()))
		})
	}
}

func TestPhotoBurstAuditSettingsReturnToDefault(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	_, err := s.SetPhotoSettings(t.Context(), 1, PhotoSettingsUpdate{Preference: new("image")})
	require.NoError(t, err)
	seedInitialAuditAuthority(t, s, s.RootID())
	_, err = s.SetPhotoSettings(t.Context(), 2, PhotoSettingsUpdate{Preference: new("raw"), BurstGapSeconds: new(5)})
	require.ErrorIs(t, err, ErrAuditMutationUnsupported)
	for i, gap := range []int{5, 2} {
		settings, err := s.SetPhotoSettings(t.Context(), int64(i+2), PhotoSettingsUpdate{KeepPreference: true, BurstGapSeconds: &gap})
		require.NoError(t, err)
		require.Equal(t, gap, settings.BurstGapSeconds)
		require.NotNil(t, settings.Preference)
		require.Equal(t, "image", *settings.Preference)
		require.NoError(t, s.ValidateMetadata(t.Context()))
	}
}
