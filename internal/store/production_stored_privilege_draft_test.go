package store

import (
	"testing"

	"github.com/stretchr/testify/require"
	documentproduction "go.kenn.io/docbank/document/production"
	"go.kenn.io/docbank/document/redaction"
	productionservice "go.kenn.io/docbank/internal/production"
)

func TestStoredPrivilegeProducedMembersPartitionsRetainedOrder(t *testing.T) {
	_, first := seedProductionGateAuthority(t)
	first.ID, first.Ordinal = "76767676-7676-4767-8767-767676767611", 1
	second := first
	second.ID, second.Ordinal = "76767676-7676-4767-8767-767676767612", 2
	second.SourceVersionID = "76767676-7676-4767-8767-767676767613"
	second.Family.RootVersionID = second.SourceVersionID
	selection := documentproduction.WithheldSelection{
		Contract: documentproduction.WithheldSelectionContractV1,
		ID:       "76767676-7676-4767-8767-767676767614",
		SetID:    "76767676-7676-4767-8767-767676767615", Revision: 1,
		PolicySHA256: fakeHash("ae"),
		Members: []documentproduction.WithheldMember{{
			ID: first.ID, Ordinal: first.Ordinal, SourceVersionID: first.SourceVersionID,
			SourceSHA256: first.SourceSHA256, SourceSize: first.SourceSize,
			FamilyOrder: 1, Family: first.Family,
		}},
	}
	produced, err := storedPrivilegeProducedMembers([]productionservice.StoredPreparedMember{
		{Member: first}, {Member: second},
	}, selection)
	require.NoError(t, err)
	require.Equal(t, []redaction.Member{second}, produced)

	selection.Members[0].Ordinal = second.Ordinal
	_, err = storedPrivilegeProducedMembers([]productionservice.StoredPreparedMember{
		{Member: first}, {Member: second},
	}, selection)
	requireProductionProblem(t, err, documentproduction.ProblemInvalidContract)
}

func TestCreateStoredPrivilegeLogDraftUsesExactSealedMembership(t *testing.T) {
	for _, test := range []struct {
		name               string
		changeWithheldHash bool
	}{
		{name: "exact withheld member"},
		{name: "changed withheld source", changeWithheldHash: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, member, setID, revision := productionEvidenceRevisionWithPolicy(t, "metadata.title", "standalone", false, true)
			stored := loadProductionInputsForTest(t, s, setID, revision)
			withheldMember := documentproduction.WithheldMember{
				ID: member.ID, Ordinal: member.Ordinal, SourceVersionID: member.SourceVersionID,
				SourceSHA256: member.SourceSHA256, SourceSize: member.SourceSize,
				FamilyOrder: 1, Family: member.Family,
			}
			if test.changeWithheldHash {
				withheldMember.SourceSHA256 = fakeHash("ab")
			}
			withheld, err := productionservice.PrepareWithheldSelection(
				"76767676-7676-4767-8767-767676767601", documentproduction.WithheldSelection{
					Contract: documentproduction.WithheldSelectionContractV1,
					ID:       "76767676-7676-4767-8767-767676767602", SetID: setID, Revision: revision,
					PolicySHA256: stored.Policy.SHA256, Members: []documentproduction.WithheldMember{withheldMember},
				}, nil)
			require.NoError(t, err)
			_, err = s.PutProductionWithheldSelection(t.Context(), withheld)
			require.NoError(t, err)
			players, err := productionservice.PreparePlayersSnapshot(
				"76767676-7676-4767-8767-767676767603", documentproduction.PlayersSnapshot{
					Contract: documentproduction.PlayersSnapshotContractV1,
					ID:       "76767676-7676-4767-8767-767676767604", Revision: 1,
					Players: []documentproduction.Player{{
						ID: "76767676-7676-4767-8767-767676767605", DisplayName: "Synthetic Person",
						EvidenceSHA256: fakeHash("ac"),
					}},
				})
			require.NoError(t, err)
			_, err = s.PutProductionPlayersSnapshot(t.Context(), players)
			require.NoError(t, err)
			request := StoredPrivilegeLogDraftRequest{
				SetID: setID, SetRevision: revision,
				Draft: productionservice.PrivilegeLogDraftRequest{
					OperationID: "76767676-7676-4767-8767-767676767606",
					LogID:       "76767676-7676-4767-8767-767676767607", Revision: 1,
				},
				PlayersSHA256: players.SnapshotSHA256,
				Rows: []documentproduction.PrivilegeRow{{
					ID: "76767676-7676-4767-8767-767676767608", WithheldMemberID: member.ID,
					FamilyOrder: 1, SourceVersionID: member.SourceVersionID, Basis: "synthetic_basis",
					PublicDescription: "Synthetic public description.",
					PrivateRationale:  "Synthetic private rationale.", EvidenceSHA256: fakeHash("ad"),
					PersonIDs: []string{players.Snapshot.Players[0].ID},
					Fields:    []documentproduction.PrivilegeField{{Name: "date", Value: "2026-09-23"}},
				}},
			}
			generation, err := s.CreateStoredPrivilegeLogDraft(t.Context(), request)
			if test.changeWithheldHash {
				requireProductionProblem(t, err, documentproduction.ProblemInvalidContract)
				require.Zero(t, generation)
				_, err = loadStoredPrivilegeLog(t.Context(), s.db, request.Draft.LogID, request.Draft.Revision)
				require.ErrorIs(t, err, ErrNotFound)
				return
			}
			require.NoError(t, err)
			require.Equal(t, int64(1), generation)
			replay, err := s.CreateStoredPrivilegeLogDraft(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, generation, replay)
			storedDraft, err := loadStoredPrivilegeLog(t.Context(), s.db, request.Draft.LogID, request.Draft.Revision)
			require.NoError(t, err)
			require.Empty(t, storedDraft.Produced)
			changed := request
			changed.Rows = append([]documentproduction.PrivilegeRow(nil), request.Rows...)
			changed.Rows[0].PrivateRationale = "Different synthetic rationale."
			_, err = s.CreateStoredPrivilegeLogDraft(t.Context(), changed)
			requireProductionProblem(t, err, documentproduction.ProblemChangedPayload)
		})
	}
}
