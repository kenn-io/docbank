package pdfstamp

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/go-pdf/fpdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
	"github.com/stretchr/testify/require"
)

func TestStampScannedPage(t *testing.T) {
	scan := image.NewGray(image.Rect(0, 0, 2100, 2100))
	for i := range scan.Pix {
		scan.Pix[i] = 255
	}
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, scan))
	pdf := fpdf.New("P", "pt", "Letter", "")
	pdf.AddPage()
	opts := fpdf.ImageOptions{ImageType: "PNG"}
	pdf.RegisterImageOptionsReader("scan", opts, &encoded)
	pdf.ImageOptions("scan", 0, 0, 612, 792, false, opts, 0, "")
	var source, output bytes.Buffer
	require.NoError(t, pdf.Output(&source))
	_, err := Stamp(t.Context(), bytes.NewReader(source.Bytes()),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
	require.NoError(t, err)
	t.Run("visible", func(t *testing.T) {
		require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
		assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
	})
}

func rewritePDF(t *testing.T, source []byte, edit func(*model.Context, types.Dict)) []byte {
	t.Helper()
	conf := &model.Configuration{Reader15: true, ValidationMode: model.ValidationRelaxed,
		Offline: true, Eol: types.EolLF, Limits: model.DefaultResourceLimits()}
	ctx, err := api.ReadAndValidate(bytes.NewReader(source), conf)
	require.NoError(t, err)
	page, _, _, err := ctx.PageDict(1, false)
	require.NoError(t, err)
	edit(ctx, page)
	var output bytes.Buffer
	require.NoError(t, api.WriteContext(ctx, &output))
	return output.Bytes()
}

func TestStampSourceFormContainingAssignedLabel(t *testing.T) {
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		form, err := ctx.NewStreamDictForBuf([]byte("BT /F1 9 Tf 50 400 Td (OUR000041) Tj ET"))
		require.NoError(t, err)
		form.InsertName("Type", "XObject")
		form.InsertName("Subtype", "Form")
		form.Insert("BBox", types.NewNumberArray(0, 0, 612, 792))
		form.Insert("Resources", types.Dict{"Font": types.Dict{"F1": types.Dict{
			"Type": types.Name("Font"), "Subtype": types.Name("Type1"), "BaseFont": types.Name("Helvetica"),
		}}})
		require.NoError(t, form.Encode())
		ref, err := ctx.IndRefForNewObject(*form)
		require.NoError(t, err)
		page.Update("Resources", types.Dict{"XObject": types.Dict{"Source": *ref}})
		content, err := ctx.NewStreamDictForBuf([]byte("/Source Do"))
		require.NoError(t, err)
		require.NoError(t, content.Encode())
		ref, err = ctx.IndRefForNewObject(*content)
		require.NoError(t, err)
		page.Update("Contents", *ref)
	})
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(source),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
	require.NoError(t, err)
}

func TestStampSameLabelRestamp(t *testing.T) {
	recipe := validRecipe(t)
	labels := []PageLabel{{SourcePage: 1, Label: "OUR000041"}}
	var first, second bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 1, "Letter")), labels, recipe, &first)
	require.NoError(t, err)
	recipe.Restamp = true
	_, err = Stamp(t.Context(), bytes.NewReader(first.Bytes()), labels, recipe, &second)
	require.NoError(t, err)
	pdfContext, err := api.ReadAndValidate(bytes.NewReader(second.Bytes()), stampConfiguration())
	require.NoError(t, err)
	page, _, _, err := pdfContext.PageDict(1, false)
	require.NoError(t, err)
	content, err := pdfContext.PageContent(page, 1)
	require.NoError(t, err)
	require.Equal(t, 1, bytes.Count(content, []byte(watermarkArtifact)), "restamping must replace the previous draw")
}

