package mcp

import (
	"encoding/json/v2"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/plaintext"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestFindConflictsClippedSourceBoundaries(t *testing.T) {
	for _, boundary := range []string{"decimal", "segment start"} {
		t.Run(boundary, func(t *testing.T) {
			f := newNativeExportFixture(t)
			_, profile, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
			require.NoError(t, err)
			provider, err := plaintext.New(plaintext.Profile{MaxDocumentBytes: 1 << 20})
			require.NoError(t, err)
			profile.RenditionProvider = provider
			profile.Profile.Rendition.Name = "plaintext"
			profile.Profile.Rendition.AdapterContract = "plaintext.in-process/v1"
			profile.Profile.Rendition.RequestedArtifacts = []document.EvidenceArtifactRole{document.EvidenceArtifactStructured}
			profile.Profile.RetentionDisclosure.RetainTypedArtifacts = false
			profile.Profile.Rendition.Descriptor = document.ProviderDescriptorV1{ID: provider.Descriptor().ID, Fingerprint: provider.Descriptor().Fingerprint}
			profile.Profile.EvidenceLexical.MaxSegmentRunes = 1000
			daemon, service, profileName := newProcessingDocumentServerWithProfile(t, f, "local", profile)
			query := "cooling period"
			for i, value := range []string{"7", "15"} {
				var text string
				if boundary == "decimal" {
					query = "service fee"
					// Four highlight runes and the trailing marker leave 505 source runes.
					text = "The service fee " + strings.Repeat("x", 482) + fmt.Sprintf(" is $7.%d0. Contact support.", i+5)
				} else {
					qualifier := []string{"premium", "standard"}[i]
					prefix := "For " + qualifier + " accounts, "
					text = prefix + strings.Repeat("x", 1000-len(prefix)-1) + " the cooling period is " + value + " days."
				}
				written, err := f.blobs.WriteDetailedContext(t.Context(), strings.NewReader(text))
				require.NoError(t, err)
				encoding, err := written.EncodingName()
				require.NoError(t, err)
				node, err := f.catalog.CreateFile(t.Context(), f.catalog.RootID(), fmt.Sprintf("policy-%d.txt", i), written.Hash, written.Size, "text/plain",
					store.BlobPhysical{Encoding: encoding, StoredBytes: written.StoredSize, PackEligible: written.PackEligible, MD5: written.MD5, Created: written.Created})
				require.NoError(t, err)
				if boundary == "decimal" {
					require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
						BlobHash: written.Hash, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: text,
					}))
				} else {
					selector := processing.Selector{NodeID: node.ID, ContentVersionID: node.CurrentVersionID, Profile: profileName}
					plan, err := service.Plan(t.Context(), selector)
					require.NoError(t, err)
					_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
					require.NoError(t, err)
				}
			}
			reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
			args := map[string]any{"query": query, "profile": profileName, "filters": map[string]any{}}
			out := exportCall(t, daemon, "find_conflicts", args)
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			embedded, err := CallDocumentReadTool(t.Context(), reader, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "find_conflicts", Arguments: raw}})
			require.NoError(t, err)
			require.False(t, embedded.IsError)
			require.Equal(t, out, structuredMap(t, embedded.StructuredContent))
			passages, ok := out["passages"].([]any)
			require.True(t, ok)
			require.Len(t, passages, 2)
			require.Equal(t, "NOT_COMPARABLE", out["status"])
			for _, passage := range passages {
				require.Equal(t, false, schemaMap(t, passage)["compared"])
			}
		})
	}
}

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
			args := map[string]any{"query": "cooling period", "profile": profile}
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

