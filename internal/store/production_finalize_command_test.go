package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/internal/canonical"
)

func TestFinalizeProductionDraftReplaysExactCommandAfterEvidenceDrift(t *testing.T) {
	s, first, _, setID, revision, _ := productionDuplicateGateFixture(t)
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	command := ProductionFinalizeCommand{
		SetID: setID, Revision: revision, ETag: draft.ETag,
		OperationID: "75000000-0000-4000-8000-000000000060",
		SnapshotID:  "75000000-0000-4000-8000-000000000061",
		NamespaceID: namespace.NamespaceID,
	}
	result, err := s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.NoError(t, err)
	var auditActor, auditKind, auditRequestSHA, auditReceiptSHA string
	require.NoError(t, s.db.QueryRow(`SELECT actor,kind,request_sha256,receipt_sha256 FROM production_audit_evidence WHERE operation_id=?`,
		command.OperationID).Scan(&auditActor, &auditKind, &auditRequestSHA, &auditReceiptSHA))
	require.Equal(t, "test-agent", auditActor)
	require.Equal(t, productionOperationFinalizeCommand, auditKind)
	commandRaw, err := canonical.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, digestProductionBytes(commandRaw), auditRequestSHA)
	require.NotEmpty(t, auditReceiptSHA)
	require.Equal(t, "finalized", result.Draft.State)
	require.Equal(t, setID, result.Draft.SetID)
	require.Equal(t, namespace.NamespaceID, result.NamespaceID)
	require.Equal(t, command.SnapshotID, result.SnapshotID)
	require.NotEmpty(t, result.PreparedSHA256)
	require.NotEmpty(t, result.ReceiptSHA256)
	loaded, err := s.LoadFinalizedProduction(t.Context(), setID, revision)
	require.NoError(t, err)
	require.Equal(t, result.PreparedSHA256, loaded.Authority.Prepared.SHA256)
	require.Equal(t, result.ReceiptSHA256, loaded.Authority.Receipt.SHA256)
	var allocations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, allocations, "finalizing must not reserve numbers")
	var metadata bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &metadata), "admitted command must survive backup export")
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(metadata.Bytes())))
	restoredFinalized, err := restored.LoadFinalizedProduction(t.Context(), setID, revision)
	require.NoError(t, err)
	require.Equal(t, result.ReceiptSHA256, restoredFinalized.Authority.Receipt.SHA256)

	productionEvidenceMetadata(t, s, first.SourceSHA256, fakeHash("97"), "Changed synthetic title")
	replay, err := s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.NoError(t, err)
	require.Equal(t, result, replay)
	changed := command
	changed.SnapshotID = "75000000-0000-4000-8000-000000000062"
	_, err = s.FinalizeProductionDraft(t.Context(), "test-agent", changed)
	var problem *documentproduction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, documentproduction.ProblemChangedPayload, problem.Code)
	stale := command
	stale.OperationID = "75000000-0000-4000-8000-000000000063"
	stale.ETag++
	_, err = s.FinalizeProductionDraft(t.Context(), "test-agent", stale)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
}

func TestFinalizeProductionDraftRejectsMissingNamespaceBeforeAdmission(t *testing.T) {
	s, first, second, setID, revision, authority := productionDuplicateGateFixture(t)
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	command := ProductionFinalizeCommand{
		SetID: setID, Revision: revision, ETag: draft.ETag,
		OperationID: "75000000-0000-4000-8000-000000000064",
		SnapshotID:  "75000000-0000-4000-8000-000000000065",
		NamespaceID: "75000000-0000-4000-8000-000000000066",
	}
	_, err = s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.ErrorIs(t, err, ErrBatesReservationConflict)
	var admitted int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		command.OperationID).Scan(&admitted))
	require.Zero(t, admitted)
}
