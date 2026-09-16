package pdfstamp

import (
	"bytes"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/internal/packagetest"
)

var visibleLabelPattern = regexp.MustCompile(`\b[A-Z][A-Z0-9_-]*[0-9]{4,}[A-Z0-9_-]*\b`)

const qualifiedPopplerVersion = "26.09.0"

func requirePopplerQualification(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCBANK_PDFSTAMP_QUALIFY") != "1" {
		t.Skip("set DOCBANK_PDFSTAMP_QUALIFY=1 to run the pinned Poppler qualification")
	}
	for _, executable := range []string{"pdftotext", "pdftoppm"} {
		path, err := exec.LookPath(executable)
		require.NoError(t, err, "%s is required for PDF stamp qualification", executable)
		command := exec.CommandContext(t.Context(), path, "-v")
		version, err := command.CombinedOutput()
		require.NoError(t, err, string(version))
		require.Contains(t, string(version), executable+" version "+qualifiedPopplerVersion)
	}
}

func readVisibleLabels(t *testing.T, pdf []byte) []string {
	t.Helper()
	requirePopplerQualification(t)
	text, err := packagetest.PDFText(t.Context(), pdf)
	require.NoError(t, err)
	return visibleLabelPattern.FindAllString(text, -1)
}

func assertRenderedStampAt(t *testing.T, pdf []byte, position string, marginPoints int) {
	t.Helper()
	for _, decoded := range renderPDFPages(t, pdf) {
		ink, ok := visibleInkBounds(decoded)
		require.True(t, ok, "page contains no rendered stamp pixels")
		assertInkAt(t, decoded.Bounds(), ink, position, marginPoints)
	}
}

func renderPDFPages(t *testing.T, pdf []byte) []image.Image {
	t.Helper()
	requirePopplerQualification(t)
	directory := t.TempDir()
	prefix := filepath.Join(directory, "page")
	command := exec.CommandContext(t.Context(), "pdftoppm", "-r", "144", "-cropbox", "-png", "-", prefix)
	command.Stdin = bytes.NewReader(pdf)
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	paths, err := filepath.Glob(prefix + "-*.png")
	require.NoError(t, err)
	sort.Strings(paths)
	require.NotEmpty(t, paths)
	pages := make([]image.Image, 0, len(paths))
	for pageIndex, path := range paths {
		file, err := os.Open(path)
		require.NoError(t, err)
		decoded, _, err := image.Decode(file)
		closeErr := file.Close()
		require.NoError(t, err)
		require.NoError(t, closeErr)
		pages = append(pages, decoded)
		retainGolden(t, path, pageIndex+1)
	}
	return pages
}

func visibleInkBounds(rendered image.Image) (image.Rectangle, bool) {
	bounds := rendered.Bounds()
	ink := image.Rectangle{Min: image.Point{X: bounds.Max.X, Y: bounds.Max.Y}, Max: bounds.Min}
	found := false
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			red, green, blue, alpha := rendered.At(x, y).RGBA()
			if alpha > 0xf000 && (red < 0xe000 || green < 0xe000 || blue < 0xe000) {
				found = true
				if x < ink.Min.X {
					ink.Min.X = x
				}
				if y < ink.Min.Y {
					ink.Min.Y = y
				}
				if x+1 > ink.Max.X {
					ink.Max.X = x + 1
				}
				if y+1 > ink.Max.Y {
					ink.Max.Y = y + 1
				}
			}
		}
	}
	return ink, found
}

func assertInkAt(t *testing.T, page, ink image.Rectangle, position string, marginPoints int) {
	t.Helper()
	const pixelsPerPoint = 2
	const tolerance = 8
	margin := float64(marginPoints * pixelsPerPoint)
	wantX, wantY := float64(page.Dx())/2, float64(page.Dy())/2
	gotX, gotY := float64(ink.Min.X+ink.Max.X)/2, float64(ink.Min.Y+ink.Max.Y)/2
	if strings.HasSuffix(position, "left") {
		wantX, gotX = margin, float64(ink.Min.X)
	} else if strings.HasSuffix(position, "right") {
		wantX, gotX = float64(page.Max.X)-margin, float64(ink.Max.X)
	}
	if strings.HasPrefix(position, "top") {
		wantY, gotY = margin, float64(ink.Min.Y)
	} else if strings.HasPrefix(position, "bottom") {
		wantY, gotY = float64(page.Max.Y)-margin, float64(ink.Max.Y)
	}
	assert.InDelta(t, wantX, gotX, tolerance, "horizontal stamp anchor")
	assert.InDelta(t, wantY, gotY, tolerance, "vertical stamp anchor")
}

func retainGolden(t *testing.T, source string, page int) {
	t.Helper()
	directory := os.Getenv("DOCBANK_PDFSTAMP_GOLDEN_DIR")
	if directory == "" {
		return
	}
	require.NoError(t, os.MkdirAll(directory, 0o750))
	name := strings.NewReplacer("/", "-", "\\", "-", " ", "-").Replace(t.Name())
	destination := filepath.Join(directory, name+fmt.Sprintf("-page-%d.png", page))
	contents, err := os.ReadFile(source)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(destination, contents, 0o600))
}
