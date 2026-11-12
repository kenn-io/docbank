package main

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
)

func TestPartyExplicitRevisionMustBePositive(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"edit", "id", "--name", "Example", "--revision=0"}, {"retire", "id", "--revision=0"}, {"retire", "id", "--revision=-1"}} {
		cmd := newPartyCommand()
		cmd.SetArgs(args)
		require.ErrorContains(t, cmd.Execute(), "revision must be positive")
	}
}

func TestPartyCLIShowsPopulatedAndRetiredParties(t *testing.T) {
	setupVaultHome(t)
	out, err := runCLI(t, "people", "create", "Synthetic Person")
	require.NoError(t, err, out)
	var person api.Person
	require.NoError(t, json.Unmarshal([]byte(out), &person))

	out, err = runCLI(t, "party", "create", "Synthetic Holdings", "--kind", "organization", "--legal-name", "Synthetic Holdings Limited", "--country", "CA", "--person", person.PersonID, "--own", "--json")
	require.NoError(t, err, out)
	var party api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &party))
	require.NotEmpty(t, party.PartyID)
	out, err = runCLI(t, "party", "create", "Synthetic Counterparty", "--json")
	require.NoError(t, err, out)

	out, err = runCLI(t, "party", "show", party.PartyID)
	require.NoError(t, err, out)
	for _, field := range []string{party.PartyID, "organization", "Synthetic Holdings", "legal name: Synthetic Holdings Limited", "country: CA", "own: true", "person: " + person.PersonID, "retired: ", "revision 1", "created: " + party.CreatedAt, "updated: " + party.UpdatedAt} {
		assert.Contains(t, out, field)
	}

	out, err = runCLI(t, "party", "retire", party.PartyID, "--revision", "1", "--json")
	require.NoError(t, err, out)
	var retired api.Party
	require.NoError(t, json.Unmarshal([]byte(out), &retired))
	require.NotNil(t, retired.RetiredAt)
	out, err = runCLI(t, "party", "show", party.PartyID)
	require.NoError(t, err, out)
	assert.Contains(t, out, "retired: "+*retired.RetiredAt)
	assert.Contains(t, out, "revision 2")
	assert.Contains(t, out, "person: "+person.PersonID)
	out, err = runCLI(t, "party", "list", "--include-retired")
	require.NoError(t, err, out)
	assert.Contains(t, out, party.PartyID)
	assert.Contains(t, out, "retired: "+*retired.RetiredAt)
	assert.NotContains(t, out, "--offset", "list shorter than the limit has no continuation cue")

	// Two parties exist, so --limit 2 returns a full page that may or may not
	// have a successor; the cue must say "may" and make no total claim.
	out, err = runCLI(t, "party", "list", "--include-retired", "--limit", "2")
	require.NoError(t, err, out)
	assert.Contains(t, out, "more parties may exist")
	assert.Contains(t, out, "--offset 2")
	assert.NotContains(t, out, "total")
	assert.NotContains(t, out, "truncated")

	// The next offset comes from the returned page bounds: offset 1 plus one item.
	out, err = runCLI(t, "party", "list", "--include-retired", "--limit", "1", "--offset", "1")
	require.NoError(t, err, out)
	assert.Contains(t, out, "more parties may exist")
	assert.Contains(t, out, "--offset 2")

	// Partial and empty pages carry no cue.
	for _, args := range [][]string{{"--limit", "3"}, {"--limit", "2", "--offset", "5"}} {
		out, err = runCLI(t, append([]string{"party", "list", "--include-retired"}, args...)...)
		require.NoError(t, err, out)
		assert.NotContains(t, out, "more parties may exist", args)
		assert.NotContains(t, out, "--offset", args)
	}

	// JSON output stays a single valid JSON document with no human cue.
	out, err = runCLI(t, "party", "list", "--include-retired", "--limit", "2", "--json")
	require.NoError(t, err, out)
	var page api.PartyPage
	require.NoError(t, json.Unmarshal([]byte(out), &page), out)
	assert.Len(t, page.Items, 2)
	assert.Equal(t, 2, page.Limit)
	assert.NotContains(t, out, "more parties may exist")
}
