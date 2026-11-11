package daemonconn

import (
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"testing"
)

func TestSelectedMediaLexicalFallbackReceipt(t *testing.T) {
	version := "22222222-2222-4222-8222-222222222222"
	request := api.DocumentSearchRequest{Query: "fixture", Mode: "semantic", Profile: "supplied-transcript", Fence: api.DocumentSourceFence{VaultUID: "11111111-1111-4111-8111-111111111111", ContentVersionIDs: []string{version}}, MediaSources: api.MediaSearchSources{{SourceID: "source", SourceVersionID: "version", ContentVersionID: version}}}
	for _, mode := range []string{"semantic"} {
		request.Mode = mode
		report := api.DocumentSearchReport{RequestedMode: mode, ActualMode: "lexical", MediaSourceSelection: true, Degradations: []string{"semantic_unavailable"}, Coverage: api.DocumentSearchCoverage{BindingRequired: true, State: "incomplete", ScopedDocuments: 1, CompleteDocuments: 0}}
		require.NoError(t, validateDocumentSearchReport(request, report))
		if mode == "semantic" {
			bad := report
			bad.Degradations = nil
			require.Error(t, validateDocumentSearchReport(request, bad))
			ordinary := request
			ordinary.MediaSources = nil
			require.Error(t, validateDocumentSearchReport(ordinary, report))
		}
	}
}
