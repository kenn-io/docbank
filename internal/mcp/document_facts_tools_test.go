package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/store"
)

func TestDocumentFactsToolsRejectAmbiguousSelection(t *testing.T) {
	for _, name := range []string{"get_document_dates", "get_document_people"} {
		for _, args := range [][]byte{[]byte(`{}`), []byte(`{"node_id":1,"content_version_id":"00000000-0000-4000-8000-000000000000"}`)} {
			_, err := documentFactsSelection(args)
			require.Error(t, err, name)
		}
	}
}

func TestDocumentFactsToolsUseSafeDisclosure(t *testing.T) {
	f := newNativeExportFixture(t)
	file := f.addFile(t, "example.txt", "synthetic")
	person, err := f.catalog.CreatePerson(t.Context(), "Bcc Person", "operator")
	require.NoError(t, err)
	_, err = f.catalog.AssertDocumentPerson(t.Context(), store.PersonDocumentAssertion{ContentVersionID: file.VersionID, PersonID: person.PersonID, Role: "blind_copy", Action: "assert", Revision: 1})
	require.NoError(t, err)
	server := newSearchProcessingServer(t, f, nil)
	for _, selector := range []map[string]any{{"node_id": file.NodeID}, {"content_version_id": file.VersionID}} {
		for _, name := range []string{"get_document_dates", "get_document_people"} {
			output := exportCall(t, server, name, selector)
			require.Equal(t, file.VersionID, output["content_version_id"])
			require.NotContains(t, output, "Bcc Person")
			if name == "get_document_people" {
				require.Empty(t, output["assertions"])
			}
		}
	}
}