func TestStampIgnoresHostConfiguration(t *testing.T) {
	if os.Getenv("DOCBANK_STAMP_CONFIG_PROBE") == "1" {
		var output bytes.Buffer
		_, err := Stamp(t.Context(), bytes.NewReader(syntheticPDF(t, 1, "Letter")),
			[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
		require.NoError(t, err)
		return
	}
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("synthetic"), 0600))
	t.Setenv("XDG_CONFIG_HOME", blocked)
	t.Setenv("APPDATA", blocked)
	if runtime.GOOS == "darwin" {
		// macOS has no separate environment override for os.UserConfigDir.
		t.Setenv("HOME", blocked)
	}
	t.Setenv("DOCBANK_STAMP_CONFIG_PROBE", "1")
	executable, err := os.Executable()
	require.NoError(t, err)
	command := exec.CommandContext(t.Context(), executable, "-test.run=^TestStampIgnoresHostConfiguration$")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

func TestStampSourceLayerCannotHideLabel(t *testing.T) {
	requirePopplerQualification(t)
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		group, err := ctx.IndRefForNewObject(types.Dict{
			"Type": types.Name("OCG"), "Name": types.StringLiteral("Synthetic hidden layer"),
		})
		require.NoError(t, err)
		catalog, err := ctx.Catalog()
		require.NoError(t, err)
		catalog.Update("OCProperties", types.Dict{
			"OCGs": types.Array{*group},
			"D":    types.Dict{"BaseState": types.Name("OFF"), "OFF": types.Array{*group}},
		})
	})
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(source),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
	require.NoError(t, err)
	require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
	assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
	var second bytes.Buffer
	_, err = Stamp(t.Context(), bytes.NewReader(output.Bytes()),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &second)
	require.ErrorIs(t, err, ErrStampEngineFailure, "an existing source layer must not bypass the restamp guard")
	require.Zero(t, second.Len())
}

func TestRecipeEngineMatchesLinkedModule(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok)
	for _, dependency := range info.Deps {
		if dependency.Path == "github.com/pdfcpu/pdfcpu" {
			require.Nil(t, dependency.Replace, "a replacement requires engine requalification")
			require.Equal(t, dependency.Version, validRecipe(t).EngineIdentity.Version)
			return
		}
	}
	t.Fatal("pdfcpu is not linked")
}

func TestRecipeRejectsPDFCPUTextEscapes(t *testing.T) {
	for _, prefix := range []string{"OUR%", `OUR\n`} {
		t.Run(prefix, func(t *testing.T) {
			recipe := validRecipe(t)
			recipe.Prefix = prefix
			require.Error(t, recipe.Validate())
		})
	}
}

func TestStampPreservesDisplayedSourceGeometry(t *testing.T) {
	requirePopplerQualification(t)
	for _, rotation := range []int{0, 90, 180, 270, -90, 450} {
		for _, inherited := range []bool{false, true} {
			t.Run(fmt.Sprintf("rotation=%d/inherited=%t", rotation, inherited), func(t *testing.T) {
				pdf := fpdf.New("P", "pt", "Letter", "")
				pdf.AddPage()
				pdf.SetFillColor(0, 0, 255)
				pdf.Rect(150, 400, 70, 35, "F")
				var original bytes.Buffer
				require.NoError(t, pdf.Output(&original))
				source := rewritePDF(t, original.Bytes(), func(ctx *model.Context, page types.Dict) {
					page.Update("CropBox", types.NewNumberArray(100, 200, 500, 700))
					if inherited {
						parent, err := ctx.DereferenceDict(page["Parent"])
						require.NoError(t, err)
						parent.Update("Rotate", types.Integer(rotation))
						page.Delete("Rotate")
					} else {
						page.Update("Rotate", types.Integer(rotation))
					}
				})
				var output bytes.Buffer
				_, err := Stamp(t.Context(), bytes.NewReader(source),
					[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
				require.NoError(t, err)
				require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
				before, after := renderPDFPages(t, source)[0], renderPDFPages(t, output.Bytes())[0]
				require.Equal(t, before.Bounds(), after.Bounds(), "displayed page dimensions")
				added := image.NewRGBA(after.Bounds())
				for y := range after.Bounds().Dy() {
					for x := range after.Bounds().Dx() {
						r1, g1, b1, a1 := before.At(x, y).RGBA()
						r2, g2, b2, a2 := after.At(x, y).RGBA()
						if r1 == r2 && g1 == g2 && b1 == b2 && a1 == a2 {
							continue
						}
						if r1 != 0xffff || g1 != 0xffff || b1 != 0xffff {
							t.Fatalf("source content changed at pixel (%d, %d)", x, y)
						}
						added.Set(x, y, after.At(x, y))
					}
				}
				ink, ok := visibleInkBounds(added)
				require.True(t, ok, "stamp has no visible pixels")
				assertInkAt(t, added.Bounds(), ink, "bottom-right", 24)
			})
		}
	}
}

func TestStampFitsEquivalentRotationOnNarrowPage(t *testing.T) {
	requirePopplerQualification(t)
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		page.Update("MediaBox", types.NewNumberArray(0, 0, 60, 200))
		page.Update("Rotate", types.Integer(450))
	})
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(source),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
	require.NoError(t, err)
	require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
	assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
}

