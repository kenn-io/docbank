package main

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/blob"
	"go.kenn.io/docbank/internal/config"
	"go.kenn.io/docbank/internal/home"
	"go.kenn.io/docbank/internal/processing"
	"go.kenn.io/docbank/internal/store"
)

func TestLocalTextBindingCollisionKeepsConsent(t *testing.T) {
	t.Setenv("DOCBANK_TEST_COLLISION_KEY", "synthetic-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if !assert.NoError(t, json.UnmarshalRead(r.Body, &request)) {
			return
		}
		data := make([]map[string]any, len(request.Input))
		for i := range request.Input {
			vector := make([]float32, 768)
			vector[0] = 1
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.MarshalWrite(w, map[string]any{"object": "list", "model": "nomic-embed-text", "data": data}))
	}))
	t.Cleanup(server.Close)
	cfg := localTextSourceConfig(t, server.URL, "nomic-embed-text-v1.5", "prefix-free", "DOCBANK_TEST_COLLISION_KEY")
	cfg.EmbeddingProfiles[config.LocalTextProfile] = cfg.EmbeddingProfiles["semantic"]
	delete(cfg.EmbeddingProfiles, "semantic")
	recipe := cfg.ProcessingProfiles["archive"]
	recipe.Embeddings = []string{config.LocalTextProfile}
	recipe.MaxSegmentRunes = 2048
	cfg.ProcessingProfiles["archive"] = recipe
	layout := home.Layout{Root: t.TempDir()}
	require.NoError(t, layout.Ensure())
	catalog, err := store.Open(t.Context(), layout.DBPath())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	blobs, err := blob.New(store.NewPackCatalog(catalog), layout.BlobsDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, blobs.Close()) })
	receipt, err := blobs.WriteDetailedContext(t.Context(), strings.NewReader("synthetic document"))
	require.NoError(t, err)
	encoding, err := receipt.EncodingName()
	require.NoError(t, err)
	node, err := catalog.CreateFile(t.Context(), catalog.RootID(), "sample.txt", receipt.Hash, receipt.Size, "text/plain", store.BlobPhysical{Encoding: encoding, StoredBytes: receipt.StoredSize})
	require.NoError(t, err)
	serviceFor := func(candidate config.Config) *processing.Service {
		bundle, err := configureEmbeddingRuntimeBundle(candidate)
		require.NoError(t, err)
		profiles, err := executableProcessingProfiles(candidate, bundle)
		require.NoError(t, err)
		service, err := processing.NewService(t.Context(), processing.ServiceConfig{Catalog: catalog, Blobs: blobs, Gate: api.NewOperationGate(), Profiles: profiles, Principal: "daemon:operator", Scope: "document-processing", SpoolDirectory: layout.BlobTmpDir()})
		require.NoError(t, err)
		t.Cleanup(service.Stop)
		return service
	}
	original := serviceFor(cfg)
	selector := processing.Selector{Profile: "archive", NodeID: node.ID, ContentVersionID: node.CurrentVersionID}
	originalPlan, err := original.Plan(t.Context(), selector)
	require.NoError(t, err)
	_, err = original.Start(t.Context(), processing.StartRequest{Selector: selector, PlanFingerprint: originalPlan.Fingerprint, Consent: true})
	require.NoError(t, err)
	setup := &processing.LocalTextSetup{Service: original, Selected: config.LocalTextProfile,
		Candidate: func(mode, name string) (*processing.Service, error) {
			candidate, err := localTextCandidate(cfg, mode, name)
			if err != nil {
				return nil, err
			}
			return serviceFor(candidate), nil
		},
		Startup: func(context.Context, bool, string) error { return nil },
		Save: func(name, mode string) error {
			return saveLocalText(layout.Root, config.LocalTextConfig{Profile: name, Server: mode})
		},
	}
	preview, err := setup.Plan(t.Context(), "local")
	require.NoError(t, err)
	require.Equal(t, "archive", preview.Profile)
	_, err = setup.Apply(t.Context(), "local", preview.Fingerprint)
	require.NoError(t, err)
	saved, err := readLocalText(layout.Root)
	require.NoError(t, err)
	require.Equal(t, preview.Profile, saved.Profile)
	name := saved.Profile
	restored, err := localTextCandidate(cfg, saved.Server, name)
	require.NoError(t, err)
	profile, err := restored.ProcessingProfile(name)
	require.NoError(t, err)
	_, fingerprints, err := document.CanonicalProfile(profile.Document)
	require.NoError(t, err)
	assert.Equal(t, preview.ProfileFingerprint, fingerprints.Profile, "%s approved and restored profile", name)
	plan, err := serviceFor(restored).Plan(t.Context(), processing.Selector{Profile: name, NodeID: node.ID, ContentVersionID: node.CurrentVersionID})
	require.NoError(t, err)
	assert.False(t, plan.ConsentRequired, "%s restored consent state: %s", name, plan.ConsentState)
}
