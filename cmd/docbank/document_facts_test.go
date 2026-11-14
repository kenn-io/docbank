package main

import (
	"encoding/json/v2"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentFactsTableWriteErrors(t *testing.T) {
	want := errors.New("write failed")
	w := reportTestWriter(func([]byte) (int, error) { return 0, want })
	t.Run("people state", func(t *testing.T) {
		require.ErrorIs(t, writeVersionPeopleTable(w, &api.VersionPeople{State: "none"}), want)
	})
	t.Run("dates state", func(t *testing.T) {
		require.ErrorIs(t, writeVersionDatesTable(w, &api.VersionDates{State: "none"}), want)
	})
	t.Run("dates diagnostic", func(t *testing.T) {
		calls := 0
		w := reportTestWriter(func(p []byte) (int, error) {
			calls++
			if calls == 2 {
				return 0, want
			}
			return len(p), nil
		})
		result := &api.VersionDates{State: "failed", Diagnostics: []api.FactDiagnostic{{Detail: "synthetic failure"}}}
		require.ErrorIs(t, writeVersionDatesTable(w, result), want)
	})
}

func TestDocumentFactsTableEscapesAndTimezones(t *testing.T) {
	t.Run("zone controls", func(t *testing.T) {
		result := &api.VersionDates{State: "published", Diagnostics: []api.FactDiagnostic{{Detail: "detail\n\x1b[31m"}}, Claims: []api.VersionDateClaim{{
			DateKind: "sent", DateValue: "2024-03-15T12:30:00", ZoneText: "zone\n\x1b[31m", Precision: "second", ClaimBasis: "source_asserted", When: api.WhenV1{State: "floating", ZoneState: "named"},
		}}}
		var output strings.Builder
		require.NoError(t, writeVersionDatesTable(&output, result))
		require.Contains(t, output.String(), `Diagnostic: "detail\n\x1b[31m"`+"\n")
		require.Contains(t, output.String(), `"zone\n\x1b[31m" (state=floating, zone="named")`)
		require.NotContains(t, output.String(), "\x1b")
	})

	t.Run("published claim", func(t *testing.T) {
		result := &api.VersionDates{State: "published", Claims: []api.VersionDateClaim{{
			Primary: true, DateKind: "vault_recorded", DateValue: "2024-03-15", Precision: "date", ClaimBasis: "source_asserted", When: api.WhenV1{State: "date"},
		}}}
		var output strings.Builder
		require.NoError(t, writeVersionDatesTable(&output, result))
		require.Equal(t, "State: published\n* Recorded  2024-03-15  \"\" (state=date, zone=\"\")  date  From document\n", output.String())
	})
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
	table, err := runCLI(t, "document", "people", "/notes.txt")
	require.NoError(t, err)
	require.Contains(t, table, "Synthetic Author")
	require.NotContains(t, table, "Synthetic Blind Recipient")
	fullTable, err := runCLI(t, "document", "people", "/notes.txt", "--full")
	require.NoError(t, err)
	require.Contains(t, fullTable, "Synthetic Blind Recipient")
	dateOutput, err := runCLI(t, "document", "dates", "/notes.txt", "--json")
	require.NoError(t, err)
	var dates api.VersionDates
	require.NoError(t, json.Unmarshal([]byte(dateOutput), &dates))
	require.Equal(t, f.nodes["notes.txt"].CurrentVersionID, dates.ContentVersionID)
	require.NotNil(t, dates.Claims)
	_, err = runCLI(t, "document", "people", "/empty")
	require.ErrorContains(t, err, "document has no current content version")
}

func TestDocumentPeopleTableBccLabels(t *testing.T) {
	result := api.VersionPeople{State: "published",
		Edges:      []api.VersionPersonEdge{{Role: "blind_copy", DisplayName: "Edge", Basis: "package_column"}, {Role: "last_saved_by", DisplayName: "Editor", Basis: "transfer_record"}},
		Assertions: []api.VersionPersonAssertion{{Role: "blind_copy", DisplayName: "Assertion", Action: "assert"}},
		Unresolved: []api.VersionUnresolvedName{{Role: "blind_copy", RawLabel: "Unresolved"}},
	}
	var table strings.Builder
	require.NoError(t, writeVersionPeopleTable(&table, &result))
	require.Equal(t, 3, strings.Count(table.String(), "Bcc"))
	require.Contains(t, table.String(), "From package")
	require.Contains(t, table.String(), `Last editor  "Editor"`)
	require.Contains(t, table.String(), "From transfer")
	escaped := &api.VersionPeople{State: "published",
		Edges:      []api.VersionPersonEdge{{Role: "author", DisplayName: "edge\n\x1b[31m", Basis: "operator_assigned"}},
		Assertions: []api.VersionPersonAssertion{{Role: "sender", DisplayName: "assertion\n\x1b[31m", Action: "suppress"}},
		Unresolved: []api.VersionUnresolvedName{{Role: "recipient", RawLabel: "unresolved\n\x1b[31m"}},
	}
	var escapedOutput strings.Builder
	require.NoError(t, writeVersionPeopleTable(&escapedOutput, escaped))
	require.Contains(t, escapedOutput.String(), `"edge\n\x1b[31m"`)
	require.Contains(t, escapedOutput.String(), `"assertion\n\x1b[31m"`)
	require.Contains(t, escapedOutput.String(), `"unresolved\n\x1b[31m"`)
	require.NotContains(t, escapedOutput.String(), "\x1b")
}

func TestDocumentPeopleTableAssertionSupport(t *testing.T) {
	edge := api.VersionPersonEdge{PersonID: "person", DisplayName: "Author", Role: "author", Basis: "operator_assigned", EvidenceKind: "operator_assertion", EvidenceID: "assertion"}
	assertion := api.VersionPersonAssertion{AssertionID: "assertion", PersonID: "person", DisplayName: "Author", Role: "author", Action: "assert"}
	for _, tc := range []struct {
		name      string
		edges     []api.VersionPersonEdge
		assertion api.VersionPersonAssertion
		wantRow   bool
	}{
		{name: "represented", edges: []api.VersionPersonEdge{edge}, assertion: assertion},
		{name: "published without edge", assertion: assertion, wantRow: true},
		{name: "new assertion", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.AssertionID = "new"; return a }(), wantRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := api.VersionPeople{State: "published", Edges: tc.edges, Assertions: []api.VersionPersonAssertion{tc.assertion}}
			var table strings.Builder
			require.NoError(t, writeVersionPeopleTable(&table, &result))
			if len(tc.edges) != 0 {
				require.Contains(t, table.String(), "Assigned by you")
			}
			if tc.wantRow {
				require.Contains(t, table.String(), tc.assertion.Action+"\n")
			} else {
				require.NotContains(t, table.String(), "assert\n")
			}
		})
	}
}
