package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentFactsToolsRejectAmbiguousSelection(t *testing.T) {
	for _, args := range [][]byte{[]byte(`{}`), []byte(`{"node_id":1,"content_version_id":"00000000-0000-4000-8000-000000000000"}`)} {
		_, err := documentFactsSelection(args)
		require.Error(t, err)
	}
}

func TestDocumentFactsToolsUseSafeDisclosure(t *testing.T) {
	f := newNativeExportFixture(t)
	file := f.addFile(t, "example.txt", "synthetic")
	person, err := f.catalog.CreatePerson(t.Context(), "Bcc Person", "operator")
	require.NoError(t, err)
	_, err = f.catalog.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: file.VersionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	author, err := f.catalog.CreatePerson(t.Context(), "Visible Author", "operator")
	require.NoError(t, err)
	_, err = f.catalog.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: file.VersionID, PersonID: author.PersonID, Role: "author", Action: "assert", Revision: 1})
	require.NoError(t, err)
	directory, err := f.catalog.Mkdir(t.Context(), f.catalog.RootID(), "empty")
	require.NoError(t, err)
	server := newSearchProcessingServer(t, f, nil)
	for _, selector := range []map[string]any{{"node_id": file.NodeID}, {"content_version_id": file.VersionID}} {
		for _, name := range []string{"get_document_dates", "get_document_people"} {
			output := exportCall(t, server, name, selector)
			require.Equal(t, file.VersionID, output["content_version_id"])
			if name == "get_document_people" {
				assertions, ok := output["assertions"].([]any)
				require.True(t, ok)
				require.Len(t, assertions, 1)
				assertion, ok := assertions[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "Visible Author", assertion["display_name"])
			}
		}
	}
	for _, name := range []string{"get_document_dates", "get_document_people"} {
		output := exportCall(t, server, name, map[string]any{"node_id": directory.ID})
		require.Equal(t, "no_content_version", output["code"])
		require.Contains(t, output["message"], "no current content version")
	}
}

func TestDocumentDatesPagePreservesClaimsAndPrimaryWithinEnvelope(t *testing.T) {
	dates := api.VersionDates{ContentVersionID: "00000000-0000-4000-8000-000000000001", NodeID: 1, State: "published", Primary: &api.VersionDatePrimary{EventID: "primary"}}
	for i := range 12 {
		dates.Claims = append(dates.Claims, api.VersionDateClaim{EventID: fmt.Sprint(i), RawValue: strings.Repeat("\\\"", 50000)})
	}
	dates.Diagnostics = []api.FactDiagnostic{{Code: "warning", Detail: "synthetic"}}
	var claims []api.VersionDateClaim
	var diagnostics []api.FactDiagnostic
	offset := 0
	for {
		page, err := documentDatesPage(dates, offset)
		require.NoError(t, err)
		size, err := toolEnvelopeBytes(page, nil)
		require.NoError(t, err)
		require.Less(t, size, maxToolResponseBytes)
		require.Equal(t, dates.Primary, page.Primary)
		claims = append(claims, page.Claims...)
		diagnostics = append(diagnostics, page.Diagnostics...)
		if page.NextOffset == nil {
			break
		}
		require.Greater(t, *page.NextOffset, offset)
		offset = *page.NextOffset
	}
	require.Equal(t, dates.Claims, claims)
	require.Equal(t, dates.Diagnostics, diagnostics)
	require.Greater(t, offset, 0)
	dates.Diagnostics = []api.FactDiagnostic{{Code: "warning", Detail: strings.Repeat("x", maxToolResponseBytes)}}
	dates.Claims = nil
	_, err := documentDatesPage(dates, 0)
	require.ErrorIs(t, err, errToolResultTooLarge)
}

func TestDocumentPeoplePagePreservesAllArraysWithinEnvelope(t *testing.T) {
	people := api.VersionPeople{ContentVersionID: "00000000-0000-4000-8000-000000000001", State: "published", Open: &api.FactOpen{Web: "https://example.test"}}
	for i := range 105 {
		people.Edges = append(people.Edges, api.VersionPersonEdge{PersonID: fmt.Sprint(i), RawLabel: strings.Repeat("x", 10000)})
	}
	people.Assertions = []api.VersionPersonAssertion{{DisplayName: "assertion"}}
	people.Unresolved = []api.VersionUnresolvedName{{RawLabel: "unresolved"}}
	var edges []api.VersionPersonEdge
	var assertions []api.VersionPersonAssertion
	var unresolved []api.VersionUnresolvedName
	for offset := 0; ; {
		page, err := documentPeoplePage(people, offset)
		require.NoError(t, err)
		size, err := toolEnvelopeBytes(page, nil)
		require.NoError(t, err)
		require.Less(t, size, maxToolResponseBytes)
		require.Equal(t, people.ContentVersionID, page.ContentVersionID)
		require.Equal(t, people.State, page.State)
		require.Equal(t, people.Open, page.Open)
		edges = append(edges, page.Edges...)
		assertions = append(assertions, page.Assertions...)
		unresolved = append(unresolved, page.Unresolved...)
		if page.NextOffset == nil {
			break
		}
		require.Greater(t, *page.NextOffset, offset)
		offset = *page.NextOffset
	}
	require.Equal(t, people.Edges, edges)
	require.Equal(t, people.Assertions, assertions)
	require.Equal(t, people.Unresolved, unresolved)
	people.Edges = []api.VersionPersonEdge{{RawLabel: strings.Repeat("x", maxToolResponseBytes)}}
	_, err := documentPeoplePage(people, 0)
	require.ErrorIs(t, err, errToolResultTooLarge)
}
