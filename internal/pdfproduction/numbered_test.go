package pdfproduction

import (
	"bytes"
	"compress/zlib"
	"image"
	"image/color"
	"image/draw"
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/redaction"
)

func TestVerifySavedNumbered(t *testing.T) {
	plan, a, _ := savedPDFFixture(t)
	recipe := qualificationRecipe()
	presentation, err := NumberedPresentation(plan.Pages, []string{"1"}, QualifiedNumberedFooter(recipe))
	require.NoError(t, err)
	p := presentation[0]
	a.Page, a.Layout = p.Layout.Output, p.Layout
	a.Endorsements = p.Endorsements
	a.LayoutSHA256, a.EndorsementsSHA256 = mustDigest(t, a.Layout), mustDigest(t, a.Endorsements)
	pixels := image.NewNRGBA(image.Rect(0, 0, 300, 450))
	draw.Draw(pixels, pixels.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(pixels, image.Rect(120, 120, 180, 180), image.NewUniform(color.Black), image.Point{}, draw.Src)
	require.NoError(t, paintEndorsements(pixels, a.Layout, a.Endorsements, recipe))
	encodeArtifact(t, &a, pixels)
	var out bytes.Buffer
	require.NoError(t, WriteFresh(t.Context(), &out, &artifactSequence{pages: []PageArtifact{a}}, plan,
		PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: recipe.MaxStagingBytes}))
	require.NoError(t, VerifyFresh(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()),
		verificationFor(t, []PageArtifact{a})))
	require.NoError(t, VerifySaved(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), plan, recipe, presentation))

	adjustGray := func(delta byte) func([]byte) {
		return func(rgb []byte) {
			for i := 300 * 300 * 3; i < len(rgb); i += 3 {
				if rgb[i] >= 32 && rgb[i] <= 223 {
					rgb[i] += delta
					rgb[i+1] += delta
					rgb[i+2] += delta
					return
				}
			}
			t.Fatal("fixture must contain an antialiased edge")
		}
	}
	for _, test := range []struct {
		name  string
		edit  func([]byte)
		valid bool
	}{
		{"antialiasing", adjustGray(16), true},
		{"excessive antialiasing", adjustGray(17), false},
		{"blank number", func(rgb []byte) {
			for i := 300 * 300 * 3; i < len(rgb); i++ {
				rgb[i] = 255
			}
		}, false},
		{"moved visible number", func(rgb []byte) {
			for y := 300; y < 450; y++ {
				row := rgb[y*300*3 : (y+1)*300*3]
				copy(row[12:], row[:len(row)-12])
				for i := range 12 {
					row[i] = 255
				}
			}
		}, false},
		{"colored ink", func(rgb []byte) {
			edited := false
			for i := 300 * 300 * 3; i < len(rgb); i += 3 {
				if rgb[i] == 0 {
					rgb[i] = 255
					edited = true
					break
				}
			}
			require.True(t, edited)
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := rewriteSavedImage(t, out.Bytes(), func(data []byte) []byte {
				r, err := zlib.NewReader(bytes.NewReader(data))
				require.NoError(t, err)
				rgb, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				test.edit(rgb)
				return savedStream(t, rgb, zlib.DefaultCompression)
			})
			err := VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, recipe, presentation)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrPDFIntegrity)
			}
			require.Error(t, VerifyFresh(t.Context(), bytes.NewReader(changed), int64(len(changed)), verificationFor(t, []PageArtifact{a})))
		})
	}
	wrong, err := NumberedPresentation(plan.Pages, []string{"2"}, QualifiedNumberedFooter(recipe))
	require.NoError(t, err)
	require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(out.Bytes()), int64(out.Len()), plan, recipe, wrong), ErrPDFIntegrity)
	for _, test := range []struct {
		name string
		edit func([]redaction.Endorsement) []redaction.Endorsement
	}{
		{"missing text", func(_ []redaction.Endorsement) []redaction.Endorsement { return []redaction.Endorsement{} }},
		{"wrong text", func(e []redaction.Endorsement) []redaction.Endorsement { e[0].Text = "2"; return e }},
		{"moved text", func(e []redaction.Endorsement) []redaction.Endorsement { e[0].Box.X0 += 100; return e }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := a
			changed.Endorsements = test.edit(slices.Clone(a.Endorsements))
			changed.EndorsementsSHA256 = mustDigest(t, changed.Endorsements)
			var altered bytes.Buffer
			require.NoError(t, WriteRendition(t.Context(), &altered, &artifactSequence{pages: []PageArtifact{changed}},
				PDFWriteOptions{Recipe: recipe, StagingDirectory: t.TempDir(), MaxBytes: recipe.MaxStagingBytes}))
			// The raster still has the expected label; this isolates the PDF text check.
			require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(altered.Bytes()), int64(altered.Len()),
				plan, recipe, presentation), ErrPDFIntegrity)
		})
	}
	changed := rewriteSavedImage(t, out.Bytes(), func(data []byte) []byte {
		r, err := zlib.NewReader(bytes.NewReader(data))
		require.NoError(t, err)
		pixels, err := io.ReadAll(r)
		require.NoError(t, err)
		require.NoError(t, r.Close())
		pixels[(449*300)*3] ^= 1
		return savedStream(t, pixels, zlib.DefaultCompression)
	})
	require.ErrorIs(t, VerifySaved(t.Context(), bytes.NewReader(changed), int64(len(changed)), plan, recipe, presentation), ErrPDFIntegrity)
}

func TestNumberedPresentationRejectsFooterOverflow(t *testing.T) {
	t.Parallel()
	recipe := qualificationRecipe()
	page := redaction.Page{Number: 1, Width: 10000, Height: 500000,
		FrameSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	presentation, err := NumberedPresentation([]redaction.Page{page}, []string{"Q000001"}, QualifiedNumberedFooter(recipe))
	require.NoError(t, err)
	err = PreflightPresentation(presentation, recipe)
	require.NoError(t, err)
	// The source is 16,380 pixels high at 300 DPI, inside the 16,384-axis cap.
	// Adding the 150-pixel strip would exceed the qualified output geometry.
	page.Height = 546000
	presentation, err = NumberedPresentation([]redaction.Page{page}, []string{"Q000001"}, QualifiedNumberedFooter(recipe))
	require.NoError(t, err)
	err = PreflightPresentation(presentation, recipe)
	var problem *redaction.Problem
	require.ErrorAs(t, err, &problem)
	require.Equal(t, "endorsement_layout_conflict", problem.Code)
}
