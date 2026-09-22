package store

import (
	"bytes"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/canonical"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestProductionMetadataRejectsMismatchedIdentities(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s, fixture := bound.store, bound.authority
	stored, err := loadStoredPrivilegeLog(t.Context(), s.db, fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: fixture.draft.LogID,
		Revision: 1, ExpectedGeneration: 1, ExpectedInputsSHA256: stored.Validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: bound.binding.Evaluation.SHA256, FrozenAt: bound.validation.ValidatedAt.Add(2 * time.Second),
	})
	require.NoError(t, err)
	attachment, err := productionservice.PreparePrivilegeLogAttachment(
		"19191919-1919-4919-8919-191919191919", "20202020-2020-4020-8020-202020202020",
		receipt, fixture.withheld.Selection, fixture.rows, productionPersistenceSHA("1"),
		[]documentproduction.PrivilegeOutputReference{{WithheldMemberID: fixture.withheld.Selection.Members[0].ID,
			AssignedNumber: "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2")}}, bound.validation.ValidatedAt.Add(3*time.Second))
	require.NoError(t, err)
	_, err = s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))

	const otherID = "abababab-abab-4bab-8bab-abababababab"
	for _, tc := range []struct {
		kind, field string
		value       any
	}{
		{productionMetadataPolicy, "policy_id", otherID},
		{productionMetadataPolicy, "version", 2},
		{productionMetadataPlayers, "snapshot_id", otherID},
		{productionMetadataPlayers, "revision", 2},
		{productionMetadataWithheld, "selection_id", otherID},
		{productionMetadataWithheld, "set_id", fixture.draft.LogID},
		{productionMetadataWithheld, "revision", 2},
		{productionMetadataValidation, "operation_id", otherID},
		{productionMetadataValidation, "request_sha256", productionPersistenceSHA("f")},
		{productionMetadataValidation, "draft_generation", 2},
		{productionMetadataValidation, "inputs_sha256", productionPersistenceSHA("f")},
		{productionMetadataValidation, "rows_sha256", productionPersistenceSHA("f")},
		{productionMetadataAttachment, "attachment_id", otherID},
	} {
		t.Run(tc.kind+"/"+tc.field, func(t *testing.T) {
			t.Parallel()
			changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				if record.Kind == tc.kind {
					var row map[string]any
					require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
					row[tc.field] = tc.value
					var err error
					record.CanonicalJSON, err = canonical.Marshal(row)
					require.NoError(t, err)
				}
				return true
			})
			restored := newTestStore(t)
			require.Error(t, restored.ImportMetadata(t.Context(), bytes.NewReader(changed)))
		})
	}
	for _, field := range []string{"log_id", "revision"} {
		t.Run("receipt/"+field, func(t *testing.T) {
			t.Parallel()
			changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				if record.Kind == productionMetadataAttachment || record.Kind == productionMetadataOperation {
					return false
				}
				if record.Kind == productionMetadataReceipt {
					var row productionMetadataReceiptRow
					require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
					altered := receipt
					if field == "log_id" {
						altered.LogID = otherID
					} else {
						altered.Revision = 2
					}
					var err error
					row.CanonicalJSON, row.SHA256, err = documentproduction.CanonicalPrivilegeLogReceipt(altered)
					require.NoError(t, err)
					record.Key = row.SHA256
					record.CanonicalJSON, err = canonical.Marshal(row)
					require.NoError(t, err)
				}
				return true
			})
			restored := newTestStore(t)
			require.ErrorContains(t, restored.ImportMetadata(t.Context(), bytes.NewReader(changed)), "receipt")
		})
	}
}

