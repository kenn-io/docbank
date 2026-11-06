package mcp

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/processing"
)

func TestSearchDocumentsModeGuidance(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund.txt", "Refunds take five days.")
	daemon, _, profile := newProcessingDocumentServer(t, f)
	output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "filters": map[string]any{}})
	require.Equal(t, profile, output["profile"])
	require.Equal(t, "auto", output["requested_mode"])
	require.Equal(t, "lexical", output["actual_mode"])
	require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing local")
	hits, ok := output["results"].([]any)
	require.True(t, ok)
	require.Len(t, hits, 1)
	require.Equal(t, "/refund.txt", schemaMap(t, hits[0])["path"])
}

func TestSearchDocumentsMixedProfileImageBinding(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund.txt", "Refunds take five days.")
	mixed := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputImage, false)
	text := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, false)
	image := mixed.Profile.Embeddings[0]
	image.Name = "image"
	mixed.Profile.Embeddings = append([]document.EmbeddingBindingV1{image}, text.Profile.Embeddings...)
	mixed.EmbeddingProviders = map[string]document.EmbeddingProvider{"image": mixed.EmbeddingProviders["query"], "query": text.EmbeddingProviders["query"]}
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{"mixed": mixed}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := newSearchProcessingServer(t, f, service)
	for _, tc := range []struct{ mode, binding, code string }{
		{mode: "hybrid", binding: "image"},
		{mode: "auto", binding: "image"},
		{mode: "semantic", binding: "image", code: "semantic_unavailable"},
	} {
		t.Run(tc.mode+"/"+tc.binding, func(t *testing.T) {
			output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "profile": "mixed", "binding_id": tc.binding, "mode": tc.mode, "filters": map[string]any{}})
			if tc.code != "" {
				require.Equal(t, tc.code, output["code"])
				if tc.code == "semantic_unavailable" {
					require.Contains(t, output["message"], `Binding "image" cannot search document text by meaning.`)
					require.Contains(t, output["message"], `Try binding "query".`)
				}
				return
			}
			require.Equal(t, "mixed", output["profile"])
			require.Equal(t, tc.mode, output["requested_mode"])
			require.Equal(t, "lexical", output["actual_mode"])
			if tc.mode == "auto" {
				require.Empty(t, output["skipped_reasons"])
			} else {
				require.Contains(t, output["skipped_reasons"], "Binding \"image\" cannot search document text by meaning. Returned keyword results; try binding \"query\".")
			}
			hits, ok := output["results"].([]any)
			require.True(t, ok)
			require.Len(t, hits, 1)
			require.Equal(t, "/refund.txt", schemaMap(t, hits[0])["path"])
		})
	}
}

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
	output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "filters": map[string]any{}})
	require.Equal(t, "z-healthy", output["profile"])
	for _, mode := range []string{"hybrid", "semantic"} {
		output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "profile": "a-outage", "binding_id": "query", "mode": mode, "filters": map[string]any{}})
		if mode == "semantic" {
			require.Equal(t, "semantic_unavailable", output["code"])
			require.Contains(t, output["message"], "index_unavailable")
			require.NotContains(t, output["message"], "docbank processing local")
			require.NotContains(t, output["message"], "has no available")
		} else {
			require.Equal(t, "lexical", output["actual_mode"])
			require.Contains(t, output["skipped_reasons"], "index_unavailable")
			require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "needs building")
			require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing plan <path-or-id> --profile <profile>")
			require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing build <path-or-id> --profile <profile> --plan-fingerprint <reviewed-sha256> --consent")
		}
	}
}

type searchQueryCounter struct {
	document.EmbeddingProvider
	queries atomic.Int64
}

func (p *searchQueryCounter) Embed(ctx context.Context, inputs []document.EmbeddingInput, auth document.EmbeddingAuthorization) (document.EmbeddingResult, error) {
	if inputs[0].Role == document.EmbeddingRoleQuery {
		p.queries.Add(1)
	}
	return p.EmbeddingProvider.Embed(ctx, inputs, auth)
}

func TestSearchDocumentsRequiresQueryConsent(t *testing.T) {
	f := newNativeExportFixture(t)
	f.addFile(t, "refund.txt", "Refunds take five days.")
	profile := newSearchTestEmbeddingProfile(t, f, document.EmbeddingInputOriginalFile, false)
	provider := &searchQueryCounter{EmbeddingProvider: profile.EmbeddingProviders["query"]}
	profile.EmbeddingProviders["query"] = provider
	service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: f.catalog, Blobs: f.blobs, Gate: api.NewOperationGate(), SpoolDirectory: t.TempDir(), Profiles: map[string]processing.ProfileConfig{"local-text": profile}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Shutdown(context.WithoutCancel(t.Context()))) })
	daemon := newSearchProcessingServer(t, f, service)
	output := exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "filters": map[string]any{}})
	require.Equal(t, "lexical", output["actual_mode"])
	require.Contains(t, output["skipped_reasons"], "processing_consent_required")
	require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "prior operator consent")
	require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing plan <path-or-id> --profile <profile>")
	require.Contains(t, fmt.Sprint(output["skipped_reasons"]), "docbank processing build <path-or-id> --profile <profile> --plan-fingerprint <reviewed-sha256> --consent")
	hits, ok := output["results"].([]any)
	require.True(t, ok)
	require.Len(t, hits, 1)
	require.Equal(t, "/refund.txt", schemaMap(t, hits[0])["path"])
	output = exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "mode": "semantic", "filters": map[string]any{}})
	require.Equal(t, "consent_required", output["code"])
	require.Contains(t, output["message"], "prior operator consent")
	require.Zero(t, provider.queries.Load())

	reader := api.NewDocumentReader(f.catalog, f.blobs, os.Getenv("DOCBANK_HOME"), service)
	page, err := reader.ListDocuments(t.Context(), api.DocumentQuery{})
	require.NoError(t, err)
	selector := processing.Selector{NodeID: page.Items[0].NodeID, ContentVersionID: page.Items[0].ContentVersionID, Profile: "local-text"}
	plan, err := service.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = service.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: plan.Fingerprint, Consent: true})
	require.NoError(t, err)
	output = exportCall(t, daemon, "search_documents", map[string]any{"query": "refund", "filters": map[string]any{}})
	require.Equal(t, "hybrid", output["actual_mode"])
	require.Positive(t, provider.queries.Load())
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
		output := exportCall(t, daemon, "search_documents", args)
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
