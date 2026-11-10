package pdfproduction

import (
	"math"

	"go.kenn.io/docbank/document"
)

// PDFium exposes binary32 page boxes. Compare with that exact projection of
// the stored coordinates, without changing the frame or accepting a general
// epsilon. Large coordinates that lose more than one point/10000 unit refuse.
func nativeBoxMatches(actual, expected [4]int64) bool {
	for i, q := range expected {
		if q < -document.MaxPageInteger || q > document.MaxPageInteger {
			return false
		}
		p, err := quantizePoint(float64(float32(float64(q) / 10000)))
		if err != nil || p < -document.MaxPageInteger || p > document.MaxPageInteger || p-q < -1 || p-q > 1 || actual[i] != p {
			return false
		}
	}
	return true
}

// Source dimensions round forward into the frame's physical units. Reversing
// that rounding and applying a point epsilon rejects valid rounded frames.
// Output verification deliberately retains its separate writer contract.
func sourceDimensionMatches(points float64, physical int64) bool {
	units := math.Round(points * 10000 / 72)
	return points > 0 && units > 0 && units <= float64(document.MaxPageInteger) && units == float64(physical)
}
