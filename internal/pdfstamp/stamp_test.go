package pdfstamp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validRecipe(t *testing.T) Recipe {
	t.Helper()
	return Recipe{
		Contract: RecipeContractV1, NamespaceID: "ns-synthetic", Prefix: "OUR",
		Padding: 6, StartAt: 41, Position: "bottom-right", MarginPoints: 24,
		FontName: "Helvetica", FontSizePoints: 9, Color: "#000000", Opacity: 1,
		Units: "point", RotationPolicy: "follow_page",
		EngineIdentity: EngineIdentity{
			Name: "pdfcpu", Version: "v0.15.0", API: "AddWatermarksMap",
			Options: []string{"onTop=true", "update=false"},
		},
	}
}

func TestRecipeSHA256UsesTheCanonicalNormalizedRecipe(t *testing.T) {
	recipe := validRecipe(t)
	recipe.Position = ""

	digest, err := recipe.SHA256()

	require.NoError(t, err)
	assert.Equal(t, "9513955cec69689641c0f9be49e36b0417b92519c03d01b48daf3abd93e7995b", digest)
}

func TestRecipeRejectsUnsupportedStampBehavior(t *testing.T) {
	tests := map[string]func(*Recipe){
		"contract":         func(r *Recipe) { r.Contract = "bates-stamp/v2" },
		"namespace":        func(r *Recipe) { r.NamespaceID = "" },
		"padding":          func(r *Recipe) { r.Padding = 11 },
		"start":            func(r *Recipe) { r.StartAt = 0 },
		"position":         func(r *Recipe) { r.Position = "near-the-bottom" },
		"margin":           func(r *Recipe) { r.MarginPoints = 145 },
		"font":             func(r *Recipe) { r.FontName = "SyntheticSans" },
		"font size":        func(r *Recipe) { r.FontSizePoints = 0 },
		"color":            func(r *Recipe) { r.Color = "black" },
		"opacity":          func(r *Recipe) { r.Opacity = .5 },
		"units":            func(r *Recipe) { r.Units = "inch" },
		"rotation":         func(r *Recipe) { r.RotationPolicy = "fixed" },
		"engine version":   func(r *Recipe) { r.EngineIdentity.Version = "v0.14.0" },
		"engine options":   func(r *Recipe) { r.EngineIdentity.Options = []string{"update=true"} },
		"control in label": func(r *Recipe) { r.Prefix = "OUR\n" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			recipe := validRecipe(t)
			mutate(&recipe)
			require.Error(t, recipe.Validate())
		})
	}
}

func TestStampWritesEveryLabelAndVerifiesTheOutput(t *testing.T) {
	recipe := validRecipe(t)
	source := syntheticPDF(t, 2, "Letter")
	sourceDigest := sha256Hex(source)
	var output bytes.Buffer

	result, err := Stamp(t.Context(), bytes.NewReader(source), []PageLabel{
		{SourcePage: 1, Label: "OUR000041"},
		{SourcePage: 2, Label: "OUR000042"},
	}, recipe, &output)

	require.NoError(t, err)
	assert.Equal(t, 2, result.PageCount)
	assert.Equal(t, sourceDigest, sha256Hex(source), "the source must remain byte-identical")
	assert.NotEqual(t, sourceDigest, result.SHA256)
	assert.Equal(t, int64(output.Len()), result.Size)
	assert.Equal(t, []PageLabel{{SourcePage: 1, Label: "OUR000041"}, {SourcePage: 2, Label: "OUR000042"}}, result.Pages)
}

func TestStampOutputIsIndependentlyVisible(t *testing.T) {
	requirePopplerQualification(t)
	var output bytes.Buffer

	_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 2, "Letter")), []PageLabel{
		{SourcePage: 1, Label: "OUR000041"},
		{SourcePage: 2, Label: "OUR000042"},
	}, validRecipe(t), &output)

	require.NoError(t, err)
	assert.Equal(t, []string{"OUR000041", "OUR000042"}, readVisibleLabels(t, output.Bytes()))
	assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
}

