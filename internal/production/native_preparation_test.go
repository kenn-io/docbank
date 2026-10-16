package production

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/pdfproduction"
)

func nativePreparationFixture(t *testing.T) ([]document.PageFrameV1, NativePDFEvidence) {
	t.Helper()
	source := document.PageSource{VersionID: "00000000-0000-4000-8000-000000000001",
		SHA256: strings.Repeat("a", 64), Size: 100}
	frames := make([]document.PageFrameV1, 2)
	pages := make([]pdfproduction.NativeTextPage, 2)
	for i, text := range []string{"Aé", "中B"} {
		frame, err := document.NewPDFPageFrame(source, i+1,
			[4]float64{0, 0, 612, 792}, [4]float64{0, 0, 612, 792}, 0)
		require.NoError(t, err)
		frames[i] = frame
		pages[i].Number = i + 1
		for j, r := range []rune(text) {
			x := int64(720000 + j*100000)
			pages[i].Glyphs = append(pages[i].Glyphs, pdfproduction.NativeGlyph{
				Text: string(r), Bounds: [4]int64{x, 7000000, x + 80000, 7100000}})
		}
	}
	return frames, NativePDFEvidence{Contract: "native-pdf-evidence/v1", Source: source,
		PageRuntimeFingerprint: strings.Repeat("b", 64),
		RendererSHA256:         pdfproduction.QualifiedRecipe().RendererSHA256, Pages: pages}
}

func TestNativePreparationAssembly(t *testing.T) {
	frames, evidence := nativePreparationFixture(t)
	result, err := assembleNativePreparation(frames, evidence)
	require.NoError(t, err)
	require.NoError(t, ValidateNativePreparation(result))
	require.Equal(t, "Aé中B", result.Map.Text)
	require.Equal(t, redaction.Span{Start: 0, End: 3}, result.Map.Pages[0].Span)
	require.Equal(t, redaction.Span{Start: 3, End: 7}, result.Map.Pages[1].Span)
	require.Len(t, result.Map.Atoms, 4)
	for i, span := range []redaction.Span{{0, 1}, {1, 3}, {3, 6}, {6, 7}} {
		require.Equal(t, span, result.Map.Atoms[i].Span)
	}
	require.Empty(t, result.Map.Units)
}

func TestNativePreparationGapsAndEmptyPages(t *testing.T) {
	frames, evidence := nativePreparationFixture(t)
	evidence.Pages[0].Glyphs = nil
	result, err := assembleNativePreparation(frames, evidence)
	require.NoError(t, err)
	require.Equal(t, redaction.Span{}, result.Map.Pages[0].Span)
	require.Equal(t, "中B", result.Map.Text)
	evidence.Pages[1].Glyphs = nil
	_, err = assembleNativePreparation(frames, evidence)
	require.Error(t, err)
	evidence.Pages[0].Glyphs = []pdfproduction.NativeGlyph{{GapReason: "visible_whitespace",
		Bounds: [4]int64{720000, 7000000, 800000, 7100000}}}
	evidence.Pages[1].NonTextBounds = [][4]int64{{720000, 7000000, 800000, 7100000}}
	result, err = assembleNativePreparation(frames, evidence)
	require.NoError(t, err)
	require.Len(t, result.Map.Gaps, 2)
	require.False(t, result.Map.Gaps[0].Unordered)
	require.True(t, result.Map.Gaps[1].Unordered)
	_, err = redaction.Resolve(result.Map, "redact_selected", nil, pdfproduction.QualifiedRecipe())
	require.Error(t, err)
}

func TestNativePreparationBindings(t *testing.T) {
	for _, field := range []string{"frame source", "map frame", "evidence source"} {
		t.Run(field, func(t *testing.T) {
			frames, evidence := nativePreparationFixture(t)
			result, err := assembleNativePreparation(frames, evidence)
			require.NoError(t, err)
			switch field {
			case "frame source":
				result.Frames[0].Source.SHA256 = strings.Repeat("c", 64)
			case "map frame":
				result.Map.Pages[0].Width++
				_, result.Map.SHA256, err = redaction.CanonicalTextMap(result.Map)
				require.NoError(t, err)
			case "evidence source":
				result.Evidence.Source.VersionID = "00000000-0000-4000-8000-000000000002"
				result.EvidenceSHA256, err = canonicalDigest(result.Evidence)
				require.NoError(t, err)
			}
			require.Error(t, ValidateNativePreparation(result))
		})
	}
}
