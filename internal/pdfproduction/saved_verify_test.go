package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func savedPDFFixture(t *testing.T) (redaction.Resolved, PageArtifact, []byte) {
	t.Helper()
	plan, a, recipe := sanitizedFixture(t, "café")
	plan.RedactBoxes = []redaction.Box{{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 4000, Y0: 4000, X1: 6000, Y1: 6000}}
	sealPlan(t, &plan)
	a.ResolvedSHA256 = plan.SHA256
	pixels := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(pixels, image.Rect(120, 120, 180, 180), image.NewUniform(color.Black), image.Point{}, draw.Src)
	encodeArtifact(t, &a, pixels)
	var out bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, plan, PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: recipe.MaxStagingBytes}))
	return plan, a, out.Bytes()
}

func TestVerifySavedPDF(t *testing.T) {
	plan, a, _ := savedPDFFixture(t)
	second := renumberArtifact(t, a, 2)
	second.Page.Span = redaction.Span{Start: 5, End: 5}
	second.Layout = redaction.PageLayout{Source: second.Page, Output: second.Page}
	second.LayoutSHA256 = mustDigest(t, second.Layout)
	whole := redaction.Box{Page: 2, FrameSHA256: second.Page.FrameSHA256,
		X1: second.Page.Width, Y1: second.Page.Height}
	second.Runs = []redaction.Run{{Kind: "redaction", Text: "[REDACTED]", Page: 2,
		Anchor: 5, Boxes: []redaction.Box{whole}, FontSizeMilliPoints: 8000}}
	pixels := image.NewNRGBA(image.Rect(0, 0, 300, 300))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.Black), image.Point{}, draw.Src)
	encodeArtifact(t, &second, pixels)
	third := renumberArtifact(t, second, 3)
	third.Runs = []redaction.Run{}
	plan.PageCount = 3
	plan.Pages = append(plan.Pages, second.Page, third.Page)
	plan.Runs = append(plan.Runs, second.Runs...)
	plan.RedactBoxes = append(plan.RedactBoxes,
		redaction.Box{Page: 1, FrameSHA256: a.Page.FrameSHA256, X0: 4500, Y0: 4500, X1: 5500, Y1: 5500},
		whole)
	sealPlan(t, &plan)
	a.ResolvedSHA256, second.ResolvedSHA256 = plan.SHA256, plan.SHA256
	third.ResolvedSHA256 = plan.SHA256
	var out bytes.Buffer
	recipe := qualificationRecipe()
	pages := &artifactSequence{pages: []PageArtifact{a, second, third}}
	options := PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: recipe.MaxStagingBytes}
	require.NoError(t, WriteFresh(t.Context(), &out, pages, plan, options))
	start := time.Now()
	require.NoError(t, VerifySaved(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), plan, recipe, nil))
	t.Logf("three-page verification: %s; PDF size: %d bytes", time.Since(start), out.Len())
}

func TestVerifySavedStreamBoundary(t *testing.T) {
	plan, _, original := savedPDFFixture(t)
	for _, test := range []struct {
		name string
		edit func([]byte) []byte
	}{
		{"trailing payload", func(data []byte) []byte { return append(data, []byte("payload")...) }},
		{"empty dictionary", func(data []byte) []byte {
			return append([]byte{0x78, 0x20, 0, 0, 0, 1}, data[2:]...)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteSavedImage(t, original, test.edit)
			err := VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, qualificationRecipe(), nil)
			require.ErrorIs(t, err, ErrPDFIntegrity)
		})
	}
}

func TestVerifySavedPixelsAndEncoding(t *testing.T) {
	plan, a, original := savedPDFFixture(t)
	for _, test := range []struct {
		name  string
		level int
		pixel int
		bad   bool
	}{
		{"stored", zlib.NoCompression, -1, false},
		{"fast", zlib.BestSpeed, -1, false},
		{"huffman", zlib.HuffmanOnly, -1, false},
		{"best", zlib.BestCompression, -1, false},
		{"outside mask", zlib.DefaultCompression, 0, false},
		{"inside mask", zlib.DefaultCompression, (150*300 + 150) * 3, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteSavedImage(t, original, func(data []byte) []byte {
				r, err := zlib.NewReader(bytes.NewReader(data))
				require.NoError(t, err)
				pixels, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				if test.pixel >= 0 {
					pixels[test.pixel] ^= 1
				}
				return savedStream(t, pixels, test.level)
			})
			require.NotEqual(t, original, changed)
			err := VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, qualificationRecipe(), nil)
			if test.bad {
				require.ErrorIs(t, err, ErrPDFIntegrity)
			} else {
				require.NoError(t, err)
			}
			// Even identical decoded pixels must not weaken render-time PNG authority.
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor(t, []PageArtifact{a})))
		})
	}
}

func TestVerifySavedStructureAndReadErrors(t *testing.T) {
	plan, _, original := savedPDFFixture(t)
	changed := rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
		objects[8] = rewriteFixtureStream(t, objects[8], func(data []byte) []byte {
			return bytes.Replace(data, []byte("<0001000200030004>"), []byte("<0002000200030004>"), 1)
		})
	}, nil)
	require.NotEqual(t, original, changed)
	require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, qualificationRecipe(), nil), ErrPDFIntegrity)
	changed = rewriteFreshFixture(t, original, 1, func(map[int][]byte) {}, []byte("99 0 obj\n<<>>\nendobj\n"))
	require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, qualificationRecipe(), nil), ErrPDFIntegrity)
	failure := errors.New("synthetic saved PDF read failure")
	require.ErrorIs(t, VerifySaved(t.Context(), savedFailedReaderAt{failure}, int64(len(original)), plan, qualificationRecipe(), nil), failure)
	ctx, cancel := context.WithCancel(t.Context())
	reader := savedCancelReaderAt{ReaderAt: bytes.NewReader(original), cancel: cancel}
	require.ErrorIs(t, VerifySaved(ctx, reader, int64(len(original)), plan, qualificationRecipe(), nil), context.Canceled)
}

type savedCancelReaderAt struct {
	io.ReaderAt

	cancel context.CancelFunc
}

func (r savedCancelReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	n, err := r.ReaderAt.ReadAt(p, offset)
	r.cancel()
	return n, err
}

type savedFailedReaderAt struct{ err error }

func (r savedFailedReaderAt) ReadAt([]byte, int64) (int, error) { return 0, r.err }

func rewriteSavedImage(t *testing.T, original []byte, edit func([]byte) []byte) []byte {
	t.Helper()
	return rewriteFreshFixture(t, original, 1, func(objects map[int][]byte) {
		object := objects[6]
		start := bytes.Index(object, []byte("\nstream\n")) + 8
		end := bytes.LastIndex(object, []byte("\nendstream\nendobj\n"))
		require.Greater(t, end, start)
		data := edit(bytes.Clone(object[start:end]))
		objects[6] = append(append(bytes.Clone(object[:start]), data...), object[end:]...)
		objects[7] = fmt.Appendf(nil, "7 0 obj\n%d\nendobj\n", len(data))
	}, nil)
}