func TestStampSupportsEveryRecipePosition(t *testing.T) {
	requirePopplerQualification(t)
	positions := []string{
		"top-left", "top-center", "top-right",
		"middle-left", "middle-center", "middle-right",
		"bottom-left", "bottom-center", "bottom-right",
	}
	for _, position := range positions {
		t.Run(position, func(t *testing.T) {
			recipe := validRecipe(t)
			recipe.Position = position
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 1, "Letter")),
				[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)
			require.NoError(t, err)
			assert.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
			assertRenderedStampAt(t, output.Bytes(), position, recipe.MarginPoints)
		})
	}
}

func TestStampRejectsIncompleteReorderedOrSubstitutedLabels(t *testing.T) {
	source := syntheticPDF(t, 2, "Letter")
	tests := map[string][]PageLabel{
		"incomplete":  {{SourcePage: 2, Label: "OUR000042"}},
		"reordered":   {{SourcePage: 2, Label: "OUR000042"}, {SourcePage: 1, Label: "OUR000041"}},
		"substituted": {{SourcePage: 1, Label: "OUR000041"}, {SourcePage: 2, Label: "OUR000099"}},
	}
	for name, labels := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(source), labels, validRecipe(t), &output)
			require.ErrorIs(t, err, ErrStampEngineFailure)
			assert.Zero(t, output.Len())
		})
	}
}

func TestStampRejectsASequenceThatOverflowsItsPadding(t *testing.T) {
	recipe := validRecipe(t)
	recipe.StartAt = 999999
	var output bytes.Buffer

	_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 2, "Letter")), []PageLabel{
		{SourcePage: 1, Label: "OUR999999"},
		{SourcePage: 2, Label: "OUR1000000"},
	}, recipe, &output)

	require.ErrorIs(t, err, ErrStampEngineFailure)
	assert.Zero(t, output.Len())
}

func TestStampRejectsClippingMalformedAndRestampByDefault(t *testing.T) {
	tests := map[string][]byte{
		"clipping":        syntheticTinyPage(t),
		"narrow crop":     syntheticNarrowCropped(t),
		"malformed":       []byte("not a pdf"),
		"already stamped": syntheticAlreadyStamped(t),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(source),
				[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
			require.ErrorIs(t, err, ErrStampEngineFailure)
			assert.Zero(t, output.Len())
		})
	}
}

func TestStampAllowsExplicitRestamp(t *testing.T) {
	recipe := validRecipe(t)
	recipe.Restamp = true
	var output bytes.Buffer

	result, err := Stamp(t.Context(), bytes.NewReader(syntheticAlreadyStamped(t)),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)

	require.NoError(t, err)
	assert.Equal(t, []PageLabel{{SourcePage: 1, Label: "OUR000041"}}, result.Pages)
	assert.NotEmpty(t, output.Bytes())
}

func TestVerifyStampedLabelsRejectsASubstitutedOutputLabel(t *testing.T) {
	source := syntheticPDF(t, 1, "Letter")
	stamped, err := stampPages(source, []string{"OUR000099"}, "Helvetica")
	require.NoError(t, err)

	err = verifyStampedLabels(stamped, []PageLabel{{SourcePage: 1, Label: "OUR000041"}})

	require.ErrorIs(t, err, ErrStampEngineFailure)
}

func TestStampHonorsCanceledContextBeforeReading(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var output bytes.Buffer

	_, err := Stamp(ctx, bytes.NewReader(syntheticPDF(t, 1, "Letter")),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)

	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, output.Len())
}

func TestLimitedStampWriterPublishesNoOversizedChunk(t *testing.T) {
	var destination bytes.Buffer
	writer := &limitedStampWriter{Writer: &destination, Remaining: 3}

	written, err := writer.Write([]byte("four"))

	require.ErrorIs(t, err, ErrStampEngineFailure)
	assert.Zero(t, written)
	assert.Zero(t, destination.Len())
}

func TestLimitedStampWriterReportsShortWrites(t *testing.T) {
	writer := &limitedStampWriter{Writer: shortWriter{}, Remaining: 4}

	written, err := writer.Write([]byte("four"))

	assert.Equal(t, 2, written)
	require.ErrorIs(t, err, io.ErrShortWrite)
}

type shortWriter struct{}

func (shortWriter) Write(value []byte) (int, error) {
	return len(value) / 2, nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func TestStampRejectsNilDestination(t *testing.T) {
	_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 1, "Letter")),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), nil)
	require.ErrorIs(t, err, ErrStampEngineFailure)
}
