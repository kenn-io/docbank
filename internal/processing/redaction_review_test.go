package processing

import (
	"encoding/json/v2"
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/redactiontest"
)

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionReviewIdentity(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "SECRET KEEP SECRET")
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	masks := []RedactionMask{{ID: uuid.New(), Selector: redaction.Selector{
		Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{Start: 0, End: 6}}}}
	first, err := BuildRedactionReview(prep, masks, 1)
	require.NoError(t, err)
	text, err := redaction.Text(first.Resolved)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP SECRET\f", string(text))
	prep.Selection.Revision += 7
	second, err := BuildRedactionReview(prep, masks, 8)
	require.NoError(t, err)
	require.Equal(t, first.SHA256, second.SHA256)
	require.Equal(t, first.PreparationSHA256, second.PreparationSHA256)
	require.NoError(t, ValidateRedactionReview(prep, masks, first))
	for _, change := range []func(*RedactionReview){
		func(r *RedactionReview) { r.Selection.NodeID++ },
		func(r *RedactionReview) { r.Recipe.PaddingPixels++ },
		func(r *RedactionReview) { r.Resolved.Runs[0].Text = "not redacted" },
	} {
		encoded, err := json.Marshal(first)
		require.NoError(t, err)
		var altered RedactionReview
		require.NoError(t, json.Unmarshal(encoded, &altered))
		change(&altered)
		altered.SHA256, err = redactionReviewDigest(altered)
		require.NoError(t, err)
		require.Error(t, ValidateRedactionReview(prep, masks, altered))
	}
	masks[0].ID = uuid.New()
	require.Error(t, ValidateRedactionReview(prep, masks, first))
}

func TestRedactionMaskDecode(t *testing.T) {
	t.Parallel()
	valid := `[{"id":"12345678-1234-4234-8234-123456789abc","selector":{"kind":"text","map_sha256":"` + strings.Repeat("a", 64) + `","span":{"start":0,"end":6}}}]`
	masks, err := DecodeRedactionMasks([]byte(valid))
	require.NoError(t, err)
	require.Len(t, masks, 1)
	for _, raw := range []string{
		strings.Replace(valid, "123456789abc", "123456789ABC", 1),
		strings.Replace(valid, "12345678-1234-4234-8234-123456789abc", "urn:uuid:12345678-1234-4234-8234-123456789abc", 1),
		strings.Replace(valid, `"span":`, `"pages":[],"span":`, 1),
		strings.Replace(valid, `"kind":"text"`, `"kind":"unit"`, 1),
		strings.Replace(valid, `"selector":`, `"extra":true,"selector":`, 1),
		`[]`, `null`,
	} {
		_, err := DecodeRedactionMasks([]byte(raw))
		require.Error(t, err, raw)
	}
	masks = append(masks, masks[0])
	require.Error(t, ValidateRedactionMasks(masks))
}

//nolint:paralleltest // Qualified PDFium accounts for process-wide RSS.
func TestRedactionMaskSpans(t *testing.T) {
	runtime := redactiontest.PageRuntime(t)
	f := newPreparationFixture(t, "\xe9 KEEP") // PDF's WinAnsi source byte is é.
	prep, err := f.service.PrepareNativePDF(t.Context(), f.request, runtime)
	require.NoError(t, err)
	require.Equal(t, "é KEEP", prep.Map.Text)
	mask := RedactionMask{ID: uuid.New(), Selector: redaction.Selector{Kind: "text", MapSHA256: prep.Map.SHA256, Span: &redaction.Span{Start: 0, End: 2}}}
	review, err := BuildRedactionReview(prep, []RedactionMask{mask}, 1)
	require.NoError(t, err)
	text, err := redaction.Text(review.Resolved)
	require.NoError(t, err)
	require.Equal(t, "[REDACTED] KEEP\f", string(text))
	for _, span := range []redaction.Span{{Start: 0, End: 1}, {Start: 1, End: 2}, {Start: 2, End: 2}, {Start: 2, End: 0}} {
		mask.Selector.Span = &span
		_, err := BuildRedactionReview(prep, []RedactionMask{mask}, 1)
		require.Error(t, err, "span %v must not select partial UTF-8 or an empty/reversed range", span)
	}
}
