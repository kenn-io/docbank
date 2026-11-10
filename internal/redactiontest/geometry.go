package redactiontest

import (
	_ "embed"
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Shared with browser qualification: literal observations from both pinned PDFium engines.
//
//go:embed testdata/page-geometry.json
var geometryData []byte

type GeometryFixture struct {
	Name          string     `json:"name"`
	Media         [4]float64 `json:"media"`
	Crop          [4]float64 `json:"crop"`
	ObservedMedia [4]float64 `json:"observedMedia"`
	ObservedCrop  [4]float64 `json:"observedCrop"`
	Physical      [2]int64   `json:"physical"`
	Pixels        [2]int     `json:"pixels"`
}

func GeometryFixtures(tb testing.TB) []GeometryFixture {
	tb.Helper()
	var fixtures []GeometryFixture
	require.NoError(tb, json.Unmarshal(geometryData, &fixtures))
	return fixtures
}

// GeometryPDF authors the original decimal boxes directly, independently of frame construction.
// Every page paints SECRET KEEP at (72,72); inherited boxes exercise PDFium's fallback boundary.
func GeometryPDF(media, crop [4]float64, rotations []int, inherited bool) []byte {
	boxes := fmt.Sprintf("/MediaBox [%g %g %g %g] /CropBox [%g %g %g %g]", media[0], media[1], media[2], media[3], crop[0], crop[1], crop[2], crop[3])
	objects := []string{"<< /Type /Catalog /Pages 2 0 R >>", "", "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"}
	var kids []string
	const content = "BT /F1 12 Tf 72 72 Td (SECRET KEEP) Tj ET\n"
	for _, rotation := range rotations {
		id := len(objects) + 1
		kids = append(kids, fmt.Sprintf("%d 0 R", id))
		direct := boxes
		if inherited {
			direct = ""
		}
		objects = append(objects, fmt.Sprintf("<< /Type /Page /Parent 2 0 R %s /Rotate %d /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", direct, rotation, id+1), fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content))
	}
	parentBoxes := ""
	if inherited {
		parentBoxes = boxes
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Count %d /Kids [%s] %s >>", len(rotations), strings.Join(kids, " "), parentBoxes)
	var pdf strings.Builder
	pdf.WriteString("%PDF-1.7\n")
	var offsets []int
	for i, obj := range objects {
		offsets = append(offsets, pdf.Len())
		fmt.Fprintf(&pdf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := pdf.Len()
	fmt.Fprintf(&pdf, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		fmt.Fprintf(&pdf, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&pdf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return []byte(pdf.String())
}
