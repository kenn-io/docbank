package store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/canonical"
)

func TestAdmitProductionDraftPreviewBindsExactInputAndReplaysAfterRestore(t *testing.T) {
	s, _, member, setID, revision, _ := productionDuplicateGateFixture(t)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	command := ProductionPreviewCommand{SetID: setID, Revision: revision, ETag: draft.ETag,
		OperationID: "76000000-0000-4000-8000-000000000060", MemberID: member.ID, Page: 1}
	admission, err := s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", command)
	require.NoError(t, err)
	require.Equal(t, command, admission.Command)
	require.Equal(t, "synthetic-operator", admission.Actor)
	require.True(t, canonical.IsSHA256Hex(admission.PreviewInputSHA256))
	reader := &productionArtifactVerifyReader{store: s}
	source, err := s.OpenProductionDraftPreviewSource(t.Context(), setID, revision, draft.ETag, member.ID, reader)
	require.NoError(t, err)
	require.Equal(t, source.PreviewInputSHA256, admission.PreviewInputSHA256)
	require.NoError(t, source.PDF.Stream.Close())
	var auditActor, auditKind, requestSHA, receiptSHA string
	require.NoError(t, s.db.QueryRow(`SELECT actor,kind,request_sha256,receipt_sha256 FROM production_audit_evidence WHERE operation_id=?`,
		command.OperationID).Scan(&auditActor, &auditKind, &requestSHA, &receiptSHA))
	require.Equal(t, "synthetic-operator", auditActor)
	require.Equal(t, productionOperationPreviewAdmission, auditKind)
	commandRaw, err := canonical.Marshal(command)
	require.NoError(t, err)
	require.Equal(t, digestProductionBytes(commandRaw), requestSHA)
	require.True(t, canonical.IsSHA256Hex(receiptSHA))
	replay, err := s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", command)
	require.NoError(t, err)
	require.Equal(t, admission, replay)
	changed := command
	changed.Page = 2
	_, err = s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", changed)
	require.Error(t, err)
	_, err = s.AdmitProductionDraftPreview(t.Context(), "another-operator", command)
	require.ErrorIs(t, err, ErrProductionOperationConflict)
	stale := command
	stale.OperationID = "76000000-0000-4000-8000-000000000061"
	stale.ETag++
	_, err = s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", stale)
	require.ErrorIs(t, err, ErrProductionRevisionConflict)
	var admitted int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		stale.OperationID).Scan(&admitted))
	require.Zero(t, admitted)
	var exported bytes.Buffer
	require.NoError(t, s.ExportMetadata(t.Context(), &exported))
	restored := newTestStore(t)
	require.NoError(t, restored.ImportMetadata(t.Context(), bytes.NewReader(exported.Bytes())))
	restoredReplay, err := restored.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", command)
	require.NoError(t, err)
	require.Equal(t, admission, restoredReplay)
}

func TestAdmitProductionDraftPreviewRejectsMissingPageWithoutReceipt(t *testing.T) {
	s, _, member, setID, revision, _ := productionDuplicateGateFixture(t)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	command := ProductionPreviewCommand{SetID: setID, Revision: revision, ETag: draft.ETag,
		OperationID: "76000000-0000-4000-8000-000000000062", MemberID: member.ID, Page: 2}
	_, err = s.AdmitProductionDraftPreview(t.Context(), "synthetic-operator", command)
	require.ErrorIs(t, err, ErrInvalidProduction)
	var admitted int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		command.OperationID).Scan(&admitted))
	require.Zero(t, admitted)
}
