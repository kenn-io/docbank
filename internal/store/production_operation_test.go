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

func TestProductionOperationImportRejectsInconsistentResponseDigests(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s, fixture := bound.store, bound.authority
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, bound.validation)
	require.NoError(t, err)
	freeze := productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: fixture.draft.LogID,
		Revision: 1, ExpectedGeneration: 1, ExpectedInputsSHA256: validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: bound.binding.Evaluation.SHA256, FrozenAt: bound.validation.ValidatedAt.Add(2 * time.Second),
	}
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, freeze)
	require.NoError(t, err)
	attachment, err := productionservice.PreparePrivilegeLogAttachment(
		"19191919-1919-4919-8919-191919191919", "20202020-2020-4020-8020-202020202020",
		receipt, fixture.withheld.Selection, fixture.rows, productionPersistenceSHA("1"),
		[]documentproduction.PrivilegeOutputReference{{WithheldMemberID: fixture.withheld.Selection.Members[0].ID,
			AssignedNumber: "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2")}}, bound.validation.ValidatedAt.Add(3*time.Second))
	require.NoError(t, err)
	_, err = s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	approval := fixture.approval(t, validation.Validation.InputsSHA256).record
	event, err := productionservice.PrepareApprovalEvent(approval.Grant, productionservice.ApprovalEventRequest{
		OperationID: "99999999-9999-4999-8999-999999999999", EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Kind: documentproduction.ApprovalEventRevoke, EffectiveAt: "2026-09-22T14:00:04Z", Reason: "Synthetic post-freeze revocation.",
	})
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), event)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	t.Run("exact retries after later revocation", func(t *testing.T) {
		t.Parallel()
		restored := newTestStore(t)
		require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
		replayedApproval, err := restored.PutProductionApproval(t.Context(), approval)
		require.NoError(t, err)
		require.Equal(t, approval.Grant, replayedApproval)
		replayedFreeze, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), restored, freeze)
		require.NoError(t, err)
		require.Equal(t, receipt, replayedFreeze)
	})

	for _, tc := range []struct {
		kind, field string
		value       any
	}{
		{productionOperationPolicy, "name", "Altered synthetic policy"},
		{productionOperationApproval, "actor", "altered-synthetic-reviewer"},
		{productionOperationPlayers, "revision", 2},
		{productionOperationWithheld, "revision", 2},
		{productionOperationFreeze, "row_count", 2},
		{productionOperationAttachment, "created_at", "2026-09-22T14:00:04Z"},
		{productionOperationValidation, "inputs_sha256", productionPersistenceSHA("f")},
		{productionOperationValidation, "rows_sha256", productionPersistenceSHA("f")},
		{productionOperationApproval, "request_sha256", productionPersistenceSHA("f")},
		{productionOperationFreeze, "request_sha256", productionPersistenceSHA("f")},
		{productionOperationPolicy, "request_sha256", productionPersistenceSHA("f")},
		{productionOperationPlayers, "request_sha256", productionPersistenceSHA("f")},
		{productionOperationWithheld, "request_sha256", productionPersistenceSHA("f")},
		{productionOperationAttachment, "request_sha256", productionPersistenceSHA("f")},
	} {
		t.Run(tc.kind+"/"+tc.field, func(t *testing.T) {
			t.Parallel()
			changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				if record.Kind != productionMetadataOperation {
					return true
				}
				var row productionMetadataOperationRow
				require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
				if row.Kind != tc.kind {
					return true
				}
				var err error
				if tc.field == "request_sha256" {
					var ok bool
					row.RequestSHA256, ok = tc.value.(string)
					require.True(t, ok)
				} else {
					var response map[string]any
					require.NoError(t, json.Unmarshal(row.ResponseJSON, &response))
					fields := response
					if tc.kind == productionOperationValidation {
						var ok bool
						fields, ok = response["Validation"].(map[string]any)
						require.True(t, ok)
					}
					fields[tc.field] = tc.value
					row.ResponseJSON, err = canonical.Marshal(response)
					require.NoError(t, err)
					row.ResponseSHA256 = digestProductionBytes(row.ResponseJSON)
				}
				record.CanonicalJSON, err = canonical.Marshal(row)
				require.NoError(t, err)
				return true
			})
			restored := newTestStore(t)
			require.Error(t, restored.ImportMetadata(t.Context(), bytes.NewReader(changed)))
		})
	}
}

func TestProductionOperationImportRequiresApprovalEventOrigin(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s := bound.store
	grant, _, _, err := loadProductionApproval(t.Context(), s.db, bound.binding.ApprovalID)
	require.NoError(t, err)
	request := productionservice.ApprovalEventRequest{
		OperationID: "99999999-9999-4999-8999-999999999999", EventID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Kind: documentproduction.ApprovalEventRevoke, EffectiveAt: "2026-09-22T14:30:00Z", Reason: "Original synthetic reason",
	}
	first, err := productionservice.PrepareApprovalEvent(grant, request)
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), first)
	require.NoError(t, err)
	request.Reason = "Changed synthetic reason"
	changed, err := productionservice.PrepareApprovalEvent(grant, request)
	require.NoError(t, err)
	request.OperationID, request.EventID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	second, err := productionservice.PrepareApprovalEvent(grant, request)
	require.NoError(t, err)
	_, err = s.PutProductionApprovalEvent(t.Context(), second)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	for _, tc := range []struct {
		name          string
		response      documentproduction.ApprovalEvent
		requestSHA256 string
		wantError     bool
	}{
		{"exact retry", first.Event, first.RequestSHA256, false},
		{"same ID different body", changed.Event, changed.RequestSHA256, true},
		{"other existing event", second.Event, first.RequestSHA256, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metadata := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				if record.Kind != productionMetadataOperation {
					return true
				}
				var row productionMetadataOperationRow
				require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
				if row.OperationID != first.OperationID {
					return true
				}
				var err error
				row.RequestSHA256 = tc.requestSHA256
				row.ResponseJSON, err = canonical.Marshal(tc.response)
				require.NoError(t, err)
				row.ResponseSHA256 = digestProductionBytes(row.ResponseJSON)
				record.CanonicalJSON, err = canonical.Marshal(row)
				require.NoError(t, err)
				return true
			})
			restored := newTestStore(t)
			err := restored.ImportMetadata(t.Context(), bytes.NewReader(metadata))
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			replayed, err := restored.PutProductionApprovalEvent(t.Context(), first)
			require.NoError(t, err)
			require.Equal(t, first.Event, replayed)
		})
	}
}

func TestProductionOperationReplayRejectsInconsistentResponseDigest(t *testing.T) {
	t.Parallel()
	s, fixture := newProductionPersistenceFixture(t)
	policy, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	policy.Name = "Altered synthetic policy"
	response, err := canonical.Marshal(policy)
	require.NoError(t, err)
	// Inject corrupt storage beneath the normal immutable write boundary.
	dropProductionTrigger(t, s, "production_operation_receipts_immutable_update")
	_, err = s.db.Exec(`UPDATE production_operation_receipts SET response_json=?,response_sha256=? WHERE operation_id=?`,
		response, digestProductionBytes(response), fixture.policy.OperationID)
	require.NoError(t, err)
	_, err = s.PutProductionPolicy(t.Context(), fixture.policy)
	requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
}
