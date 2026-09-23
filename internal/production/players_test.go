package production

import (
	"testing"

	"github.com/stretchr/testify/require"

	documentproduction "go.kenn.io/docbank/document/production"
)

func TestPlayerAliasAmbiguityIsExplicit(t *testing.T) {
	snapshot := documentproduction.PlayersSnapshot{
		Contract: documentproduction.PlayersSnapshotContractV1,
		ID:       "11111111-1111-4111-8111-111111111111", Revision: 2,
		Players: []documentproduction.Player{
			{ID: "22222222-2222-4222-8222-222222222222", DisplayName: "Synthetic Person One",
				Aliases: []string{"Shared Alias", "one@example.test"}, EvidenceSHA256: approvalTestSHA("1")},
			{ID: "33333333-3333-4333-8333-333333333333", DisplayName: "Synthetic Person Two",
				Aliases: []string{"shared  alias", "two@example.test"}, EvidenceSHA256: approvalTestSHA("2")},
		},
	}
	prepared, err := PreparePlayersSnapshot("44444444-4444-4444-8444-444444444444", snapshot)
	require.NoError(t, err)
	require.Equal(t, prepared.SnapshotSHA256, prepared.Snapshot.SHA256)
	require.NotEmpty(t, prepared.Canonical)

	_, err = ResolvePlayerAlias(prepared.Snapshot, " SHARED alias ")
	requirePlayerAliasProblem(t, err, []string{snapshot.Players[0].ID, snapshot.Players[1].ID})
	_, err = ResolvePlayerAlias(prepared.Snapshot, "missing@example.test")
	requirePlayerAliasProblem(t, err, []string{})

	resolved, err := ResolvePlayerAlias(prepared.Snapshot, "ONE@example.test")
	require.NoError(t, err)
	require.Equal(t, snapshot.Players[0].ID, resolved.ID)

	changed := snapshot
	changed.Revision++
	changed.Players = append([]documentproduction.Player(nil), snapshot.Players...)
	changed.Players[0].EvidenceSHA256 = approvalTestSHA("3")
	revised, err := PreparePlayersSnapshot("55555555-5555-4555-8555-555555555555", changed)
	require.NoError(t, err)
	require.NotEqual(t, prepared.SnapshotSHA256, revised.SnapshotSHA256)
}

func requirePlayerAliasProblem(t *testing.T, err error, ids []string) {
	t.Helper()
	require.Error(t, err)
	problem := &documentproduction.Problem{}
	require.ErrorAs(t, err, &problem)
	require.Equal(t, documentproduction.ProblemPolicyUnsatisfied, problem.Code)
	require.Equal(t, ids, problem.IDs)
}
