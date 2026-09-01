package document

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisualPreviewV1CanonicalRoundTrip(t *testing.T) {
	recipe := testVisualPreviewRecipe()
	value := VisualPreviewV1{
		ContractVersion: VisualPreviewContractV1, SourceSHA256: visualPreviewTestHash("11"),
		Recipe: recipe, State: VisualPreviewReady,
		Output: &VisualPreviewOutputV1{BlobSHA256: visualPreviewTestHash("22"),
			Size: 123, MediaType: "image/jpeg", Width: 1600, Height: 900},
	}
	encoded, checksum, err := MarshalVisualPreviewV1(value)
	require.NoError(t, err)
	decoded, decodedChecksum, err := DecodeVisualPreviewV1(encoded)
	require.NoError(t, err)
	assert.Equal(t, value, decoded)
	assert.Equal(t, checksum, decodedChecksum)

	recipeBytes, fingerprint, err := MarshalVisualPreviewRecipeV1(recipe)
	require.NoError(t, err)
	decodedRecipe, decodedFingerprint, err := DecodeVisualPreviewRecipeV1(recipeBytes)
	require.NoError(t, err)
	assert.Equal(t, recipe, decodedRecipe)
	assert.Equal(t, fingerprint, decodedFingerprint)
}

func TestVisualPreviewV1RejectsInconsistentOutcomes(t *testing.T) {
	base := VisualPreviewV1{ContractVersion: VisualPreviewContractV1,
		SourceSHA256: visualPreviewTestHash("11"), Recipe: testVisualPreviewRecipe()}
	tests := []struct {
		name  string
		value VisualPreviewV1
	}{
		{name: "ready without output", value: func() VisualPreviewV1 {
			v := base
			v.State = VisualPreviewReady
			return v
		}()},
		{name: "failure with output", value: func() VisualPreviewV1 {
			v := base
			v.State = VisualPreviewFailed
			v.Output = &VisualPreviewOutputV1{BlobSHA256: visualPreviewTestHash("22"),
				Size: 1, MediaType: "image/jpeg", Width: 1, Height: 1}
			v.Failure = &VisualPreviewFailureV1{Code: "decode_failed", Detail: "bad bytes"}
			return v
		}()},
		{name: "oversized dimensions", value: func() VisualPreviewV1 {
			v := base
			v.State = VisualPreviewReady
			v.Output = &VisualPreviewOutputV1{BlobSHA256: visualPreviewTestHash("22"),
				Size: 1, MediaType: "image/jpeg", Width: v.Recipe.MaxEdgePixels + 1, Height: 1}
			return v
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := MarshalVisualPreviewV1(test.value)
			require.Error(t, err)
		})
	}
}

func testVisualPreviewRecipe() VisualPreviewRecipeV1 {
	return VisualPreviewRecipeV1{ContractVersion: VisualPreviewContractV1,
		MaxEdgePixels: 2048, OutputMediaType: "image/jpeg", OrientationPolicy: "apply",
		ColorPolicy: "srgb", FramePolicy: "primary",
		ProcessorFingerprint: visualPreviewTestHash("33")}
}

func visualPreviewTestHash(suffix string) string { return strings.Repeat("0", 64-len(suffix)) + suffix }
