package mcp

import (
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
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
			assertSchemaAccepts(t, catalogMap(toolCatalog(ServerOptions{}))[name].OutputSchema, output)
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
	versionID := "00000000-0000-4000-8000-000000000001"
	dates := api.VersionDates{ContentVersionID: versionID, NodeID: 1, State: "published"}
	axis, err := document.EventAxisKey("2026-03-10", "date", "omitted", nil)
	require.NoError(t, err)
	for i := range 12 {
		evidenceID := fmt.Sprintf("metadata-%d", i)
		evidenceHash := strings.Repeat("a", 64)
		eventID := document.DocumentEventID("vault-uid-1", versionID, evidenceID, "created", "2026-03-10", "date", "omitted", evidenceHash)
		claim := api.VersionDateClaim{EventID: eventID, DateKind: "created", DateValue: "2026-03-10", RawValue: strings.Repeat("\\\"", 30000), Precision: "date", TimezoneKind: "omitted", AxisKey: axis, ClaimBasis: "source_asserted", ParseConfidence: "exact", EvidenceKind: "source_metadata", EvidenceID: evidenceID,
			When: api.WhenV1{State: "date", Precision: "date", Date: "2026-03-10", PlacedBy: api.WhenPlacedBy{Source: "created", Rule: document.PrimaryRuleV1, EventID: eventID, ClaimBasis: "source_asserted"}}}
		dates.Claims = append(dates.Claims, claim)
	}
	dates.Claims[0].Primary = true
	dates.Primary = &api.VersionDatePrimary{EventID: dates.Claims[0].EventID, Reason: "created_date", RuleID: document.PrimaryRuleV1, Disclosure: "safe"}
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
		if offset == 0 {
			require.Less(t, *page.NextOffset, len(dates.Claims))
		}
		offset = *page.NextOffset
	}
	require.Equal(t, dates.Claims, claims)
	require.Equal(t, dates.Diagnostics, diagnostics)
	require.Positive(t, offset)
	dates.Diagnostics = []api.FactDiagnostic{{Code: "warning", Detail: strings.Repeat("x", maxToolResponseBytes)}}
	dates.Claims = nil
	_, err = documentDatesPage(dates, 0)
	require.ErrorIs(t, err, errToolResultTooLarge)
}

func TestDocumentPeoplePagePreservesAllArraysWithinEnvelope(t *testing.T) {
	people := api.VersionPeople{ContentVersionID: "00000000-0000-4000-8000-000000000001", State: "published", Open: &api.FactOpen{Web: "https://example.test"}}
	for i := range 40 {
		edge := api.VersionPersonEdge{PersonID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), DisplayName: "Example Person", Role: "author", Basis: "identifier_match", Confidence: "exact_identifier", EvidenceKind: "source_metadata", EvidenceID: "metadata", RawLabel: "Example Person", ClaimCount: 1, LinkedContact: true}
		for j := range document.MaxPersonExternalIdentities {
			edge.External = append(edge.External, api.FactExternal{System: "msgvault", ArchiveID: fmt.Sprintf("archive-%03d-%s", j, strings.Repeat("a", 110)), UID: fmt.Sprintf("person-%03d-%s", i, strings.Repeat("u", 240))})
		}
		people.Edges = append(people.Edges, edge)
	}
	people.Assertions = []api.VersionPersonAssertion{{AssertionID: "00000000-0000-4000-8000-000000000101", PersonID: people.Edges[0].PersonID, DisplayName: "Example Person", Role: "author", Action: "assert", RecordedAt: "2026-03-10T00:00:00Z", Revision: 1}}
	people.Unresolved = []api.VersionUnresolvedName{{RawLabel: "Unresolved Person", Role: "author", CandidateID: "00000000-0000-4000-8000-000000000102", EvidenceKind: "source_metadata"}}
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
		if offset == 0 {
			require.Less(t, *page.NextOffset, len(people.Edges))
		}
		offset = *page.NextOffset
	}
	require.Equal(t, people.Edges, edges)
	require.Equal(t, people.Assertions, assertions)
	require.Equal(t, people.Unresolved, unresolved)
}

func TestDocumentFactsOutputSchemaBounds(t *testing.T) {
	_, dates := getDocumentDatesSchemas()
	_, people := getDocumentPeopleSchemas()
	asSchema := func(value any) schema {
		result, ok := value.(map[string]any)
		require.True(t, ok)
		return result
	}
	properties := func(s schema) schema { return asSchema(s["properties"]) }
	items := func(s schema) schema { return asSchema(s["items"]) }
	for _, tc := range []struct {
		root  schema
		field string
	}{
		{dates, "claims"}, {dates, "diagnostics"}, {people, "edges"}, {people, "assertions"}, {people, "unresolved"},
	} {
		array := asSchema(properties(tc.root)[tc.field])
		// Isolate the catalog array bound from its item requirements.
		bound := maps.Clone(array)
		bound["items"] = schema{}
		assertSchemaAccepts(t, bound, make([]any, 100))
		assertSchemaRejects(t, bound, make([]any, 101))
	}
	claim := properties(items(asSchema(properties(dates)["claims"])))
	edge := properties(items(asSchema(properties(people)["edges"])))
	external := properties(items(asSchema(edge["external"])))
	for _, tc := range []struct {
		field any
		limit int
	}{
		{claim["raw_value"], document.MaxDocumentEventRawValueBytes},
		{edge["display_name"], document.MaxPersonDisplayNameBytes},
		{edge["raw_label"], document.MaxPersonDisplayNameBytes},
		{external["archive_id"], document.MaxPersonArchiveIDBytes},
		{external["uid"], document.MaxPersonExternalUIDBytes},
	} {
		assertSchemaAccepts(t, tc.field, strings.Repeat("x", tc.limit))
		assertSchemaRejects(t, tc.field, strings.Repeat("x", tc.limit+1))
	}
	require.Equal(t, document.MaxPersonExternalIdentities, asSchema(edge["external"])["maxItems"])
}
