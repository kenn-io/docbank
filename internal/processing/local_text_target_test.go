package processing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLocalTextTargetReviewsChoiceAndRetainedReasons(t *testing.T) {
	_, original := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	saved := "previous"
	setup := newLocalTextMoveSetup(t, original.Service, &saved)
	reason := "contextual text keeps its provider"
	setup.CandidateTarget = func(target LocalTextTarget, name string) (*Service, []string, error) {
		service, err := setup.Candidate(target.Server, name)
		return service, []string{reason}, err
	}
	var savedTarget LocalTextTarget
	setup.SaveTarget = func(names []string, target LocalTextTarget) error {
		name := names[0]
		saved, savedTarget = name, target
		return nil
	}
	target := LocalTextTarget{Server: "auto", EmbeddingServer: "https://embedding.example.invalid/api/v1/servers/synthetic", EmbeddingModel: "synthetic-model", EmbeddingDimensions: 3}
	plan, err := setup.PlanTarget(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, []string{reason}, plan.Retained)
	require.Equal(t, "pdf", plan.Profiles[0].Name)
	require.Contains(t, plan.VectorEffect, "documents before setup returns")
	changed := target
	changed.EmbeddingDimensions++
	_, err = setup.ApplyTarget(t.Context(), changed, plan.Fingerprint)
	require.ErrorIs(t, err, ErrPlanChanged)
	require.Equal(t, "previous", saved)
	reason = "a different retained reason"
	_, err = setup.ApplyTarget(t.Context(), target, plan.Fingerprint)
	require.ErrorIs(t, err, ErrPlanChanged)
	require.Equal(t, "previous", saved)
	reason = plan.Retained[0]
	_, err = setup.ApplyTarget(t.Context(), target, plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, "pdf", saved)
	require.Equal(t, target, savedTarget)
}

func TestLocalTextTargetReviewsEveryProfile(t *testing.T) {
	provider := &localTextEmbeddingProvider{descriptor: embeddingWorkerDescriptor(t)}
	fixture, original := newLocalTextBackfillFixture(t, newWorkerProvider(t), provider)
	source := original.Service.profiles["pdf"]
	profile := func(name string) ProfileConfig {
		portable := source.portable
		portable.Rendition = new(*portable.Rendition)
		portable.Rendition.AuthorizationFingerprint = workerHash(name)
		return ProfileConfig{Profile: portable, RenditionProvider: source.provider,
			EmbeddingProviders: source.embedders, Tokenizers: source.tokenizers}
	}
	service := func(profiles map[string]ProfileConfig) *Service {
		s, err := NewService(t.Context(), ServiceConfig{Catalog: fixture.catalog, Blobs: fixture.blobs,
			Gate: newWorkerTestGate(), Profiles: profiles, SpoolDirectory: t.TempDir()})
		require.NoError(t, err)
		t.Cleanup(s.Stop)
		return s
	}
	setup := &LocalTextSetup{Service: service(map[string]ProfileConfig{"alpha": profile("alpha"), "beta": profile("beta")})}
	setup.CandidateTarget = func(_ LocalTextTarget, name string) (*Service, []string, error) {
		return service(map[string]ProfileConfig{name: profile(name + "-server")}), nil, nil
	}
	target := LocalTextTarget{EmbeddingServer: "https://embedding.example.invalid/api/server", EmbeddingModel: "synthetic-model", EmbeddingDimensions: 3}
	plan, err := setup.PlanTarget(t.Context(), target)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, []string{plan.Profiles[0].Name, plan.Profiles[1].Name})
	var saved []string
	setup.SaveTarget = func(names []string, _ LocalTextTarget) error { saved = names; return nil }
	_, err = setup.ApplyTarget(t.Context(), target, plan.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, []string{"alpha", "beta"}, saved)
}

func TestLocalTextPairedBindingsFinishBackfill(t *testing.T) {
	_, setup := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	items, err := setup.pendingTargets(t.Context(), "", 100, false)
	require.NoError(t, err)
	require.NotEmpty(t, items)
	for _, item := range items {
		require.NoError(t, setup.process(t.Context(), item))
	}
	profile := setup.Service.profiles["pdf"]
	kit := profile.portable.Embeddings[0]
	kit.Name = "chunk-kit"
	profile.portable.Embeddings = append(profile.portable.Embeddings, kit)
	profile.mediaFallbackBindings = map[string]string{"chunk-kit": "chunk"}
	setup.Service.profiles["pdf"] = profile
	items, err = setup.pendingTargets(t.Context(), "", 100, false)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestLocalTextTargetReportsRetainedOnly(t *testing.T) {
	_, original := newLocalTextBackfillFixture(t, newWorkerProvider(t))
	setup := &LocalTextSetup{Service: original.Service}
	setup.CandidateTarget = func(_ LocalTextTarget, name string) (*Service, []string, error) {
		if name == "local-text" {
			t.Fatal("retained profile must not be replaced by a fallback")
		}
		return nil, []string{"contextual text stays with its provider"}, ErrProfileNotConfigured
	}
	_, err := setup.PlanTarget(t.Context(), LocalTextTarget{EmbeddingServer: "https://embedding.example.invalid/v1", EmbeddingModel: "model", EmbeddingDimensions: 2})
	require.ErrorContains(t, err, "contextual text stays with its provider")
}
