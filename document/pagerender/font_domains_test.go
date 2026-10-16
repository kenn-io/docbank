package pagerender

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInspectionFontDomains(t *testing.T) {
	simple := "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"
	type3 := "<< /Type /Font /Subtype /Type3 /FontBBox [0 0 600 600] /FontMatrix [0.001 0 " +
		"0 0.001 0 0] /CharProcs << /space 7 0 R >> /Encoding << /Type /Encoding " +
		"/Differences [32 /space] >> /FirstChar 32 /LastChar 32 /Widths [600] " +
		"/Resources << >> >>"
	cid := "<< /Type /Font /Subtype /Type0 /BaseFont /Helvetica /Encoding /Identity-H " +
		"/DescendantFonts [6 0 R] >>"
	for _, tc := range []struct {
		name, resources, font string
		inherited, want       bool
	}{
		{"direct", "/Font << /F1 4 0 R >>", simple, false, true},
		{"inherited", "/Font << /F1 4 0 R >>", simple, true, true},
		{"graphics state only", "/ExtGState << /GS << /Type /ExtGState /Font [4 0 R 12] >> >>", simple,
			false, true},
		{"inherited graphics state", "/ExtGState << /GS << /Type /ExtGState /Font [4 0 R 12] >> >>",
			simple, true, true},
		{"unused Type3", "/Font << /F1 5 0 R /Unused 4 0 R >>", type3, false, false},
		{"unused CID", "/Font << /F1 5 0 R /Unused 4 0 R >>", cid, false, false},
		{"graphics state CID",
			"/Font << /F1 5 0 R >> /ExtGState << /GS << /Type /ExtGState /Font [4 0 R 12] >> >>", cid,
			true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, page := "", ""
			resource := "/Resources << " + tc.resources + " >>"
			if tc.inherited {
				parent = resource
			} else {
				page = resource
			}
			data := syntheticPDFObjects([]string{
				"<< /Type /Catalog /Pages 2 0 R >>",
				"<< /Type /Pages /Kids [3 0 R] /Count 1 /MediaBox [0 0 612 792] " + parent + " >>",
				"<< /Type /Page /Parent 2 0 R " + page + " >>",
				tc.font, simple,
				"<< /Type /Font /Subtype /CIDFontType0 /BaseFont /Helvetica /CIDSystemInfo " +
					"<< /Registry (Adobe) /Ordering (Identity) /Supplement 0 >> >>",
				"<< /Length 7 >>\nstream\n600 0 d0\nendstream",
			})
			result, err := InspectInput(bytes.NewReader(data), testSource(data).VersionID, "application/pdf")
			require.NoError(t, err)
			require.Equal(t, testSource(data), result.Source)
			require.Equal(t, []PageFontDomain{{Page: 1, SimpleFontsOnly: new(tc.want)}}, result.FontDomains)
		})
	}
}

func TestInspectionRejectsUnboundFontDomains(t *testing.T) {
	data := syntheticPDF([]string{"/MediaBox [0 0 612 792]"}, "")
	result, err := InspectInput(bytes.NewReader(data), testSource(data).VersionID, "application/pdf")
	require.NoError(t, err)
	for _, mutate := range []func(*Inspection){
		func(v *Inspection) { v.Source.SHA256 = fmt.Sprintf("%064d", 1) },
		func(v *Inspection) { v.ParserIdentity = "unknown" },
		func(v *Inspection) { v.FontDomains = nil },
		func(v *Inspection) { v.FontDomains = []PageFontDomain{{Page: 2, SimpleFontsOnly: new(true)}} },
		func(v *Inspection) { v.FontDomains = []PageFontDomain{{Page: 1}} },
	} {
		changed := result
		mutate(&changed)
		require.ErrorIs(t, ValidateInspection(changed, testSource(data), "application/pdf"),
			ErrInvalidOutput)
	}
	require.NoError(t, ValidateInspection(result, testSource(data), "application/pdf"))
}
