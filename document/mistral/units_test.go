package mistral

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLocalUnitCounterRegistryIsMistralOwnedAndBounded(t *testing.T) {
	// This guards future registrations and keeps local counters tied to formats.
	for formatID, counter := range localUnitCounters {
		if counter == nil {
			t.Fatalf("localUnitCounters[%q] is nil", formatID)
		}
		if _, ok := CandidateFormatByID(formatID); !ok {
			t.Fatalf("localUnitCounters[%q] has no candidate", formatID)
		}
	}
	for formatID, method := range expectedUnitBounds {
		if method == UnitBoundLocalExact && localUnitCounters[formatID] == nil {
			t.Fatalf("local exact format %q has no local counter", formatID)
		}
	}
}

func TestCountLocalUnitsLeavesUnprovedFormatsUnbounded(t *testing.T) {
	docx, ok := CandidateFormatByID("docx")
	if !ok {
		t.Fatal("docx candidate is missing")
	}
	units, err := countLocalUnits(docx, bytes.NewReader([]byte("synthetic")), 9)
	if err != nil {
		t.Fatal(err)
	}
	if units != 0 {
		t.Fatalf("countLocalUnits(docx) = %d, want 0 for unproved format", units)
	}
}

func TestCountPPTXSlides(t *testing.T) {
	tests := []struct {
		name      string
		archive   []byte
		wantUnits int
		wantError bool
	}{
		{
			name: "one slide",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}),
			wantUnits: 1,
		},
		{
			name: "multiple slides including hidden",
			archive: pptxArchive(t, []pptxTestSlide{
				{id: "256", relationshipID: "rId1", target: "slides/title-page.xml"},
				{id: "257", relationshipID: "rId2", target: "slides/hidden-slide.xml", hidden: true},
				{id: "258", relationshipID: "rId3", target: "slides/slide3.xml"},
			}),
			wantUnits: 3,
		},
		{
			name: "package rooted target",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "/ppt/slides/renamed-part.xml",
			}}),
			wantUnits: 1,
		},
		{
			name: "orphan and adjacent names do not count",
			archive: pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{
				"ppt/slides/slide99.xml":            "orphan",
				"ppt/slides/slide1.xml.bak":         "backup",
				"ppt/slides/_rels/slide1.xml.rels":  "relationship",
				"ppt/slideLayouts/slideLayout1.xml": "layout",
				"ppt/slideMasters/slideMaster1.xml": "master",
				"ppt/notesSlides/notesSlide1.xml":   "notes",
				"ppt/slides/slide2.xml":             "adjacent",
				"ppt/slides/slide1000.xml":          "unrelated numeric name",
			}),
			wantUnits: 1,
		},
		{
			name: "nonnumeric target",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/final-deck.xml",
			}}),
			wantUnits: 1,
		},
		{
			name:      "empty slide list",
			archive:   pptxArchiveWithSlideXML(t, `<p:presentation xmlns:p="`+pptxPresentationNamespace+`" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst/></p:presentation>`, validPPTXRelationships(), validPPTXContentTypes()),
			wantError: true,
		},
		{
			name:      "malformed presentation XML",
			archive:   pptxArchiveWithSlideXML(t, `<p:presentation xmlns:p="`+pptxPresentationNamespace+`"><p:sldIdLst>`, validPPTXRelationships(), validPPTXContentTypes()),
			wantError: true,
		},
		{
			name:      "leading XML text",
			archive:   pptxArchiveWithSlideXML(t, "outside"+validPPTXPresentation(), validPPTXRelationships(), validPPTXContentTypes()),
			wantError: true,
		},
		{
			name: "missing presentation",
			archive: documentZIP(t, map[string]string{
				pptxPresentationRelsPath: validPPTXRelationships(), ooxmlContentTypesName: validPPTXContentTypes(),
			}),
			wantError: true,
		},
		{
			name: "missing relationships",
			archive: documentZIP(t, map[string]string{
				pptxPresentationPath: validPPTXPresentation(), ooxmlContentTypesName: validPPTXContentTypes(),
			}),
			wantError: true,
		},
		{
			name: "missing content types",
			archive: documentZIP(t, map[string]string{
				pptxPresentationPath: validPPTXPresentation(), pptxPresentationRelsPath: validPPTXRelationships(),
			}),
			wantError: true,
		},
		{
			name:      "missing relationship",
			archive:   pptxArchiveWithSlideXML(t, validPPTXPresentation(), `<Relationships xmlns="`+pptxRelationshipNamespace+`"/>`, validPPTXContentTypes()),
			wantError: true,
		},
		{
			name: "external relationship mode",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "https://example.invalid/slide.xml", targetMode: "External",
			}}),
			wantError: true,
		},
		{
			name: "external relationship target",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "https://example.invalid/slide.xml",
			}}),
			wantError: true,
		},
		{
			name: "duplicate relationship ID",
			archive: pptxArchive(t, []pptxTestSlide{
				{id: "256", relationshipID: "rId1", target: "slides/slide1.xml"},
				{id: "257", relationshipID: "rId1", target: "slides/slide2.xml"},
			}),
			wantError: true,
		},
		{
			name: "duplicate slide ID",
			archive: pptxArchive(t, []pptxTestSlide{
				{id: "256", relationshipID: "rId1", target: "slides/slide1.xml"},
				{id: "256", relationshipID: "rId2", target: "slides/slide2.xml"},
			}),
			wantError: true,
		},
		{
			name: "escaping target",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "../../outside.xml",
			}}),
			wantError: true,
		},
		{
			name: "wrong content type",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml", contentType: "application/xml",
			}}),
			wantError: true,
		},
		{
			name:      "trailing XML",
			archive:   pptxArchiveWithSlideXML(t, validPPTXPresentation(), validPPTXRelationships(), validPPTXContentTypes()+`<Types/>`),
			wantError: true,
		},
		{name: "malformed ZIP", archive: []byte("not a ZIP"), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			units, err := countPPTXSlides(bytes.NewReader(test.archive), int64(len(test.archive)))
			if test.wantError {
				t.Logf("error=%v", err)
				if err == nil {
					t.Fatal("countPPTXSlides succeeded for invalid PPTX")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if units != test.wantUnits {
				t.Fatalf("countPPTXSlides() = %d, want %d", units, test.wantUnits)
			}
			t.Logf("count=%d", units)
		})
	}

	for name, input := range map[string]struct {
		reader io.ReaderAt
		size   int64
	}{
		"nil reader": {reader: nil, size: 1},
		"zero size":  {reader: bytes.NewReader([]byte("x")), size: 0},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := countPPTXSlides(input.reader, input.size)
			t.Logf("error=%v", err)
			if err == nil {
				t.Fatal("countPPTXSlides accepted invalid input")
			}
		})
	}
}

