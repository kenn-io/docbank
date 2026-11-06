package mcp

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestSearchDocumentsRealProviderSelection(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund.txt", "Refunds take five days.")
	profiles := map[string]processing.ProfileConfig{
		"a-outage":  newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, true),
		"z-healthy": newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, false),
	}
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: profiles})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := newSearchProcessingServer(t, f, service)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	// Grant query egress through the same preview and consent flow as processing.
	page, err := reader.ListDocuments(t.Context(), api.DocumentQuery{})
	require.NoError(t, err)
	for name := range profiles {
		selector := processing.Selector{NodeID: page.Items[0].NodeID, ContentVersionID: page.Items[0].ContentVersionID, Profile: name}
		plan, err := service.Plan(t.Context(), selector)
		require.NoError(t, err)
		if name == "a-outage" {
			_, err = service.GrantConsent(t.Context(), processing.ConsentGrantRequest{Selector: selector, PlanFingerprint: plan.Fingerprint})
		} else {
			_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
		}
		require.NoError(t, err)
	}
	for _, failure := range []string{"index_unavailable", "processing_failed"} {
		if failure == "processing_failed" {
			selector := processing.Selector{NodeID: page.Items[0].NodeID, ContentVersionID: page.Items[0].ContentVersionID, Profile: "a-outage"}
			plan, err := service.Plan(t.Context(), selector)
			require.NoError(t, err)
			_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
			require.NoError(t, err)
		}
		answer := exportCall(t, daemon, "answer_question", map[string]any{"question": "refund"})
		require.Equal(t, "z-healthy", schemaMap(t, schemaMap(t, answer["receipt"])["profile"])["name"], answer)
		for _, embedded := range []bool{false, true} {
			for _, mode := range []string{"", "auto", "hybrid", "semantic"} {
				args := map[string]any{"query": "refund", "filters": map[string]any{}}
				if mode != "" {
					args["mode"] = mode
				}
				output := callSearchDocumentReader(t, daemon, reader, embedded, args)
				require.Equal(t, "z-healthy", output["profile"])
				requested := mode
				if requested == "" {
					requested = "auto"
				}
				require.Equal(t, requested, output["requested_mode"])
			}
			for _, mode := range []string{"hybrid", "semantic"} {
				output := callSearchDocumentReader(t, daemon, reader, embedded, map[string]any{"query": "refund", "profile": "a-outage", "binding_id": "query", "mode": mode, "filters": map[string]any{}})
				if mode == "semantic" {
					require.Equal(t, "semantic_unavailable", output["code"])
					require.Contains(t, output["message"], failure)
				} else {
					require.Equal(t, "lexical", output["actual_mode"])
					require.Contains(t, output["skipped_reasons"], failure)
				}
			}
		}
	}
}

func TestSearchDocumentsValidatesBeforeSemanticGuidance(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund.txt", "Refunds take five days.")
	image := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputImage, false)
	export := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, false)
	provider, ok := export.EmbeddingProviders["query"].(questionOutageProvider)
	require.True(t, ok)
	provider.descriptor.Fingerprint = ""
	provider.descriptor.SupportsTextQuery = false
	descriptor, err := document.NewEmbeddingDescriptor(provider.descriptor)
	require.NoError(t, err)
	provider.descriptor = descriptor
	export.EmbeddingProviders["query"] = provider
	export.Profile.Embeddings[0].Descriptor = document.ProviderDescriptorV1{ID: descriptor.ID, Fingerprint: descriptor.Fingerprint}
	_, captions, err := processing.NewSuppliedMediaProfile(f.catalog, f.blobs, "test:operator")
	require.NoError(t, err)
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{"export": export, "images": image, "supplied-captions": captions}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := newSearchProcessingServer(t, f, service)
	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	for _, embedded := range []bool{false, true} {
		for _, tc := range []struct {
			profile, binding, mode, code string
			missingDirectory             bool
		}{
			{profile: "export", mode: "semantic", code: "semantic_unavailable"},
			{profile: "supplied-captions", binding: "missing", mode: "lexical", code: "not_found"},
			{profile: "images", binding: "missing", mode: "hybrid", code: "not_found"},
			{profile: "images", binding: "missing", mode: "semantic", code: "not_found"},
			{profile: "images", binding: "query", mode: "hybrid"},
			{profile: "images", binding: "query", mode: "semantic", code: "semantic_unavailable"},
			{profile: "supplied-captions", mode: "semantic", code: "not_found", missingDirectory: true},
		} {
			filters := map[string]any{}
			if tc.missingDirectory {
				filters["under_node_id"] = 999999
			}
			args := map[string]any{"query": "refund", "profile": tc.profile, "mode": tc.mode, "filters": filters}
			if tc.binding != "" {
				args["binding_id"] = tc.binding
			}
			output := callSearchDocumentReader(t, daemon, reader, embedded, args)
			if tc.code != "" {
				require.Equal(t, tc.code, output["code"])
				if tc.code == "semantic_unavailable" {
					require.Contains(t, output["message"], "docbank processing local")
				}
			} else {
				require.Equal(t, "lexical", output["actual_mode"])
				require.Contains(t, output["skipped_reasons"], "Profile \"images\" has no available search by meaning. Run docbank processing local to add it.")
			}
		}
	}
}
