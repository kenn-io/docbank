package store

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

// ProductionFinalizeHTTPFixture has fully reviewed synthetic members and a
// passing retained gate, but leaves finalization to the public command.
func ProductionFinalizeHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string) {
	t.Helper()
	s, first, second, setID, revision, authority := productionDuplicateGateFixture(t)
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	return s, filepath.Dir(s.path), setID, revision, draft.ETag, namespace.NamespaceID
}

// ProductionRenderDaemonHTTPFixture supplies a reviewed, finalizable vault
// with a real synthetic PDF that the daemon renderer can open after restart.
func ProductionRenderDaemonHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string) {
	t.Helper()
	pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "pt", Size: fpdf.SizeType{Wd: 72, Ht: 72}})
	pdf.SetFont("Helvetica", "", 12)
	pdf.AddPage()
	pdf.Text(20, 36, "x")
	var output bytes.Buffer
	require.NoError(t, pdf.Output(&output))
	pdfBytes := output.Bytes()
	s, first, second, setID, revision, authority := productionDuplicateGateFixtureWithPDF(t,
		pdfBytes, redaction.Box{X0: 0, Y0: 0, X1: 10_000, Y1: 10_000})
	require.Equal(t, first.SourceSHA256, second.SourceSHA256)
	require.True(t, documentproduction.GateResultsPassed(authority.GateResults))
	namespace, err := s.EnsureBatesNamespace(t.Context(), "SYN", "", 6)
	require.NoError(t, err)
	root := filepath.Dir(s.path)
	f := &realRestartFixture{Store: s, root: root}
	f.blobs, err = openRestartBlobs(s, root)
	require.NoError(t, err)
	written, err := f.write(t.Context(), bytes.NewReader(pdfBytes))
	require.NoError(t, err)
	require.Equal(t, first.PDFSHA256, written.Hash)
	require.NoError(t, f.blobs.Close())
	draft, err := s.ProductionDraft(t.Context(), setID, revision)
	require.NoError(t, err)
	return s, root, setID, revision, draft.ETag, namespace.NamespaceID
}

// ProductionRequiredApprovalHTTPFixture has reviewed synthetic inputs and an
// exact authenticated-human grant, but leaves selection and finalization to
// the public API under test.
func ProductionRequiredApprovalHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string, string) {
	t.Helper()
	fixture := newProductionPolicyFinalizationFixture(t, true, false)
	stored := loadProductionInputsForTest(t, fixture.store, fixture.set.ID, fixture.draft.Revision)
	subject, err := productionservice.StoredApprovalSubject(stored)
	require.NoError(t, err)
	now := time.Now().UTC()
	record, err := productionservice.PrepareApprovalRecord(productionservice.RecordApprovalRequest{
		OperationID: "76000000-0000-4000-8000-000000000022",
		ApprovalID:  "76000000-0000-4000-8000-000000000023",
		Subject:     subject, Evidence: "Synthetic approval evidence.",
	}, stored.Policy, productionservice.AuthenticatedApproval{
		Actor: "synthetic-reviewer",
		Authority: documentproduction.ApprovalAuthority{
			Contract:    documentproduction.ApprovalAuthorityContractV1,
			Kind:        documentproduction.ApprovalAuthorityAuthenticatedHuman,
			PrincipalID: "synthetic-principal", AuthenticationMethod: "synthetic-authentication",
			AuthenticatedAt: now.Format(time.RFC3339Nano), EvidenceSHA256: fakeHash("e5"),
		},
	}, now)
	require.NoError(t, err)
	grant, err := fixture.store.PutProductionApproval(t.Context(), record)
	require.NoError(t, err)
	return fixture.store, filepath.Dir(fixture.store.path), fixture.set.ID, fixture.draft.Revision,
		fixture.current.ETag, fixture.namespaceID, grant.ID
}