func TestCountPPTXSlidesAcceptsEscapedAndDefaultTargets(t *testing.T) {
	escaped := pptxArchive(t, []pptxTestSlide{{
		id: "256", relationshipID: "rId1", target: "slides/title%20page.xml",
	}})
	units, err := countPPTXSlides(bytes.NewReader(escaped), int64(len(escaped)))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, units)
	t.Logf("escaped_target count=%d", units)

	defaultContentTypes := "<Types xmlns=\"" + pptxContentTypesNamespace + "\">" +
		"<Default Extension=\"xml\" ContentType=\"" + pptxSlideContentType + "\"/>" +
		"<Override PartName=\"/ppt/presentation.xml\" " +
		"ContentType=\"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml\"/>" +
		"</Types>"
	defaultTarget := pptxArchiveWithSlideXML(
		t, validPPTXPresentation(), validPPTXRelationships(), defaultContentTypes,
	)
	units, err = countPPTXSlides(bytes.NewReader(defaultTarget), int64(len(defaultTarget)))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, units)
	t.Logf("default_content_type count=%d", units)
}

type pptxTestSlide struct {
	id             string
	relationshipID string
	target         string
	targetMode     string
	contentType    string
	hidden         bool
}

func pptxArchive(t *testing.T, slides []pptxTestSlide) []byte {
	t.Helper()
	return pptxArchiveWithEntries(t, slides, nil)
}

func pptxArchiveWithEntries(t *testing.T, slides []pptxTestSlide, extra map[string]string) []byte {
	t.Helper()
	presentation := strings.Builder{}
	presentation.WriteString(`<p:presentation xmlns:p="` + pptxPresentationNamespace + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>`)
	for _, slide := range slides {
		show := ""
		if slide.hidden {
			show = ` show="0"`
		}
		fmt.Fprintf(&presentation, `<p:sldId id="%s" r:id="%s"%s/>`, slide.id, slide.relationshipID, show)
	}
	presentation.WriteString(`</p:sldIdLst></p:presentation>`)

	relationships := strings.Builder{}
	relationships.WriteString(`<Relationships xmlns="` + pptxRelationshipNamespace + `">`)
	for _, slide := range slides {
		mode := ""
		if slide.targetMode != "" {
			mode = ` TargetMode="` + slide.targetMode + `"`
		}
		fmt.Fprintf(&relationships, `<Relationship Id="%s" Type="%s" Target="%s"%s/>`, slide.relationshipID, pptxRelationshipType, slide.target, mode)
	}
	relationships.WriteString(`</Relationships>`)

	contentTypes := strings.Builder{}
	contentTypes.WriteString(`<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>`)
	entries := map[string]string{
		pptxPresentationPath:     presentation.String(),
		pptxPresentationRelsPath: relationships.String(),
		ooxmlContentTypesName:    "",
	}
	for _, slide := range slides {
		contentType := slide.contentType
		if contentType == "" {
			contentType = pptxSlideContentType
		}
		target := pptxArchiveTargetName(slide.target)
		if target != "" {
			fmt.Fprintf(&contentTypes, `<Override PartName="/%s" ContentType="%s"/>`, target, contentType)
			entries[target] = `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`
		}
	}
	contentTypes.WriteString(`</Types>`)
	entries[ooxmlContentTypesName] = contentTypes.String()
	maps.Copy(entries, extra)
	return documentZIP(t, entries)
}

func pptxArchiveTargetName(target string) string {
	if strings.HasPrefix(target, "/") {
		return strings.TrimPrefix(target, "/")
	}
	return path.Clean(path.Join(path.Dir(pptxPresentationPath), target))
}

func pptxArchiveWithSlideXML(t *testing.T, presentation, relationships, contentTypes string) []byte {
	t.Helper()
	return documentZIP(t, map[string]string{
		pptxPresentationPath:     presentation,
		pptxPresentationRelsPath: relationships,
		ooxmlContentTypesName:    contentTypes,
		"ppt/slides/slide1.xml":  `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`,
	})
}

func validPPTXPresentation() string {
	return `<p:presentation xmlns:p="` + pptxPresentationNamespace + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`
}

func validPPTXRelationships() string {
	return `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxRelationshipType + `" Target="slides/slide1.xml"/></Relationships>`
}

func validPPTXContentTypes() string {
	return `<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="` + pptxSlideContentType + `"/></Types>`
}
