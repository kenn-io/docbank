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

type factsFailAtWriter struct {
	at    int
	calls int
	err   error
}

func (w *factsFailAtWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.at {
		return 0, w.err
	}
	return len(p), nil
}

func TestDocumentFactsTableWriteErrors(t *testing.T) {
	want := errors.New("write failed")
	t.Run("people state", func(t *testing.T) {
		w := &factsFailAtWriter{at: 1, err: want}
		require.ErrorIs(t, writeVersionPeopleTable(w, &api.VersionPeople{State: "none"}), want)
	})
	t.Run("dates state", func(t *testing.T) {
		w := &factsFailAtWriter{at: 1, err: want}
		require.ErrorIs(t, writeVersionDatesTable(w, &api.VersionDates{State: "none"}), want)
	})
	t.Run("dates diagnostic", func(t *testing.T) {
		w := &factsFailAtWriter{at: 2, err: want}
		result := &api.VersionDates{State: "failed", Diagnostics: []api.FactDiagnostic{{Detail: "synthetic failure"}}}
		require.ErrorIs(t, writeVersionDatesTable(w, result), want)
	})
}

func TestDocumentDatesTablePublishedClaim(t *testing.T) {
	result := &api.VersionDates{State: "published", Claims: []api.VersionDateClaim{{
		Primary: true, DateKind: "sent", DateValue: "2024-03-15", ZoneText: "America/Vancouver", Precision: "date", ClaimBasis: "source_asserted",
	}}}
	var output strings.Builder
	require.NoError(t, writeVersionDatesTable(&output, result))
	require.Equal(t, "State: published\n* sent  2024-03-15  America/Vancouver  date  From document\n", output.String())
}

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

func TestDocumentPeopleTableAssertionSupport(t *testing.T) {
	edge := api.VersionPersonEdge{PersonID: "person", DisplayName: "Author", Role: "author", Basis: "operator_assigned", EvidenceKind: "operator_assertion", EvidenceID: "assertion"}
	assertion := api.VersionPersonAssertion{AssertionID: "assertion", PersonID: "person", DisplayName: "Author", Role: "author", Action: "assert"}
	for _, tc := range []struct {
		name      string
		state     string
		edges     []api.VersionPersonEdge
		assertion api.VersionPersonAssertion
		wantRow   bool
	}{
		{name: "represented", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: assertion},
		{name: "none", state: "none", assertion: assertion, wantRow: true},
		{name: "pending", state: "pending", assertion: assertion, wantRow: true},
		{name: "failed", state: "failed", assertion: assertion, wantRow: true},
		{name: "published without edge", state: "published", assertion: assertion, wantRow: true},
		{name: "suppress", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.Action = "suppress"; return a }(), wantRow: true},
		{name: "withdrawn", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.Action = "withdrawn"; return a }(), wantRow: true},
		{name: "new assertion", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.AssertionID = "new"; return a }(), wantRow: true},
		{name: "different person", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.PersonID = "other"; return a }(), wantRow: true},
		{name: "different role", state: "published", edges: []api.VersionPersonEdge{edge}, assertion: func() api.VersionPersonAssertion { a := assertion; a.Role = "sender"; return a }(), wantRow: true},
		{name: "different evidence kind", state: "published", edges: []api.VersionPersonEdge{func() api.VersionPersonEdge { e := edge; e.EvidenceKind = "custodian_assignment"; return e }()}, assertion: assertion, wantRow: true},
		{name: "empty identity", state: "published", edges: []api.VersionPersonEdge{func() api.VersionPersonEdge { e := edge; e.EvidenceID = ""; return e }()}, assertion: func() api.VersionPersonAssertion { a := assertion; a.AssertionID = ""; return a }(), wantRow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := api.VersionPeople{State: tc.state, Edges: tc.edges, Assertions: []api.VersionPersonAssertion{tc.assertion}, Unresolved: []api.VersionUnresolvedName{{Role: "author", RawLabel: "Unknown"}}}
			var before strings.Builder
			require.NoError(t, writeVersionJSON(&before, &result))
			var table strings.Builder
			require.NoError(t, writeVersionPeopleTable(&table, &result))
			require.Contains(t, table.String(), "Unknown  unresolved")
			if len(tc.edges) != 0 {
				require.Contains(t, table.String(), "Assigned by you")
			}
			if tc.wantRow {
				require.Contains(t, table.String(), tc.assertion.Action+"\n")
			} else {
				require.NotContains(t, table.String(), "assert\n")
			}
			var output strings.Builder
			require.NoError(t, writeVersionJSON(&output, &result))
			var decoded api.VersionPeople
			require.NoError(t, json.Unmarshal([]byte(output.String()), &decoded))
			require.Equal(t, before.String(), output.String())
			require.Equal(t, result.Assertions, decoded.Assertions)
			require.Len(t, decoded.Edges, len(tc.edges))
			if len(tc.edges) != 0 {
				require.Equal(t, tc.edges[0].EvidenceID, decoded.Edges[0].EvidenceID)
			}
		})
	}
}
