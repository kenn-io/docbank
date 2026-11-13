package redactionfiles

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
	"go.kenn.io/docbank/internal/processing"
)

func TestReadInputsAndReview(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	preparation := filepath.Join(dir, "preparation.json")
	data, err := os.ReadFile("../../cmd/docbank/testdata/redaction-preparation.json")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(preparation, data, 0600))
	var prep processing.RedactionPreparation
	require.NoError(t, json.Unmarshal(data, &prep))
	masks := []processing.RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{End: 6}}}}
	encoded, err := json.Marshal(masks)
	require.NoError(t, err)
	maskPath := filepath.Join(dir, "masks.json")
	require.NoError(t, os.WriteFile(maskPath, encoded, 0600))
	inputs, err := ReadInputs(t.Context(), preparation, maskPath)
	require.NoError(t, err)
	require.Equal(t, "SECRET KEEP SECRET", inputs.Preparation.Map.Text)
	review, err := processing.BuildRedactionReview(prep, masks, prep.Selection.Revision, pdfproduction.QualifiedRecipe())
	require.NoError(t, err)
	encoded, err = json.Marshal(review)
	require.NoError(t, err)
	reviewPath := filepath.Join(dir, "review.json")
	require.NoError(t, os.WriteFile(reviewPath, encoded, 0600))
	got, err := ReadReview(t.Context(), reviewPath, inputs)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED]", got.Resolved.Runs[0].Text)
	// Otherwise valid inputs isolate strict decoding from later identity checks.
	for _, extra := range []string{`"unknown":"synthetic private value",`, fmt.Sprintf(`"contract":%q,`, prep.Contract)} {
		changed := append([]byte("{"+extra), data[1:]...)
		require.NoError(t, os.WriteFile(preparation, changed, 0600))
		_, err := ReadInputs(t.Context(), preparation, maskPath)
		require.ErrorIs(t, err, ErrInvalidInput)
	}
	inputs.Masks[0].Selector.Span.End = 7
	_, err = ReadReview(t.Context(), reviewPath, inputs)
	require.ErrorIs(t, err, processing.ErrRedactionReviewChanged)
}

func TestReadPrivateInputBounds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "input.json")
	require.NoError(t, os.WriteFile(file, []byte("12345"), 0600))
	_, err := ReadFile(t.Context(), file, 4)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = ReadFile(t.Context(), dir, 16)
	require.ErrorIs(t, err, ErrInvalidInput)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ReadFile(ctx, file, 16)
	require.ErrorIs(t, err, context.Canceled)
}
