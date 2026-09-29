package main

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/store"
)

func TestPeopleCLIWorkflow(t *testing.T) {
	for _, name := range []string{"list", "show", "custodians", "create", "rename", "retire", "merge", "split"} {
		command, _, err := peopleCmd.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, command.Name())
	}

	dir := t.TempDir()
	t.Setenv("DOCBANK_HOME", dir)
	catalog, err := store.Open((home.Layout{Root: dir}).DBPath())
	require.NoError(t, err)
	seed, err := catalog.CreatePerson(t.Context(), "Synthetic seed", "operator")
	require.NoError(t, err)
	identity, err := catalog.AddPersonIdentity(t.Context(), seed.PersonID, seed.Revision, store.PersonIdentity{
		Kind: "name_alias", ValueDisplay: "Synthetic seed", Origin: "operator",
		EvidenceKind: "operator_assertion", EvidenceID: "synthetic-cli", Confidence: "operator_asserted",
	})
	require.NoError(t, err)
	run, err := catalog.BeginIngest(t.Context(), "cli", "synthetic CLI collection")
	require.NoError(t, err)
	node, err := catalog.IngestFileExact(t.Context(), run, catalog.RootID(), "cli-document.txt", strings.Repeat("a", 64), int64(len("synthetic CLI document")), "text/plain", "cli-document.txt", "")
	require.NoError(t, err)
	assignment, err := catalog.SetCustodian(t.Context(), store.CustodianRequest{
		Scope: store.CustodianScope{Kind: "document", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}, PersonID: seed.PersonID,
		RawLabel: "Synthetic seed", Rank: "primary", Basis: "operator_assigned", SourceRef: "synthetic-cli", IfMatchRevision: 1,
	})
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	startTestDaemon(t, dir)

	createdOutput, err := runCLI(t, "people", "create", "Synthetic Ada")
	require.NoError(t, err)
	var created api.Person
	require.NoError(t, json.Unmarshal([]byte(createdOutput), &created))
	assert.Equal(t, "Synthetic Ada", created.DisplayName)

	showOutput, err := runCLI(t, "people", "show", created.PersonID)
	require.NoError(t, err)
	var shown api.PersonDetail
	require.NoError(t, json.Unmarshal([]byte(showOutput), &shown))
	assert.Equal(t, created.PersonID, shown.PersonID)
	custodiansOutput, err := runCLI(t, "people", "custodians", seed.PersonID)
	require.NoError(t, err)
	var custodians api.CustodianPage
	require.NoError(t, json.Unmarshal([]byte(custodiansOutput), &custodians))
	require.Len(t, custodians.Items, 1)
	assert.Equal(t, assignment.AssignmentID, custodians.Items[0].AssignmentID)
	assert.Equal(t, node.ID, custodians.Items[0].NodeID)
	assert.Equal(t, node.CurrentVersionID, custodians.Items[0].ContentVersionID)

	renamedOutput, err := runCLI(t, "people", "rename", created.PersonID, "Renamed Ada", "--revision", strconv.FormatInt(created.Revision, 10))
	require.NoError(t, err)
	var renamed api.Person
	require.NoError(t, json.Unmarshal([]byte(renamedOutput), &renamed))
	assert.Equal(t, "Renamed Ada", renamed.DisplayName)

	listOutput, err := runCLI(t, "people", "list", "Renamed")
	require.NoError(t, err)
	var page api.PersonPage
	require.NoError(t, json.Unmarshal([]byte(listOutput), &page))
	require.Len(t, page.Items, 1)
	assert.Equal(t, created.PersonID, page.Items[0].PersonID)

	mergeOutput, err := runCLI(t, "people", "merge", created.PersonID, seed.PersonID,
		"--revision", strconv.FormatInt(renamed.Revision, 10), "--absorbed-revision", strconv.FormatInt(seed.Revision+1, 10),
		"--operation-id", "00000000-0000-4000-8000-000000000003")
	require.NoError(t, err)
	var merge api.PersonMergeReceipt
	require.NoError(t, json.Unmarshal([]byte(mergeOutput), &merge))
	assert.Equal(t, created.PersonID, merge.SurvivorPersonID)

	staleOutput, staleErr := runCLI(t, "people", "rename", created.PersonID, "Stale Ada", "--revision", strconv.FormatInt(renamed.Revision, 10))
	require.ErrorIs(t, staleErr, store.ErrStaleRevision, staleOutput)
	assert.Equal(t, exitStale, commandExitCode(staleErr, true))

	invalidSplitOutput, invalidSplitErr := runCLI(t, "people", "split", created.PersonID, "--revision", strconv.FormatInt(merge.SurvivorRevisionAfter, 10),
		"--display-name", "Invalid split", "--identity", identity.IdentityID, "--identity", identity.IdentityID)
	require.ErrorIs(t, invalidSplitErr, store.ErrInvalidPerson, invalidSplitOutput)
	assert.Equal(t, exitUsage, commandExitCode(invalidSplitErr, true))

	splitOutput, err := runCLI(t, "people", "split", created.PersonID, "--revision", strconv.FormatInt(merge.SurvivorRevisionAfter, 10),
		"--display-name", "Split Ada", "--identity", identity.IdentityID)
	require.NoError(t, err)
	var split api.PersonSplitReceipt
	require.NoError(t, json.Unmarshal([]byte(splitOutput), &split))
	assert.Equal(t, created.PersonID, split.SourcePersonID)
	assert.Equal(t, merge.SurvivorRevisionAfter+1, split.SourceRevisionAfter)

	retiredOutput, err := runCLI(t, "people", "retire", created.PersonID, "--revision", strconv.FormatInt(split.SourceRevisionAfter, 10))
	require.NoError(t, err)
	var retired api.Person
	require.NoError(t, json.Unmarshal([]byte(retiredOutput), &retired))
	assert.Equal(t, "retired", retired.State)

	_, err = runCLI(t, "people", "show", created.PersonID)
	require.ErrorIs(t, err, store.ErrNotFound)
}
