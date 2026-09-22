package store

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestProductionFrozenMetadataRequiresApprovalBinding(t *testing.T) {
	t.Parallel()
	bound, receipt := productionMetadataFrozenDraft(t)
	var exported bytes.Buffer
	require.NoError(t, bound.store.ExportMetadata(t.Context(), &exported))

	for _, tc := range []struct {
		name           string
		removeApproval bool
	}{
		{"required approval binding present", false},
		{"missing approval binding", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metadata := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				switch record.Kind {
				case productionMetadataOperation:
					return false
				case productionMetadataBinding:
					return !tc.removeApproval
				case productionMetadataReceipt:
					if tc.removeApproval {
						var row productionMetadataReceiptRow
						require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
						altered := receipt
						altered.ApprovalEvaluationSHA256 = ""
						var err error
						row.CanonicalJSON, row.SHA256, err = documentproduction.CanonicalPrivilegeLogReceipt(altered)
						require.NoError(t, err)
						record.Key = row.SHA256
						record.CanonicalJSON, err = canonical.Marshal(row)
						require.NoError(t, err)
					}
				}
				return true
			})
			restored := newTestStore(t)
			err := restored.ImportMetadata(t.Context(), bytes.NewReader(metadata))
			if tc.removeApproval {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			loaded, err := restored.PrivilegeLogReceipt(t.Context(), receipt.LogID, receipt.Revision)
			require.NoError(t, err)
			require.Equal(t, receipt, loaded)
		})
	}
}

func TestProductionMetadataRequiresCompletePredecessor(t *testing.T) {
	t.Parallel()
	bound, receipt := productionMetadataFrozenDraft(t)
	fixture := bound.authority
	correction, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "93939393-9393-4393-8393-939393939393", LogID: "94949494-9494-4494-8494-949494949494", Revision: 1,
		PredecessorLogID: receipt.LogID, PredecessorReceiptSHA256: receipt.SHA256,
	}, fixture.withheld.Selection, fixture.policy.Policy)
	require.NoError(t, err)
	_, err = bound.store.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: correction, PlayersSHA256: fixture.players.SnapshotSHA256, Rows: fixture.rows,
	})
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, bound.store.ExportMetadata(t.Context(), &exported))

	for _, tc := range []struct {
		name           string
		logID, receipt *string
		wantError      bool
	}{
		{"complete lineage", &receipt.LogID, &receipt.SHA256, false},
		{"missing predecessor ID", nil, &receipt.SHA256, true},
		{"missing predecessor receipt", &receipt.LogID, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metadata := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				if record.Kind != productionMetadataDraft {
					return true
				}
				var row productionMetadataDraftRow
				require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
				if row.LogID == correction.LogID {
					row.PredecessorLogID, row.PredecessorReceiptSHA256 = tc.logID, tc.receipt
					var err error
					record.CanonicalJSON, err = canonical.Marshal(row)
					require.NoError(t, err)
				}
				return true
			})
			restored := newTestStore(t)
			err := restored.ImportMetadata(t.Context(), bytes.NewReader(metadata))
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			original, err := loadStoredPrivilegeLog(t.Context(), restored.db, receipt.LogID, receipt.Revision)
			require.NoError(t, err)
			require.Empty(t, original.PredecessorID)
			loaded, err := loadStoredPrivilegeLog(t.Context(), restored.db, correction.LogID, correction.Revision)
			require.NoError(t, err)
			require.Equal(t, receipt.LogID, loaded.PredecessorID)
		})
	}
}

func productionMetadataFrozenDraft(t *testing.T) (productionMetadataFixture, documentproduction.PrivilegeLogReceipt) {
	t.Helper()
	bound := productionMetadataBoundDraft(t)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), bound.store, bound.validation)
	require.NoError(t, err)
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), bound.store, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: bound.authority.draft.LogID,
		Revision: 1, ExpectedGeneration: 1, ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: bound.binding.Evaluation.SHA256, FrozenAt: bound.validation.ValidatedAt.Add(2 * time.Second),
	})
	require.NoError(t, err)
	return bound, receipt
}
