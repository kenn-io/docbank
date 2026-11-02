package embedmodel_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedmodel"
	"go.kenn.io/kit/vector"
)

func TestTextAndPartsStayDistinct(t *testing.T) {
	text := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindText,
		Text: "alpha",
		Spans: []embedmodel.SourceSpan{{
			ByteStart: 0, ByteEnd: 5, RuneStart: 0, RuneEnd: 5, Label: "body",
		}},
	}
	got, err := text.EmbedText()
	require.NoError(t, err)
	assert.Equal(t, "alpha", got)

	parts := embedmodel.Content{
		Role: embedconfig.RoleQuery,
		Kind: embedmodel.KindText,
		Parts: []embedmodel.Part{
			{Kind: embedmodel.KindText, Text: "one"},
			{Kind: embedmodel.KindText, Text: "two"},
		},
	}
	got, err = parts.EmbedText()
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo", got)

	image := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindImage,
		Parts: []embedmodel.Part{{
			Kind: embedmodel.KindImage, MediaType: "image/png",
		}},
	}
	require.NoError(t, image.Validate())
	_, err = image.EmbedText()
	require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
}

func TestTopLevelImageWithTextIsNotEncoded(t *testing.T) {
	image := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindImage,
		Text: "caption",
	}
	require.NoError(t, image.Validate())
	_, err := image.EmbedText()
	require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)

	file := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindFile,
		Text: "caption",
	}
	require.NoError(t, file.Validate())
	_, err = file.EmbedText()
	require.ErrorIs(t, err, embedmodel.ErrUnsupportedContent)
}

func TestBlankTextMatchesTheEmbeddingRule(t *testing.T) {
	assert.True(t, embedmodel.BlankText(""))
	assert.True(t, embedmodel.BlankText(" \t\n\u200b\ufeff"))
	assert.False(t, embedmodel.BlankText("\u2800"))
	assert.False(t, embedmodel.BlankText("a"))
}

func TestDescriptorIdentitiesStaySeparate(t *testing.T) {
	document := sampleDescriptor()
	require.NoError(t, document.Validate())

	generation, err := document.Generation()
	require.NoError(t, err)
	assert.Equal(t, "bge-m3", generation.Model)
	assert.Equal(t, 1024, generation.Dimensions)
	space, err := document.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"vector_space": space}, generation.Params)
	assert.NotContains(t, generation.Params, "input_recipe")

	inputID, err := document.InputIdentity()
	require.NoError(t, err)
	wider := document
	wider.Input.OverlapTokens = 8
	widerID, err := wider.InputIdentity()
	require.NoError(t, err)
	assert.NotEqual(t, inputID, widerID)
	widerGeneration, err := wider.Generation()
	require.NoError(t, err)
	assert.Equal(t, generation.Fingerprint(), widerGeneration.Fingerprint())
	assert.Equal(t, generation.Params, widerGeneration.Params)
}

func TestDescriptorRejectsMetricsTheVectorPipelineCannotStore(t *testing.T) {
	for _, metric := range []embedconfig.Metric{embedconfig.MetricDotProduct, embedconfig.MetricL2} {
		descriptor := sampleDescriptor()
		descriptor.Model.Metric = metric
		err := descriptor.Validate()
		require.Error(t, err)
		require.ErrorContains(t, err, "cosine")
		_, err = descriptor.Generation()
		require.Error(t, err)
	}
}

func TestQueryCompatibilityIgnoresInputWindow(t *testing.T) {
	document := sampleDescriptor()
	query := document
	query.Input.MaxTokens = 64
	query.Input.OverlapTokens = 0
	query.Input.MaxSpans = 1
	require.NoError(t, embedmodel.Compatible(document, query))

	query.Model.Dimensions = 768
	err := embedmodel.Compatible(document, query)
	require.ErrorIs(t, err, embedmodel.ErrIncompatible)
}

func TestCoordinatesRequireSpansForText(t *testing.T) {
	descriptor := sampleDescriptor()
	descriptor.Coordinates = true
	missing := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindText,
		Text: "alpha",
	}
	require.Error(t, descriptor.ValidateContent(missing))

	missing.Spans = []embedmodel.SourceSpan{{ByteEnd: 5, RuneEnd: 5}}
	require.NoError(t, descriptor.ValidateContent(missing))

	image := embedmodel.Content{
		Role: embedconfig.RoleDocument,
		Kind: embedmodel.KindImage,
		Parts: []embedmodel.Part{{
			Kind: embedmodel.KindImage, MediaType: "image/png",
		}},
	}
	require.NoError(t, descriptor.ValidateContent(image))
}

func TestFormatUsesLiteralAffixes(t *testing.T) {
	roles := embedconfig.Roles{DocumentPrefix: "doc: ", QuerySuffix: " ?"}
	got, err := embedmodel.Format(embedconfig.RoleDocument, "alpha", roles)
	require.NoError(t, err)
	assert.Equal(t, "doc: alpha", got)
	got, err = embedmodel.Format(embedconfig.RoleQuery, "alpha", roles)
	require.NoError(t, err)
	assert.Equal(t, "alpha ?", got)
}

