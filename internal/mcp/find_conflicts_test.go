package mcp

import (
	"encoding/json/v2"
	"os"
	"strings"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/store"
)

func TestFindConflictsReportsDifferingFigures(t *testing.T) {
	f := newNativeExportFixture(t)
	var identities []api.DocumentIdentity
	versions := []string{}
	for _, file := range []struct{ name, sentence string }{
		{"cooling period 2025.txt", "The cooling period is 7 days."},
		{"policy-2026.txt", "The cooling period is 15 days."},
		{"premium.txt", "The premium cooling period is 30 days."},
		{"cooling period notes.txt", ""},
	} {
		text := "Policy overview. " + file.sentence + " Contact support for help."
		member := f.addFile(t, file.name, text)
		if file.sentence != "" {
			require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
				BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1,
				Status: store.ExtractionOK, Text: text,
			}))
		}
		identities = append(identities, api.DocumentIdentity{NodeID: member.NodeID, ContentVersionID: member.VersionID, Path: "/" + file.name})
		versions = append(versions, member.VersionID)
	}
	daemon, service, profile := newProcessingDocumentServer(t, f)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	docs, err := reader.ResolveDocumentSummaries(t.Context(), api.DocumentSummaryResolveRequest{Identities: identities})
	require.NoError(t, err)
	report, err := reader.SearchDocuments(t.Context(), api.DocumentSearchRequest{
		Query: "cooling period", Mode: "lexical", Profile: profile, ContentFirst: true,
		Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{}},
	})
	require.NoError(t, err)
	for _, hit := range report.Results {
		if hit.Path == "/cooling period 2025.txt" {
			require.True(t, answerContentExcerpt(hit))
			require.True(t, strings.HasPrefix(hit.Excerpt, "Policy overview."), hit.Excerpt)
			require.Contains(t, hit.Excerpt, "The cooling period is 7 days.")
		}
	}
	for _, scope := range []string{"filters", "content_version_ids"} {
		t.Run(scope, func(t *testing.T) {
			args := map[string]any{"query": "cooling period", "profile": profile, "mode": "lexical"}
			if scope == "filters" {
				args[scope] = map[string]any{}
			} else {
				args[scope] = versions
			}
			out := exportCall(t, daemon, "find_conflicts", args)
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			embedded, err := CallDocumentReadTool(t.Context(), reader, &sdkmcp.CallToolRequest{
				Params: &sdkmcp.CallToolParamsRaw{Name: "find_conflicts", Arguments: raw},
			})
			require.NoError(t, err)
			require.False(t, embedded.IsError)
			require.Equal(t, out, structuredMap(t, embedded.StructuredContent))
			_, outputSchema := findConflictsSchemas()
			assertSchemaAccepts(t, outputSchema, out)
			require.Equal(t, "CONFLICTING", out["status"])
			require.Equal(t, "lexical", out["actual_mode"])
			passages := out["passages"].([]any)
			require.Len(t, passages, 4)
			for _, entry := range passages {
				p := schemaMap(t, entry)
				path := p["path"].(string)
				require.Equal(t, path != "/cooling period notes.txt", p["compared"])
				require.NotEmpty(t, p["evidence_ids"])
				for _, doc := range docs {
					if doc.Path == path {
						require.Equal(t, doc.ModifiedAt, p["modified_at"])
						require.EqualValues(t, doc.NodeID, p["node_id"])
						require.Equal(t, doc.ContentVersionID, p["content_version_id"])
					}
				}
			}
			conflicts := out["conflicts"].([]any)
			require.Len(t, conflicts, 1)
			conflict := schemaMap(t, conflicts[0])
			require.Equal(t, "duration", conflict["kind"])
			require.Equal(t, "day", conflict["unit"])
			require.Equal(t, "the cooling period is", conflict["subject"])
			figures := conflict["figures"].([]any)
			require.Len(t, figures, 2)
			quotes := map[string]string{}
			for _, entry := range figures {
				figure := schemaMap(t, entry)
				quotes[figure["value"].(string)] = figure["quote"].(string)
				index := int(figure["passage"].(float64)) - 1
				p := schemaMap(t, passages[index])
				require.Equal(t, map[string]string{"7": "/cooling period 2025.txt", "15": "/policy-2026.txt"}[figure["value"].(string)], p["path"])
			}
			require.Equal(t, map[string]string{"7": "The cooling period is 7 days.", "15": "The cooling period is 15 days."}, quotes)
		})
	}
}
