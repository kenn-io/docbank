package embedding_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/embedding"
)

func TestCombinedRecipeBuildsDeterministicBoundedPlan(t *testing.T) {
	normalized := normalizedDocument(t)
	recipe, err := embedding.NewRecipe(embedding.RecipeConfig{
		Mode: embedding.RepresentationCombined, MaxInputRunes: 220,
		Distillation: &embedding.DistillationConfig{
			Provider: "synthetic", Model: "summarizer", ModelRevision: "revision-1",
			PromptTemplateVersion: 3, MaxPartitionRunes: 4_100, MaxSections: 20, MaxSectionRunes: 500,
		},
	})
	require.NoError(t, err)
	contextValue := embedding.DocumentContext{
		Filename: "  quarterly\tresults.pdf ",
		Title:    "Synthetic report\nfor testing",
	}

	request, err := embedding.PrepareDistillation(normalized, contextValue, recipe)
	require.NoError(t, err)
	requestJSON, err := json.Marshal(request)
	require.NoError(t, err)
	var restoredRequest embedding.DistillationRequest
	require.NoError(t, json.Unmarshal(requestJSON, &restoredRequest))
	assert.Equal(t, request, restoredRequest)
	request = restoredRequest
	require.NotEmpty(t, request.Partitions)
	assert.Equal(t, recipe.Fingerprint(), request.RecipeFingerprint)
	for index, partition := range request.Partitions {
		assert.Equal(t, index, partition.Ordinal)
		assert.LessOrEqual(t, utf8.RuneCountInString(partition.Text), 4_100)
		assert.NotEmpty(t, partition.SourceRefs)
	}

	sections := make([]embedding.DerivedSectionResult, 0, len(request.Partitions))
	for _, partition := range request.Partitions {
		sections = append(sections, embedding.DerivedSectionResult{
			Text: "\u202eConcise synthetic summary\r\nfor " + partition.Key[:8], PartitionKeys: []string{partition.Key},
		})
	}
	distillate, err := embedding.ValidateDistillate(request, embedding.DistillationResult{
		Provider: request.Provider, Model: request.Model, ModelRevision: request.ModelRevision, Sections: sections,
	})
	require.NoError(t, err)
	assert.NotContains(t, distillate.Sections[0].Text, "\u202e")
	assert.NotContains(t, distillate.Sections[0].Text, "\r")
	distillateJSON, err := json.Marshal(distillate)
	require.NoError(t, err)
	var restoredDistillate embedding.Distillate
	require.NoError(t, json.Unmarshal(distillateJSON, &restoredDistillate))
	assert.Equal(t, distillate, restoredDistillate)
	distillate = restoredDistillate

	plan, err := embedding.BuildEmbeddingPlan(normalized, contextValue, recipe, &distillate)
	require.NoError(t, err)
	require.Len(t, plan.Inputs, len(normalized.Chunks)+len(distillate.Sections))
	assert.Equal(t, distillate.Fingerprint, plan.DistillateFingerprint)
	assert.True(t, plan.Inputs[0].Truncated)
	for index, input := range plan.Inputs {
		assert.Equal(t, index, input.Ordinal)
		assert.LessOrEqual(t, utf8.RuneCountInString(input.Text), 220)
		assert.True(t, utf8.ValidString(input.Text))
		assert.NotEmpty(t, input.SourceRefs)
	}

	repeated, err := embedding.BuildEmbeddingPlan(normalized, contextValue, recipe, &distillate)
	require.NoError(t, err)
	assert.Equal(t, plan, repeated)
}

func TestDistillateRequiresCompleteOrderedCoverage(t *testing.T) {
	normalized := normalizedDocument(t)
	recipe, err := embedding.NewRecipe(embedding.RecipeConfig{
		Mode: embedding.RepresentationDistilled,
		Distillation: &embedding.DistillationConfig{
			Provider: "synthetic", Model: "summarizer", ModelRevision: "revision-1",
			PromptTemplateVersion: 1, MaxPartitionRunes: 4_100,
		},
	})
	require.NoError(t, err)
	request, err := embedding.PrepareDistillation(normalized, embedding.DocumentContext{}, recipe)
	require.NoError(t, err)
	require.Greater(t, len(request.Partitions), 1)

	_, err = embedding.ValidateDistillate(request, embedding.DistillationResult{
		Provider: request.Provider, Model: request.Model, ModelRevision: request.ModelRevision,
		Sections: []embedding.DerivedSectionResult{{
			Text: "Incomplete summary", PartitionKeys: []string{request.Partitions[1].Key},
		}},
	})
	assert.ErrorContains(t, err, "source order")
}

func TestRawRecipeNeedsNoDistillationAndRejectsIt(t *testing.T) {
	normalized := normalizedDocument(t)
	recipe, err := embedding.NewRecipe(embedding.RecipeConfig{})
	require.NoError(t, err)
	assert.Equal(t, embedding.RepresentationRaw, recipe.Values().Mode)

	_, err = embedding.PrepareDistillation(normalized, embedding.DocumentContext{}, recipe)
	require.ErrorContains(t, err, "does not configure distillation")
	plan, err := embedding.BuildEmbeddingPlan(normalized, embedding.DocumentContext{}, recipe, nil)
	require.NoError(t, err)
	assert.Len(t, plan.Inputs, len(normalized.Chunks))
}

func TestEgressFingerprintSeparatesPurposeAndDestination(t *testing.T) {
	base := embedding.EgressIdentity{
		Purpose: embedding.EgressDocumentEmbedding, Provider: "synthetic",
		Endpoint: "HTTPS://EXAMPLE.COM/v1/", Model: "embed-1", ModelRevision: "2026-08",
	}
	first, err := base.Fingerprint()
	require.NoError(t, err)
	canonical, err := (embedding.EgressIdentity{
		Purpose: embedding.EgressDocumentEmbedding, Provider: "synthetic",
		Endpoint: "https://example.com/v1", Model: "embed-1", ModelRevision: "2026-08",
	}).Fingerprint()
	require.NoError(t, err)
	assert.Equal(t, first, canonical)

	query := base
	query.Purpose = embedding.EgressQueryEmbedding
	queryFingerprint, err := query.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, first, queryFingerprint)

	redirected := base
	redirected.Endpoint = "https://other.example/v1"
	redirectedFingerprint, err := redirected.Fingerprint()
	require.NoError(t, err)
	assert.NotEqual(t, first, redirectedFingerprint)

	space := embedding.VectorSpaceIdentity{
		Provider: "synthetic", Model: "embed-1", ModelRevision: "2026-08",
		Dimension: 1_024, Normalization: "unit-length",
	}
	spaceFingerprint, err := space.Fingerprint()
	require.NoError(t, err)
	assert.NotEmpty(t, spaceFingerprint)
}

func normalizedDocument(t *testing.T) document.NormalizedDocument {
	t.Helper()
	policy, err := document.NewNormalizePolicy(50_000)
	require.NoError(t, err)
	longText := strings.Repeat("café résumé semantic evidence. ", 260)
	normalized, err := document.NormalizeDocument(document.SourceDocument{
		Family: "pdf", UnitKind: "page",
		Units: []document.SourceUnit{
			{Index: 0, Markdown: "# Revenue\n" + longText},
			{Index: 1, Markdown: "# Risks\n" + longText},
		},
	}, policy)
	require.NoError(t, err)
	return normalized
}