func TestRestampReplacesOldVisibleLabel(t *testing.T) {
	requirePopplerQualification(t)
	recipe := validRecipe(t)
	recipe.Restamp = true
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(syntheticAlreadyStamped(t)),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)
	require.NoError(t, err)
	require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
	assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
}

func TestStampPagesSharingSourceContentAndResources(t *testing.T) {
	for _, restamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("restamp=%t", restamp), func(t *testing.T) {
			original := syntheticPDF(t, 2, "Letter")
			if restamp {
				var err error
				original, err = stampPages(original, []string{"OLD000001", "OLD000002"}, "Helvetica")
				require.NoError(t, err)
			}
			source := rewritePDF(t, original, func(ctx *model.Context, page types.Dict) {
				second, _, _, err := ctx.PageDict(2, false)
				require.NoError(t, err)
				second.Update("Contents", page["Contents"])
				resources, err := ctx.DereferenceDict(page["Resources"])
				require.NoError(t, err)
				shared, err := ctx.IndRefForNewObject(resources)
				require.NoError(t, err)
				page.Update("Resources", *shared)
				second.Update("Resources", *shared)
			})
			recipe := validRecipe(t)
			recipe.Restamp = restamp
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(source),
				[]PageLabel{{SourcePage: 1, Label: "OUR000041"}, {SourcePage: 2, Label: "OUR000042"}}, recipe, &output)
			require.NoError(t, err)
			t.Run("visible", func(t *testing.T) {
				require.Equal(t, []string{"OUR000041", "OUR000042"}, readVisibleLabels(t, output.Bytes()))
			})
		})
	}
}

func TestRestampAllowsBlankPages(t *testing.T) {
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		page.Delete("Contents")
		page.Delete("Resources")
	})
	recipe := validRecipe(t)
	recipe.Restamp = true
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(source),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)
	require.NoError(t, err)
}

func TestStampPreservesContentStreamBoundaries(t *testing.T) {
	requirePopplerQualification(t)
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		contents := types.Array{}
		for _, operators := range []string{"0 0 1 rg", "150 400 70 35 re f"} {
			stream, err := ctx.NewStreamDictForBuf([]byte(operators))
			require.NoError(t, err)
			require.NoError(t, stream.Encode())
			ref, err := ctx.IndRefForNewObject(*stream)
			require.NoError(t, err)
			contents = append(contents, *ref)
		}
		page.Update("Contents", contents)
	})
	before := renderPDFPages(t, source)[0]
	require.Equal(t, color.RGBA{B: 255, A: 255}, before.At(320, 730))
	var output bytes.Buffer
	_, err := Stamp(t.Context(), bytes.NewReader(source),
		[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
	require.NoError(t, err)
	after := renderPDFPages(t, output.Bytes())[0]
	require.Equal(t, before.At(320, 730), after.At(320, 730), "preserve the source rectangle")
	require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
}

func TestStampIsolatesSourceGraphicsState(t *testing.T) {
	requirePopplerQualification(t)
	for name, operators := range map[string]string{
		"clip":                "0 0 10 10 re W n",
		"transform":           "1 0 0 1 1000 1000 cm",
		"clip then save":      "0 0 10 10 re W n q",
		"restore then clip":   "Q 0 0 10 10 re W n",
		"transform then save": "1 0 0 1 1000 1000 cm q",
	} {
		t.Run(name, func(t *testing.T) {
			source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
				require.NoError(t, setPageContent(ctx, page, []byte(operators)))
			})
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(source),
				[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, validRecipe(t), &output)
			require.NoError(t, err)
			require.Equal(t, []string{"OUR000041"}, readVisibleLabels(t, output.Bytes()))
			assertRenderedStampAt(t, output.Bytes(), "bottom-right", 24)
		})
	}
}