func TestProductionMetadataExportRejectsMismatchedIdentities(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, statement string }{
		{"validation", `UPDATE production_privilege_log_validations SET operation_id='abababab-abab-4bab-8bab-abababababab'`},
		{"row", `UPDATE production_privilege_log_rows SET row_id='abababab-abab-4bab-8bab-abababababab'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := productionMetadataBoundDraft(t).store
			// Inject corrupt storage beneath the normal immutable write boundary.
			if tc.name == "validation" {
				dropProductionTrigger(t, s, "production_privilege_log_validations_immutable_update")
			}
			_, err := s.db.Exec(tc.statement)
			require.NoError(t, err)
			var exported bytes.Buffer
			require.Error(t, s.ExportMetadata(t.Context(), &exported))
		})
	}
}

func TestProductionMetadataRejectsOrphanDraftOperations(t *testing.T) {
	t.Parallel()
	s := productionMetadataBoundDraft(t).store
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	for _, operation := range []string{productionOperationDraft, productionOperationValidation, productionOperationApprovalBind} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
				switch record.Kind {
				case productionMetadataDraft, productionMetadataRow, productionMetadataValidation, productionMetadataBinding:
					return false
				case productionMetadataOperation:
					var row productionMetadataOperationRow
					require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
					return row.Kind == operation
				}
				return true
			})
			restored := newTestStore(t)
			require.Error(t, restored.ImportMetadata(t.Context(), bytes.NewReader(changed)))
		})
	}
}

func TestProductionMetadataRetainsHistoricalDraftOperationRetries(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s, fixture := bound.store, bound.authority
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, bound.validation)
	require.NoError(t, err)
	update := PrivilegeLogRowUpdate{OperationID: "18181818-1818-4818-8818-181818181818",
		LogID: fixture.draft.LogID, Revision: 1, ExpectedGeneration: 1, Rows: fixture.rows}
	update.Rows[0].PublicDescription = "Updated synthetic description."
	_, err = s.ReplacePrivilegeLogRows(t.Context(), update)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	replayed, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), restored, bound.validation)
	require.NoError(t, err)
	require.Equal(t, validation, replayed)
	require.NoError(t, restored.BindPrivilegeLogApproval(t.Context(), bound.binding))
	generation, err := restored.ReplacePrivilegeLogRows(t.Context(), update)
	require.NoError(t, err)
	require.Equal(t, int64(2), generation)
}

func TestProductionMetadataPreservesDecodeError(t *testing.T) {
	t.Parallel()
	s, fixture := newProductionPersistenceFixture(t)
	_, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
		if record.Kind == productionMetadataPolicy {
			record.CanonicalJSON = []byte(`{"policy_id":1}`)
		}
		return true
	})
	restored := newTestStore(t)
	err = restored.ImportMetadata(t.Context(), bytes.NewReader(changed))
	var decodeErr *json.SemanticError
	require.ErrorAs(t, err, &decodeErr)
	require.Contains(t, string(decodeErr.JSONPointer), "policy_id")
}

func TestProductionApprovalRejectsContradictoryMemberSnapshots(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s, fixture := bound.store, bound.authority
	stored, err := loadStoredPrivilegeLog(t.Context(), s.db, fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	approval := fixture.approval(t, stored.Validation.Validation.InputsSHA256)
	subject := approval.record.Subject
	subject.Members[0].SourceVersionID = "abababab-abab-4bab-8bab-abababababab"
	subject.Members[0].SourceSHA256 = productionPersistenceSHA("f")
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "29292929-2929-4929-8929-292929292929", ApprovalID: "30303030-3030-4030-8030-303030303030",
		Subject: subject, Evidence: "Synthetic contradictory approval.",
	}, fixture.policy.Policy, productionservice.AuthenticatedApproval{
		Actor: approval.record.Grant.Actor, Authority: approval.record.Authority,
	}, bound.validation.ValidatedAt)
	require.NoError(t, err)
	grant, err := s.PutProductionApproval(t.Context(), record)
	require.NoError(t, err)
	evaluation, err := productionservice.EvaluateRequiredApproval(fixture.policy.Policy, subject, &grant, nil, bound.validation.ValidatedAt.Add(2*time.Second))
	require.NoError(t, err)
	binding := bound.binding
	binding.OperationID = "31313131-3131-4131-8131-313131313131"
	binding.ApprovalID, binding.Evaluation = grant.ID, evaluation
	t.Run("bind", func(t *testing.T) {
		requireProductionProblem(t, s.BindPrivilegeLogApproval(t.Context(), binding), documentproduction.ProblemApprovalStale)
	})
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	t.Run("restore", func(t *testing.T) {
		changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
			if record.Kind == productionMetadataOperation {
				return false
			}
			if record.Kind == productionMetadataBinding {
				var row productionMetadataBindingRow
				require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
				row.ApprovalID = grant.ID
				var err error
				row.CanonicalJSON, row.EvaluationSHA256, err = documentproduction.CanonicalApprovalEvaluation(evaluation)
				require.NoError(t, err)
				record.CanonicalJSON, err = canonical.Marshal(row)
				require.NoError(t, err)
			}
			return true
		})
		restored := newTestStore(t)
		err := restored.ImportMetadata(t.Context(), bytes.NewReader(changed))
		requireProductionProblem(t, err, documentproduction.ProblemApprovalStale)
	})
}

func TestProductionAttachmentRejectsDetachedMembers(t *testing.T) {
	t.Parallel()
	bound := productionMetadataBoundDraft(t)
	s, fixture := bound.store, bound.authority
	stored, err := loadStoredPrivilegeLog(t.Context(), s.db, fixture.draft.LogID, fixture.draft.Revision)
	require.NoError(t, err)
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s, productionservice.PrivilegeLogFreezeRequest{
		OperationID: "88888888-8888-4888-8888-888888888888", LogID: fixture.draft.LogID,
		Revision: 1, ExpectedGeneration: 1, ExpectedInputsSHA256: stored.Validation.Validation.InputsSHA256,
		ExpectedApprovalEvaluationSHA256: bound.binding.Evaluation.SHA256, FrozenAt: bound.validation.ValidatedAt.Add(3 * time.Second),
	})
	require.NoError(t, err)
	attachment, err := productionservice.PreparePrivilegeLogAttachment(
		"19191919-1919-4919-8919-191919191919", "20202020-2020-4020-8020-202020202020",
		receipt, fixture.withheld.Selection, fixture.rows, productionPersistenceSHA("1"),
		[]documentproduction.PrivilegeOutputReference{{WithheldMemberID: fixture.withheld.Selection.Members[0].ID,
			AssignedNumber: "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2")}}, bound.validation.ValidatedAt.Add(4*time.Second))
	require.NoError(t, err)
	detached := attachment
	detached.Receipt.References = []documentproduction.PrivilegeOutputReference{{
		WithheldMemberID: "abababab-abab-4bab-8bab-abababababab", AssignedNumber: "SYNTHETIC0001", ArtifactSHA256: productionPersistenceSHA("2"),
	}}
	detached.Canonical, detached.RequestSHA256, err = documentproduction.CanonicalPrivilegeLogAttachment(detached.Receipt)
	require.NoError(t, err)
	detached.Receipt.SHA256 = detached.RequestSHA256

	t.Run("write", func(t *testing.T) {
		_, err := s.PutPrivilegeLogAttachment(t.Context(), detached)
		requireProductionProblem(t, err, documentproduction.ProblemInvalidContract)
	})
	_, err = s.PutPrivilegeLogAttachment(t.Context(), attachment)
	require.NoError(t, err)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	t.Run("restore", func(t *testing.T) {
		changed := rewriteProductionMetadata(t, exported.Bytes(), func(record *metadataProductionAuthority) bool {
			if record.Kind == productionMetadataOperation {
				return false
			}
			if record.Kind == productionMetadataAttachment {
				var row productionMetadataAttachmentRow
				require.NoError(t, json.Unmarshal(record.CanonicalJSON, &row))
				row.CanonicalJSON, row.SHA256 = detached.Canonical, detached.Receipt.SHA256
				record.Key = row.SHA256
				var err error
				record.CanonicalJSON, err = canonical.Marshal(row)
				require.NoError(t, err)
			}
			return true
		})
		restored := newTestStore(t)
		requireProductionProblem(t, restored.ImportMetadata(t.Context(), bytes.NewReader(changed)), documentproduction.ProblemInvalidContract)
	})
}

type productionMetadataFixture struct {
	store      *Store
	authority  productionPersistenceFixture
	validation productionservice.PrivilegeLogValidationRequest
	binding    PrivilegeLogApprovalBinding
}

func productionMetadataBoundDraft(t *testing.T) productionMetadataFixture {
	t.Helper()
	s, fixture := newProductionPersistenceFixture(t)
	policy, err := s.PutProductionPolicy(t.Context(), fixture.policy)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), fixture.players)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), fixture.withheld)
	require.NoError(t, err)
	_, err = s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{Draft: fixture.draft,
		PlayersSHA256: fixture.players.SnapshotSHA256, Produced: []redaction.Member{}, Rows: fixture.rows})
	require.NoError(t, err)
	request := productionservice.PrivilegeLogValidationRequest{
		OperationID: "77777777-7777-4777-8777-777777777777", LogID: fixture.draft.LogID,
		Revision: 1, ExpectedGeneration: 1, ValidatedAt: productionPersistenceTime(t, "2026-09-22T14:00:00Z"),
	}
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s, request)
	require.NoError(t, err)
	approval := fixture.approval(t, validation.Validation.InputsSHA256)
	grant, err := s.PutProductionApproval(t.Context(), approval.record)
	require.NoError(t, err)
	evaluation, err := productionservice.EvaluateRequiredApproval(policy, approval.record.Subject, &grant, nil, request.ValidatedAt.Add(2*time.Second))
	require.NoError(t, err)
	binding := PrivilegeLogApprovalBinding{OperationID: "17171717-1717-4717-8717-171717171717",
		LogID: fixture.draft.LogID, Revision: 1, ExpectedGeneration: 1, ApprovalID: grant.ID, Evaluation: evaluation}
	require.NoError(t, s.BindPrivilegeLogApproval(t.Context(), binding))
	return productionMetadataFixture{store: s, authority: fixture, validation: request, binding: binding}
}

func rewriteProductionMetadata(t *testing.T, input []byte, edit func(*metadataProductionAuthority) bool) []byte {
	t.Helper()
	var output bytes.Buffer
	for line := range bytes.SplitSeq(bytes.TrimSpace(input), []byte("\n")) {
		var record metadataProductionAuthority
		require.NoError(t, json.Unmarshal(line, &record))
		if record.Type == metadataProductionAuthorityType {
			if !edit(&record) {
				continue
			}
			record.Checksum = digestProductionBytes(record.CanonicalJSON)
			var err error
			line, err = json.Marshal(record)
			require.NoError(t, err)
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	return output.Bytes()
}