func TestFindConflictsWrappedAndCutExcerpts(t *testing.T) {
	for _, tc := range []struct {
		name, a, b, status string
		cut                bool
	}{
		{"wrapped", "The cooling period is 7 days\nfor premium accounts.", "The cooling period is 15 days\nfor standard accounts.", "NOT_COMPARABLE", false},
		// Highlight bytes disappear after cropping, so the old cut was shorter
		// than 512 runes and left a seemingly complete assertion.
		{"cut", "The cooling period is 7 days.", "The cooling period is 15 days.", "CONFLICTING", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNativeExportFixture(t)
			texts := []string{tc.a, tc.b}
			if tc.cut {
				texts = append(texts, strings.Repeat("x", 600)+"\nThe cooling period is 30 days.")
			}
			for i, text := range texts {
				member := f.addFile(t, fmt.Sprintf("policy-%d.txt", i), text)
				require.NoError(t, f.catalog.RecordExtraction(t.Context(), store.ExtractionResult{
					BlobHash: member.SHA256, Extractor: "plain-text", ExtractorVersion: 1, Status: store.ExtractionOK, Text: text,
				}))
			}
			daemon, service, profile := newProcessingDocumentServer(t, f)
			reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
			args := map[string]any{"query": "cooling period", "profile": profile, "filters": map[string]any{}}
			out := exportCall(t, daemon, "find_conflicts", args)
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			embedded, err := CallDocumentReadTool(t.Context(), reader, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "find_conflicts", Arguments: raw}})
			require.NoError(t, err)
			require.Equal(t, out, structuredMap(t, embedded.StructuredContent))
			require.Equal(t, tc.status, out["status"])
			passages := out["passages"].([]any)
			require.Len(t, passages, len(texts))
			for _, entry := range passages {
				p := schemaMap(t, entry)
				require.Equal(t, p["path"] != "/policy-2.txt", p["compared"], p["path"])
			}
			if tc.cut {
				conflicts := out["conflicts"].([]any)
				require.Len(t, conflicts, 1)
				require.Len(t, schemaMap(t, conflicts[0])["figures"], 2)
				report, err := reader.SearchDocuments(t.Context(), api.DocumentSearchRequest{Query: "cooling period", Mode: "lexical", Profile: profile, ContentFirst: true, Scope: &api.DocumentSearchScope{Kind: "all_live", Filters: &api.DocumentSourceFenceFilters{}}})
				require.NoError(t, err)
				for _, hit := range report.Results {
					if hit.Path == "/policy-2.txt" {
						require.True(t, strings.HasPrefix(hit.Excerpt, " … "), hit.Excerpt)
						require.Less(t, len([]rune(hit.Excerpt)), 512)
					}
				}
			}
		})
	}
}

func TestFindConflictsRejectsRetrievalOptions(t *testing.T) {
	f := newNativeExportFixture(t)
	daemon, service, profile := newProcessingDocumentServer(t, f)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	for _, option := range []struct {
		key   string
		value any
	}{{"mode", "semantic"}, {"mode", "hybrid"}, {"binding_id", "synthetic-binding"}, {"explain", true}, {"rerank", true}} {
		t.Run(option.key+fmt.Sprint(option.value), func(t *testing.T) {
			args := map[string]any{"query": "cooling period", "profile": profile, "filters": map[string]any{}, option.key: option.value}
			wire := exchangeRaw(t, daemon, requestFor("tools/call", map[string]any{"name": "find_conflicts", "arguments": args}))
			wireErr := decodeWireError(t, wire)
			require.EqualValues(t, jsonrpc.CodeInvalidParams, wireErr.Code)
			require.Equal(t, "invalid tool arguments", wireErr.Message)
			raw, err := json.Marshal(args)
			require.NoError(t, err)
			_, err = CallDocumentReadTool(t.Context(), reader, &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "find_conflicts", Arguments: raw}})
			var rpcErr *jsonrpc.Error
			require.ErrorAs(t, err, &rpcErr)
			require.EqualValues(t, jsonrpc.CodeInvalidParams, rpcErr.Code)
			require.Equal(t, "invalid tool arguments", rpcErr.Message)
		})
	}
}