func TestStampRejectsAnnotationsBeforePublishing(t *testing.T) {
	source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
		appearance, err := ctx.NewStreamDictForBuf([]byte("1 1 1 rg 0 0 612 792 re f"))
		require.NoError(t, err)
		appearance.InsertName("Type", "XObject")
		appearance.InsertName("Subtype", "Form")
		appearance.Insert("BBox", types.NewNumberArray(0, 0, 612, 792))
		appearance.Insert("Resources", types.NewDict())
		require.NoError(t, appearance.Encode())
		ref, err := ctx.IndRefForNewObject(*appearance)
		require.NoError(t, err)
		annotation, err := ctx.IndRefForNewObject(types.Dict{
			"Type": types.Name("Annot"), "Subtype": types.Name("Square"),
			"Rect": types.NewNumberArray(0, 0, 612, 792), "F": types.Integer(4),
			"AP": types.Dict{"N": *ref},
		})
		require.NoError(t, err)
		page.Update("Annots", types.Array{*annotation})
	})
	for _, restamp := range []bool{false, true} {
		t.Run(fmt.Sprintf("restamp=%t", restamp), func(t *testing.T) {
			recipe := validRecipe(t)
			recipe.Restamp = restamp
			var output bytes.Buffer
			_, err := Stamp(t.Context(), bytes.NewReader(source),
				[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)
			require.ErrorIs(t, err, ErrStampEngineFailure)
			require.ErrorContains(t, err, "flatten annotations before stamping")
			require.Zero(t, output.Len())
		})
	}
}

func TestStampRejectsTaggedSourcesBeforePublishing(t *testing.T) {
	for _, marked := range []bool{false, true} {
		source := rewritePDF(t, syntheticPDF(t, 1, "Letter"), func(ctx *model.Context, page types.Dict) {
			_, pageRef, _, err := ctx.PageDict(1, false)
			require.NoError(t, err)
			tree := types.Dict{"Type": types.Name("StructTreeRoot")}
			treeRef, err := ctx.IndRefForNewObject(tree)
			require.NoError(t, err)
			element, err := ctx.IndRefForNewObject(types.Dict{
				"Type": types.Name("StructElem"), "S": types.Name("P"),
				"P": *treeRef, "Pg": *pageRef, "K": types.Integer(0),
			})
			require.NoError(t, err)
			tree.Update("K", *element)
			tree.Update("ParentTree", types.Dict{"Nums": types.Array{types.Integer(0), types.Array{*element}}})
			tree.Update("ParentTreeNextKey", types.Integer(1))
			catalog, err := ctx.Catalog()
			require.NoError(t, err)
			catalog.Update("Version", types.Name("1.4"))
			catalog.Update("StructTreeRoot", *treeRef)
			if marked {
				catalog.Update("MarkInfo", types.Dict{"Marked": types.Boolean(true)})
			}
			page.Update("StructParents", types.Integer(0))
			require.NoError(t, setPageContent(ctx, page, []byte("/P <</MCID 0>> BDC 0 0 1 rg 150 400 70 35 re f EMC")))
		})
		for _, restamp := range []bool{false, true} {
			t.Run(fmt.Sprintf("marked=%t/restamp=%t", marked, restamp), func(t *testing.T) {
				recipe := validRecipe(t)
				recipe.Restamp = restamp
				var output bytes.Buffer
				_, err := Stamp(t.Context(), bytes.NewReader(source),
					[]PageLabel{{SourcePage: 1, Label: "OUR000041"}}, recipe, &output)
				require.ErrorIs(t, err, ErrStampEngineFailure)
				require.ErrorContains(t, err, "tagged PDFs are not supported")
				require.Zero(t, output.Len())
			})
		}
	}
}