// ProductionRequiredPrivilegeHTTPFixture has a frozen synthetic public/private
// privilege log but leaves receipt selection and finalization to the agent.
func ProductionRequiredPrivilegeHTTPFixture(t *testing.T) (*Store, string, string, int64, int64, string, string) {
	t.Helper()
	fixture := newProductionPolicyFinalizationFixture(t, false, true)
	s := fixture.store
	stored := loadProductionInputsForTest(t, s, fixture.set.ID, fixture.draft.Revision)
	withheld := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "77000000-0000-4000-8000-000000000053", SetID: fixture.set.ID,
		Revision: fixture.draft.Revision, PolicySHA256: stored.Policy.SHA256,
	}
	for _, member := range []redaction.Member{fixture.first, fixture.second} {
		withheld.Members = append(withheld.Members, documentproduction.WithheldMember{
			ID: member.ID, Ordinal: member.Ordinal,
			SourceVersionID: member.SourceVersionID, SourceSHA256: member.SourceSHA256,
			SourceSize: member.SourceSize, FamilyOrder: member.Ordinal, Family: member.Family,
		})
	}
	preparedWithheld, err := productionservice.PrepareWithheldSelection(
		"77000000-0000-4000-8000-000000000054", withheld, nil)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), preparedWithheld)
	require.NoError(t, err)
	players := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "77000000-0000-4000-8000-000000000055", Revision: 1,
		Players: []documentproduction.Player{{
			ID: "77000000-0000-4000-8000-000000000056", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: fakeHash("e7"),
		}},
	}
	preparedPlayers, err := productionservice.PreparePlayersSnapshot(
		"77000000-0000-4000-8000-000000000057", players)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), preparedPlayers)
	require.NoError(t, err)
	const logID = "77000000-0000-4000-8000-000000000058"
	preparedDraft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "77000000-0000-4000-8000-000000000059", LogID: logID, Revision: 1,
	}, preparedWithheld.Selection, stored.Policy)
	require.NoError(t, err)
	rows := make([]documentproduction.PrivilegeRow, 0, len(withheld.Members))
	for index, member := range withheld.Members {
		rows = append(rows, documentproduction.PrivilegeRow{
			ID:               []string{"77000000-0000-4000-8000-000000000060", "77000000-0000-4000-8000-000000000061"}[index],
			WithheldMemberID: member.ID, FamilyOrder: member.FamilyOrder,
			SourceVersionID: member.SourceVersionID, Basis: "synthetic_basis",
			PublicDescription: "Synthetic public description.",
			PrivateRationale:  "Synthetic private rationale.", EvidenceSHA256: fakeHash("e8"),
			PersonIDs: []string{players.Players[0].ID},
			Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-27"}},
		})
	}
	generation, err := s.CreatePrivilegeLogDraft(t.Context(), PrivilegeLogDraftAuthority{
		Draft: preparedDraft, PlayersSHA256: preparedPlayers.SnapshotSHA256,
		Produced: []redaction.Member{}, Rows: rows,
	})
	require.NoError(t, err)
	validation, err := productionservice.ValidateStoredPrivilegeLog(t.Context(), s,
		productionservice.PrivilegeLogValidationRequest{
			OperationID: "77000000-0000-4000-8000-000000000062", LogID: logID,
			Revision: 1, ExpectedGeneration: generation, ValidatedAt: time.Now().UTC(),
		})
	require.NoError(t, err)
	_, err = productionservice.FreezeStoredPrivilegeLog(t.Context(), s,
		productionservice.PrivilegeLogFreezeRequest{
			OperationID: "77000000-0000-4000-8000-000000000063", LogID: logID,
			Revision: 1, ExpectedGeneration: generation,
			ExpectedInputsSHA256: validation.Validation.InputsSHA256,
			FrozenAt:             time.Now().UTC(),
		})
	require.NoError(t, err)
	return s, filepath.Dir(s.path), fixture.set.ID, fixture.draft.Revision,
		fixture.current.ETag, fixture.namespaceID, logID
}
