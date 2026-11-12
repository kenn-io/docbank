package main

import (
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentFactsCommandRegistration(t *testing.T) {
	for _, path := range [][]string{{"people", "list"}, {"document", "people"}, {"document", "dates"}} {
		cmd, remaining, err := rootCmd.Find(path)
		require.NoError(t, err)
		require.Empty(t, remaining)
		require.Equal(t, path[len(path)-1], cmd.Name())
	}
}

func TestDocumentFactsCommands(t *testing.T) {
	f := newDocumentInspectionFixture(t)
	catalog, err := store.Open(t.Context(), filepath.Join(f.root, "docbank.db"))
	require.NoError(t, err)
	person, err := catalog.CreatePerson(t.Context(), "Synthetic Author", "operator")
	require.NoError(t, err)
	_, err = catalog.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: f.nodes["notes.txt"].CurrentVersionID, PersonID: person.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	blind, err := catalog.CreatePerson(t.Context(), "Synthetic Blind Recipient", "operator")
	require.NoError(t, err)
	_, err = catalog.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: f.nodes["notes.txt"].CurrentVersionID, PersonID: blind.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	_, err = catalog.Mkdir(t.Context(), catalog.RootID(), "empty")
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	t.Setenv("DOCBANK_HOME", f.root)
	stop := startServe(t)
	t.Cleanup(stop)
	waitForDaemon(t, f.root)
	for _, noun := range []string{"dates", "people"} {
		table, err := runCLI(t, "document", noun, "/notes.txt")
		require.NoError(t, err)
		require.Contains(t, table, "State: ")
		if noun == "people" {
			require.Contains(t, table, "Synthetic Author")
			require.Contains(t, table, "author")
			require.Contains(t, table, "assert")
		}
		safeOutput, err := runCLI(t, "document", noun, "/notes.txt", "--json")
		require.NoError(t, err)
		if noun == "people" {
			var safe api.VersionPeople
			require.NoError(t, json.Unmarshal([]byte(safeOutput), &safe))
			require.Len(t, safe.Assertions, 1)
			require.Equal(t, person.PersonID, safe.Assertions[0].PersonID)
		}
		_, err = runCLI(t, "document", noun, "/empty")
		require.ErrorContains(t, err, "document has no current content version")
		output, err := runCLI(t, "document", noun, "/notes.txt", "--json", "--full")
		require.NoError(t, err)
		if noun == "dates" {
			var got api.VersionDates
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.Equal(t, f.nodes["notes.txt"].CurrentVersionID, got.ContentVersionID)
		} else {
			var got api.VersionPeople
			require.NoError(t, json.Unmarshal([]byte(output), &got))
			require.Equal(t, f.nodes["notes.txt"].CurrentVersionID, got.ContentVersionID)
			require.Len(t, got.Assertions, 2)
			roles := map[string]string{}
			for _, assertion := range got.Assertions {
				roles[assertion.PersonID] = assertion.Role
			}
			require.Equal(t, "author", roles[person.PersonID])
			require.Equal(t, "blind_copy", roles[blind.PersonID])
		}
	}
}
