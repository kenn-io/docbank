package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestFinalizeProductionDraftRequiresFrozenPrivilegeLogBeforeAdmission(t *testing.T) {
	fixture := newProductionPolicyFinalizationFixture(t, false, true)
	s, set, draft, current := fixture.store, fixture.set, fixture.draft, fixture.current
	command := ProductionFinalizeCommand{
		SetID: set.ID, Revision: draft.Revision, ETag: current.ETag,
		OperationID: "77000000-0000-4000-8000-000000000001",
		SnapshotID:  "77000000-0000-4000-8000-000000000002",
		NamespaceID: fixture.namespaceID,
	}
	_, err := s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.ErrorIs(t, err, ErrInvalidProduction)
	var admitted int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM production_operation_receipts WHERE operation_id=?`,
		command.OperationID).Scan(&admitted))
	require.Zero(t, admitted, "missing frozen privilege receipt cannot admit finalization")

	stored := loadProductionInputsForTest(t, s, set.ID, draft.Revision)
	withheld := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "77000000-0000-4000-8000-000000000003", SetID: set.ID,
		Revision: draft.Revision, PolicySHA256: stored.Policy.SHA256,
	}
	for _, member := range []redaction.Member{fixture.first, fixture.second} {
		withheld.Members = append(withheld.Members, documentproduction.WithheldMember{
			ID: member.ID, Ordinal: member.Ordinal,
			SourceVersionID: member.SourceVersionID, SourceSHA256: member.SourceSHA256,
			SourceSize: member.SourceSize, FamilyOrder: member.Ordinal, Family: member.Family,
		})
	}
	preparedWithheld, err := productionservice.PrepareWithheldSelection(
		"77000000-0000-4000-8000-000000000004", withheld, nil)
	require.NoError(t, err)
	_, err = s.PutProductionWithheldSelection(t.Context(), preparedWithheld)
	require.NoError(t, err)
	players := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "77000000-0000-4000-8000-000000000005", Revision: 1,
		Players: []documentproduction.Player{{
			ID: "77000000-0000-4000-8000-000000000006", DisplayName: "Synthetic Person",
			Aliases: []string{"synthetic@example.test"}, EvidenceSHA256: fakeHash("e7"),
		}},
	}
	preparedPlayers, err := productionservice.PreparePlayersSnapshot(
		"77000000-0000-4000-8000-000000000007", players)
	require.NoError(t, err)
	_, err = s.PutProductionPlayersSnapshot(t.Context(), preparedPlayers)
	require.NoError(t, err)
	const logID = "77000000-0000-4000-8000-000000000008"
	preparedDraft, err := productionservice.PreparePrivilegeLogDraft(productionservice.PrivilegeLogDraftRequest{
		OperationID: "77000000-0000-4000-8000-000000000009", LogID: logID, Revision: 1,
	}, preparedWithheld.Selection, stored.Policy)
	require.NoError(t, err)
	rows := make([]documentproduction.PrivilegeRow, 0, len(withheld.Members))
	for index, member := range withheld.Members {
		rows = append(rows, documentproduction.PrivilegeRow{
			ID:               []string{"77000000-0000-4000-8000-000000000010", "77000000-0000-4000-8000-000000000011"}[index],
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
			OperationID: "77000000-0000-4000-8000-000000000012", LogID: logID,
			Revision: 1, ExpectedGeneration: generation, ValidatedAt: time.Now().UTC(),
		})
	require.NoError(t, err)
	receipt, err := productionservice.FreezeStoredPrivilegeLog(t.Context(), s,
		productionservice.PrivilegeLogFreezeRequest{
			OperationID: "77000000-0000-4000-8000-000000000013", LogID: logID,
			Revision: 1, ExpectedGeneration: generation,
			ExpectedInputsSHA256: validation.Validation.InputsSHA256,
			FrozenAt:             time.Now().UTC(),
		})
	require.NoError(t, err)
	require.NoError(t, s.SelectProductionRevisionGateAuthority(t.Context(), set.ID, draft.Revision,
		ProductionRevisionGateSelection{PrivilegeLogID: logID, PrivilegeLogRevision: 1}))
	finalized, err := s.FinalizeProductionDraft(t.Context(), "test-agent", command)
	require.NoError(t, err)
	require.Equal(t, "finalized", finalized.Draft.State)
	loaded, err := s.LoadFinalizedProduction(t.Context(), set.ID, draft.Revision)
	require.NoError(t, err)
	require.NotNil(t, loaded.Authority.Prepared.PrivilegeLogReceipt)
	require.Equal(t, receipt.SHA256, loaded.Authority.Prepared.PrivilegeLogReceipt.SHA256)
	require.Equal(t, finalized.ReceiptSHA256, loaded.Authority.Receipt.SHA256)
	var allocations int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM bates_allocations`).Scan(&allocations))
	require.Zero(t, allocations, "finalization does not reserve numbers")
}