func sampleDescriptor() embedmodel.Descriptor {
	return embedmodel.Descriptor{
		Model: embedconfig.Model{
			Name:          "bge-m3",
			Revision:      "weights-epoch",
			Dimensions:    1024,
			Metric:        embedconfig.MetricCosine,
			Normalization: embedconfig.NormalizationL2,
			Pooling:       "cls",
		},
		Roles: embedconfig.Roles{
			DocumentPrefix: "search_document: ",
			QueryPrefix:    "search_query: ",
		},
		Deployment: embedconfig.Deployment{BaseURL: "http://127.0.0.1:11434/v1"},
		Input: embedconfig.InputLimits{
			Recipe:            "v2",
			Tokenizer:         "bge-m3",
			TokenizerRevision: "rev",
			ContentID:         "body",
			MaxTokens:         512,
			OverlapTokens:     64,
			MaxSpans:          50,
			Truncation:        embedconfig.TruncationDropTail,
		},
	}
}

func TestVectorIdentityPinsTheEndpointOnlyOnRequest(t *testing.T) {
	document := sampleDescriptor()
	id, err := document.VectorIdentity()
	require.NoError(t, err)
	moved := document
	moved.Deployment.BaseURL = "http://127.0.0.1:11435/v1"
	movedID, err := moved.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, id, movedID, "an unpinned endpoint move keeps the vector space")

	document.Deployment.PinEndpoint = true
	pinned, err := document.VectorIdentity()
	require.NoError(t, err)
	moved.Deployment.PinEndpoint = true
	movedPinned, err := moved.VectorIdentity()
	require.NoError(t, err)
	assert.NotEqual(t, pinned, movedPinned)
}

func TestIdentitiesIgnoreWireAndTrustSettings(t *testing.T) {
	document := sampleDescriptor()
	vectorID, err := document.VectorIdentity()
	require.NoError(t, err)
	inputID, err := document.InputIdentity()
	require.NoError(t, err)

	changed := document
	changed.Model.EncodingFormat = "base64"
	changed.Deployment.TrustPrivateNetwork = true
	gotVector, err := changed.VectorIdentity()
	require.NoError(t, err)
	gotInput, err := changed.InputIdentity()
	require.NoError(t, err)
	assert.Equal(t, vectorID, gotVector, "wire encoding is not part of the vector identity")
	assert.Equal(t, inputID, gotInput)
}

func TestInputIdentityTreatsZeroMaxSpansAsASetting(t *testing.T) {
	open := sampleDescriptor()
	open.Input.MaxSpans = 0
	capped := open
	capped.Input.MaxSpans = 1
	openID, err := open.InputIdentity()
	require.NoError(t, err)
	cappedID, err := capped.InputIdentity()
	require.NoError(t, err)
	assert.NotEqual(t, openID, cappedID, "a zero span cap is a real unlimited setting, not an omitted field")
}

func TestValidateTrimsLikeTheIdentities(t *testing.T) {
	document := sampleDescriptor()
	padded := document
	padded.Model.Metric = " cosine "
	require.NoError(t, padded.Validate())
	want, err := document.VectorIdentity()
	require.NoError(t, err)
	got, err := padded.VectorIdentity()
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestContentWithoutKindIsText(t *testing.T) {
	content := embedmodel.Content{Role: embedconfig.RoleDocument, Text: "plain"}
	require.NoError(t, content.Validate())
	text, err := content.EmbedText()
	require.NoError(t, err)
	assert.Equal(t, "plain", text)
}

func TestMatchesKeepsLegacyGenerationsAndUsesKitFormForNewOnes(t *testing.T) {
	assert := assert.New(t)
	// The fingerprint a consumer's existing generation was stored under,
	// in the consumer's own format.
	legacy := vector.Generation{
		Model: "bge-m3", Dimensions: 1024,
		Params: map[string]string{"max_input_chars": "2000", "doc_unit_scheme": "run_v1"},
	}.Fingerprint()
	plain := sampleDescriptor()
	adopting := plain
	adopting.Legacy = []string{legacy, "bge-m3:1024:v1:c2000"}

	current, err := adopting.Generation()
	require.NoError(t, err)
	kitForm, err := plain.Generation()
	require.NoError(t, err)
	assert.Equal(kitForm.Fingerprint(), current.Fingerprint(), "new generations use kit's form")

	for _, fingerprint := range []string{legacy, "bge-m3:1024:v1:c2000", current.Fingerprint()} {
		ok, err := adopting.Matches(fingerprint)
		require.NoError(t, err)
		assert.True(ok, "fingerprint %q stays current", fingerprint)
	}
	ok, err := plain.Matches(legacy)
	require.NoError(t, err)
	assert.False(ok, "without Legacy an old fingerprint is a different generation")

	changed := adopting
	changed.Model.Name = "other-model"
	ok, err = changed.Matches(current.Fingerprint())
	require.NoError(t, err)
	assert.False(ok, "a model change is a new vector space")
}

func TestValidateRejectsAnEmptyLegacyFingerprint(t *testing.T) {
	d := sampleDescriptor()
	d.Legacy = []string{"  "}
	require.ErrorContains(t, d.Validate(), "legacy fingerprint 0 is empty")
}
