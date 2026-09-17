package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

const pptxMarkupCompatibilityNS = "http://schemas.openxmlformats.org/markup-compatibility/2006"

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
		if method.usesLocalCounter() && localUnitCounters[formatID] == nil {
			t.Fatalf("local counted format %q has no local counter", formatID)
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
				"ppt/slides/_rels/slide1.xml.rels":  `<Relationships xmlns="` + pptxRelationshipNamespace + `"/>`,
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
			name: "case-insensitive part names",
			archive: pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/SLIDE1.XML",
				entryName: "ppt/slides/slide1.xml", contentTypeName: "/ppt/slides/slide1.xml", contentType: strings.ToUpper(pptxSlideContentType),
			}}, nil),
			wantUnits: 1,
		},
		{
			name:      "alternate slide-list content",
			archive:   pptxArchiveWithSlideXML(t, `<p:presentation xmlns:p="`+pptxPresentationNamespace+`" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships" xmlns:mc="`+pptxMarkupCompatibilityNS+`"><p:sldIdLst><p:sldId id="256" r:id="rId1"/><mc:AlternateContent><mc:Choice Requires="p"><p:sldId id="257" r:id="rId2"/></mc:Choice><mc:Fallback><p:sldId id="258" r:id="rId3"/></mc:Fallback></mc:AlternateContent></p:sldIdLst></p:presentation>`, validPPTXRelationships(), validPPTXContentTypes()),
			wantError: true,
		},
		{
			name: "alternate content in extension list",
			archive: pptxArchiveWithSlideXML(t, strings.Replace(validPPTXPresentation(), `</p:presentation>`,
				`<p:extLst><p:ext uri="urn:example:extension"><mc:AlternateContent xmlns:mc="`+pptxMarkupCompatibilityNS+`"><mc:Choice Requires="p"/><mc:Fallback/></mc:AlternateContent></p:ext></p:extLst></p:presentation>`, 1),
				validPPTXRelationships(), validPPTXContentTypes()),
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
			name: "root relationship selects another presentation",
			archive: pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{
				pptxRootRelationshipsPath: `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxOfficeDocumentRelType + `" Target="ppt/other.xml"/></Relationships>`,
			}),
			wantError: true,
		},
		{
			name: "root relationship is missing",
			archive: documentZIP(t, map[string]string{
				pptxPresentationPath: validPPTXPresentation(), pptxPresentationRelsPath: validPPTXRelationships(), ooxmlContentTypesName: validPPTXContentTypes(),
			}),
			wantError: true,
		},
		{
			name: "external root relationship mode",
			archive: pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{
				pptxRootRelationshipsPath: strings.Replace(validPPTXRootRelationships(), `Target="`, `TargetMode="External" Target="`, 1),
			}),
			wantError: true,
		},
		{
			name:      "UTF-8 BOM in presentation and relationships",
			archive:   pptxArchiveWithSlideXML(t, string([]byte{0xef, 0xbb, 0xbf})+validPPTXPresentation(), string([]byte{0xef, 0xbb, 0xbf})+validPPTXRelationships(), validPPTXContentTypes()),
			wantUnits: 1,
		},
		{
			name:      "missing relationship",
			archive:   pptxArchiveWithSlideXML(t, validPPTXPresentation(), `<Relationships xmlns="`+pptxRelationshipNamespace+`"/>`, validPPTXContentTypes()),
			wantError: true,
		},
		{
			name: "external relationship mode",
			archive: pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml", targetMode: "External",
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
			if err == nil {
				t.Fatal("countPPTXSlides accepted invalid input")
			}
		})
	}
}

func TestCountPPTXSlidesAcceptsCompleteStrictPackage(t *testing.T) {
	archive := pptxArchiveWithFamily(t, pptxNamespaceFamilyStrict, []pptxTestSlide{{
		id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
	}}, nil)

	units, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
	require.NoError(t, err)
	assert.Equal(t, 1, units)
}

func TestCountPPTXSlidesAcceptsNamespaceDeclarationsNamedLikeAttributes(t *testing.T) {
	for _, family := range []pptxNamespaceFamily{pptxNamespaceFamilyTransitional, pptxNamespaceFamilyStrict} {
		t.Run(fmt.Sprintf("family-%d", family), func(t *testing.T) {
			presentationNamespace := pptxPresentationNamespaces.value(family)
			relationshipIDNamespace := pptxRelationshipIDNamespaces.value(family)
			relationshipType := pptxSlideRelationshipTypes.value(family)
			presentation := `<p:presentation xmlns:p="` + presentationNamespace + `"><p:sldIdLst><p:sldId xmlns:id="` + relationshipIDNamespace + `" id="256" id:id="rId1"/></p:sldIdLst></p:presentation>`
			relationships := `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship xmlns:Type="urn:example:namespace" Id="rId1" Type="` + relationshipType + `" Target="slides/slide1.xml"/></Relationships>`
			archive := documentZIP(t, map[string]string{
				pptxPresentationPath:      presentation,
				pptxPresentationRelsPath:  relationships,
				pptxRootRelationshipsPath: validPPTXRootRelationshipsForFamily(family),
				ooxmlContentTypesName:     validPPTXContentTypes(),
				"ppt/slides/slide1.xml":   `<p:sld xmlns:p="` + presentationNamespace + `"/>`,
			})
			units, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.NoError(t, err)
			assert.Equal(t, 1, units)
		})
	}
}

func TestCountPPTXSlidesRejectsMixedAndUnknownNamespaceFamilies(t *testing.T) {
	strictPresentation := pptxPresentationNamespaces.value(pptxNamespaceFamilyStrict)
	transitionalPresentation := pptxPresentationNamespaces.value(pptxNamespaceFamilyTransitional)
	strictRelationshipID := pptxRelationshipIDNamespaces.value(pptxNamespaceFamilyStrict)
	transitionalRelationshipID := pptxRelationshipIDNamespaces.value(pptxNamespaceFamilyTransitional)
	transitionalOfficeDocument := pptxOfficeDocumentRelationshipTypes.value(pptxNamespaceFamilyTransitional)
	strictSlide := pptxSlideRelationshipTypes.value(pptxNamespaceFamilyStrict)
	transitionalSlide := pptxSlideRelationshipTypes.value(pptxNamespaceFamilyTransitional)
	slides := []pptxTestSlide{{id: "256", relationshipID: "rId1", target: "slides/slide1.xml"}}

	tests := []struct {
		name   string
		family pptxNamespaceFamily
		extra  map[string]string
	}{
		{
			name:   "presentation child uses transitional namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationPath: `<p:presentation xmlns:p="` + strictPresentation + `" xmlns:r="` + strictRelationshipID + `"><p:sldIdLst xmlns:p="` + transitionalPresentation + `"><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`,
			},
		},
		{
			name:   "presentation child uses strict namespace",
			family: pptxNamespaceFamilyTransitional,
			extra: map[string]string{
				pptxPresentationPath: `<p:presentation xmlns:p="` + transitionalPresentation + `" xmlns:r="` + transitionalRelationshipID + `"><p:sldIdLst xmlns:p="` + strictPresentation + `"><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`,
			},
		},
		{
			name:   "presentation relationship ID uses transitional namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationPath: `<p:presentation xmlns:p="` + strictPresentation + `" xmlns:r="` + strictRelationshipID + `"><p:sldIdLst><p:sldId id="256" xmlns:r="` + transitionalRelationshipID + `" r:id="rId1"/></p:sldIdLst></p:presentation>`,
			},
		},
		{
			name:   "relationship child uses unknown namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationRelsPath: `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship xmlns="urn:example:unknown" Id="rId1" Type="` + strictSlide + `" Target="slides/slide1.xml"/></Relationships>`,
			},
		},
		{
			name:   "relationship root uses unknown namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationRelsPath: `<Relationships xmlns="urn:example:unknown"><Relationship Id="rId1" Type="` + strictSlide + `" Target="slides/slide1.xml"/></Relationships>`,
			},
		},
		{
			name:   "slide relationship type uses transitional namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationRelsPath: `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + transitionalSlide + `" Target="slides/slide1.xml"/></Relationships>`,
			},
		},
		{
			name:   "office document relationship type uses transitional namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxRootRelationshipsPath: `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + transitionalOfficeDocument + `" Target="ppt/presentation.xml"/></Relationships>`,
			},
		},
		{
			name:   "unknown presentation namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationPath: `<p:presentation xmlns:p="urn:example:unknown" xmlns:r="` + strictRelationshipID + `"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`,
			},
		},
		{
			name:   "unknown relationship namespace",
			family: pptxNamespaceFamilyStrict,
			extra: map[string]string{
				pptxPresentationRelsPath: `<Relationships xmlns="urn:example:unknown"><Relationship Id="rId1" Type="` + strictSlide + `" Target="slides/slide1.xml"/></Relationships>`,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			archive := pptxArchiveWithFamily(t, test.family, slides, test.extra)
			_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.Error(t, err)
		})
	}
}

func TestCountPPTXSlidesAcceptsEscapedAndDefaultTargets(t *testing.T) {
	for _, test := range []struct {
		name, target, entryName, contentTypeName string
	}{
		{
			name:            "encoded target and decoded names",
			target:          "slides/title%20page.xml",
			entryName:       "ppt/slides/title page.xml",
			contentTypeName: "/ppt/slides/title page.xml",
		},
		{
			name:            "decoded target and encoded names",
			target:          "slides/title page.xml",
			entryName:       "ppt/slides/title%20page.xml",
			contentTypeName: "/ppt/slides/title%20page.xml",
		},
		{
			name:            "encoded separator",
			target:          "slides%2Ftitle%20page.xml",
			entryName:       "ppt/slides/title page.xml",
			contentTypeName: "/ppt/slides/title page.xml",
		},
		{
			name:            "encoded hash",
			target:          "slides/title%23page.xml",
			entryName:       "ppt/slides/title#page.xml",
			contentTypeName: "/ppt/slides/title%23page.xml",
		},
		{
			name:            "encoded question mark",
			target:          "slides/title%3Fpage.xml",
			entryName:       "ppt/slides/title?page.xml",
			contentTypeName: "/ppt/slides/title%3Fpage.xml",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			escaped := pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: test.target,
				entryName: test.entryName, contentTypeName: test.contentTypeName,
			}})
			units, err := countPPTXSlides(bytes.NewReader(escaped), int64(len(escaped)))
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, 1, units)
		})
	}

	for _, test := range []struct {
		name, target, entryName, contentTypeName string
	}{
		{
			name:            "raw dot segment",
			target:          "slides/./slide1.xml",
			entryName:       "ppt/slides/slide1.xml",
			contentTypeName: "/ppt/slides/slide1.xml",
		},
		{
			name:            "encoded dot segment",
			target:          "slides/%2E/slide1.xml",
			entryName:       "ppt/slides/slide1.xml",
			contentTypeName: "/ppt/slides/slide1.xml",
		},
		{
			name:            "raw parent segment",
			target:          "slides/../slides/slide1.xml",
			entryName:       "ppt/slides/slide1.xml",
			contentTypeName: "/ppt/slides/slide1.xml",
		},
		{
			name:            "encoded parent segment",
			target:          "slides/%2E%2E/slide1.xml",
			entryName:       "ppt/slide1.xml",
			contentTypeName: "/ppt/slide1.xml",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := pptxArchive(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: test.target,
				entryName: test.entryName, contentTypeName: test.contentTypeName,
			}})
			units, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.NoError(t, err)
			assert.Equal(t, 1, units)
		})
	}

	defaultContentTypes := "<Types xmlns=\"" + pptxContentTypesNamespace + "\">" +
		"<Default Extension=\"xml\" ContentType=\"" + strings.ToUpper(pptxSlideContentType) + "\"/>" +
		"<Override PartName=\"/ppt/presentation.xml\" " +
		"ContentType=\"application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml\"/>" +
		"</Types>"
	defaultTarget := pptxArchiveWithSlideXML(
		t, validPPTXPresentation(), validPPTXRelationships(), defaultContentTypes,
	)
	defaultUnits, err := countPPTXSlides(bytes.NewReader(defaultTarget), int64(len(defaultTarget)))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, 1, defaultUnits)
}

func TestCountPPTXSlidesRejectsAmbiguousPackageAliases(t *testing.T) {
	t.Run("ZIP entries", func(t *testing.T) {
		archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
			id: "256", relationshipID: "rId1", target: "slides/title%20page.xml",
			entryName: "ppt/slides/title page.xml", contentTypeName: "/ppt/slides/title page.xml",
		}}, map[string]string{
			"ppt/slides/title%20page.xml": `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`,
		})
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "ambiguous equivalent entries")
	})

	t.Run("content type declarations", func(t *testing.T) {
		contentTypes := `<Types xmlns="` + pptxContentTypesNamespace + `">` +
			`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>` +
			`<Override PartName="/ppt/slides/slide1.xml" ContentType="` + pptxSlideContentType + `"/>` +
			`<Override PartName="/ppt/slides/slide%31.xml" ContentType="` + pptxSlideContentType + `"/>` +
			`</Types>`
		archive := pptxArchiveWithSlideXML(t, validPPTXPresentation(), validPPTXRelationships(), contentTypes)
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "content type")
		require.ErrorContains(t, err, "duplicated")
	})

	t.Run("slide targets resolving to one ZIP entry", func(t *testing.T) {
		presentation := `<p:presentation xmlns:p="` + pptxPresentationNamespace + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/><p:sldId id="257" r:id="rId2"/></p:sldIdLst></p:presentation>`
		relationships := `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxRelationshipType + `" Target="slides/title%20page.xml"/><Relationship Id="rId2" Type="` + pptxRelationshipType + `" Target="slides/title%2520page.xml"/></Relationships>`
		contentTypes := `<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/title%20page.xml" ContentType="` + pptxSlideContentType + `"/></Types>`
		archive := documentZIP(t, map[string]string{
			pptxPresentationPath:          presentation,
			pptxPresentationRelsPath:      relationships,
			pptxRootRelationshipsPath:     validPPTXRootRelationships(),
			ooxmlContentTypesName:         contentTypes,
			"ppt/slides/title%20page.xml": `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`,
		})
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "duplicated")
	})

	t.Run("content type aliases with equal values remain ambiguous", func(t *testing.T) {
		contentTypes := `<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/title%2520page.xml" ContentType="` + pptxSlideContentType + `"/><Override PartName="/ppt/slides/title page.xml" ContentType="` + pptxSlideContentType + `"/></Types>`
		archive := documentZIP(t, map[string]string{
			pptxPresentationPath:        validPPTXPresentation(),
			pptxPresentationRelsPath:    validPPTXRelationshipsWithTarget("slides/title%20page.xml"),
			pptxRootRelationshipsPath:   validPPTXRootRelationships(),
			ooxmlContentTypesName:       contentTypes,
			"ppt/slides/title page.xml": `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`,
		})
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "ambiguous declarations")
	})
}

func TestCountPPTXSlidesRejectsInvalidPackageAliases(t *testing.T) {
	t.Run("malformed ZIP entry escape", func(t *testing.T) {
		archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
			id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
		}}, map[string]string{
			"ppt/slides/bad%ZZ.xml": "invalid",
		})
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "valid path")
	})

	t.Run("package-root ZIP entry escape", func(t *testing.T) {
		archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
			id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
		}}, map[string]string{
			"../outside.xml": "invalid",
		})
		_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
		require.ErrorContains(t, err, "escapes the package root")
	})

	for _, test := range []struct {
		name, partName, want string
	}{
		{name: "malformed content type escape", partName: "/ppt/slides/slide%ZZ.xml", want: "valid URI"},
		{name: "package-root content type escape", partName: "/ppt/../../slide.xml", want: "escapes the package root"},
	} {
		t.Run(test.name, func(t *testing.T) {
			contentTypes := `<Types xmlns="` + pptxContentTypesNamespace + `">` +
				`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>` +
				`<Override PartName="` + test.partName + `" ContentType="` + pptxSlideContentType + `"/>` +
				`</Types>`
			archive := pptxArchiveWithSlideXML(t, validPPTXPresentation(), validPPTXRelationships(), contentTypes)
			_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestCountPPTXSlidesRejectsEncodedSeparatorsInRawPartNames(t *testing.T) {
	for _, partName := range []string{
		"ppt/slides/_rels/slide%2F1.xml.rels",
		"ppt/slides/_rels/slide%2f1.xml.rels",
		"ppt/slides/_rels/slide%5C1.xml.rels",
		"ppt/slides/_rels/slide%5c1.xml.rels",
	} {
		t.Run(partName, func(t *testing.T) {
			relationships := `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId2" Type="http://example.test/image" Target="https://example.invalid/image.png"/></Relationships>`
			archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{partName: relationships})
			_, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.ErrorContains(t, err, "encoded path separator")
		})
	}
}

func TestCountPPTXSlidesCanonicalizesRootTarget(t *testing.T) {
	for _, target := range []string{
		"ppt/./presentation.xml",
		"/ppt/%70resentation.xml",
		"%2Fppt%2Fpresentation.xml",
	} {
		t.Run(target, func(t *testing.T) {
			root := `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` +
				pptxOfficeDocumentRelType + `" Target="` + target + `"/></Relationships>`
			archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{pptxRootRelationshipsPath: root})
			units, err := countPPTXSlides(bytes.NewReader(archive), int64(len(archive)))
			require.NoError(t, err)
			assert.Equal(t, 1, units)
		})
	}
}

func TestRenditionClientRejectsMalformedAndExternalPPTXBeforeHTTP(t *testing.T) {
	policy := testPolicy(t, 1<<20, 10)
	manifest := syntheticManifest(t, policy, true)
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "pptx" {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundLocalExact
			manifest.Results[index].UnitCount = 1
			manifest.Results[index].UnitsProcessed = 1
			manifest.Results[index].LocalUnits = 1
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	descriptor := renditionDescriptor(t, policy, manifest, "pptx")
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	client := renditionServerClient(t, policy, manifest, descriptor, server)

	tests := []struct {
		name, relationship, entryName string
	}{
		{
			name: "malformed presentation relationship",
			relationship: `<Relationships xmlns="` + pptxRelationshipNamespace +
				`"><Relationship Id="rId2"`,
		},
		{
			name: "external slide relationship",
			relationship: `<Relationships xmlns="` + pptxRelationshipNamespace +
				`"><Relationship Id="rId1" Type="` + pptxRelationshipType + `" Target="slides/slide1.xml" TargetMode="External"/></Relationships>`,
		},
		{
			name: "encoded relationship-part separator",
			relationship: `<Relationships xmlns="` + pptxRelationshipNamespace +
				`"><Relationship Id="rId2" Type="http://example.test/image" Target="https://example.invalid/image.png"/></Relationships>`,
			entryName: "ppt/slides/_rels/slide%2F1.xml.rels",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entryName := test.entryName
			if entryName == "" {
				entryName = pptxPresentationRelsPath
			}
			archive := pptxArchiveWithEntries(t, []pptxTestSlide{{
				id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
			}}, map[string]string{
				entryName: test.relationship,
			})
			fixture := pptxRenditionFixture(t, descriptor, archive)
			_, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
			assertRenditionCode(t, err, document.RenditionErrorUnsupportedInput)
			assert.Zero(t, requests.Load())
		})
	}
}

func TestRenditionClientAcceptsStrictPPTXWithHyperlink(t *testing.T) {
	policy := testPolicy(t, 1<<20, 3)
	manifest := syntheticManifest(t, policy, true)
	for index := range manifest.Results {
		if manifest.Results[index].FormatID == "pptx" {
			manifest.Results[index].ReasonCode = ""
			manifest.Results[index].UnitBoundMethod = UnitBoundLocalExact
			manifest.Results[index].UnitCount = 1
			manifest.Results[index].UnitsProcessed = 1
			manifest.Results[index].LocalUnits = 1
		}
	}
	require.NoError(t, manifest.ValidateComplete())
	descriptor := renditionDescriptor(t, policy, manifest, "pptx")
	source := pptxArchiveWithFamily(t, pptxNamespaceFamilyStrict, []pptxTestSlide{{
		id: "256", relationshipID: "rId1", target: "slides/slide1.xml",
	}}, map[string]string{
		"ppt/slides/_rels/slide1.xml.rels":  `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxStrictRelationshipIDNS + `/slideLayout" Target="../slideLayouts/slideLayout1.xml"/><Relationship Id="rId2" Type="` + pptxStrictRelationshipIDNS + `/hyperlink" Target="https://example.com/" TargetMode="External"/></Relationships>`,
		"ppt/slideLayouts/slideLayout1.xml": `<p:sldLayout xmlns:p="` + pptxStrictPresentationNS + `"/>`,
	})
	fixture := pptxRenditionFixture(t, descriptor, source)
	var requests atomic.Int64
	response := fmt.Sprintf(`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"strict"}],"usage_info":{"pages_processed":1,"doc_size_bytes":%d}}`, len(source))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	client := renditionServerClient(t, policy, manifest, descriptor, server)

	result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, int64(1), requests.Load())
	assert.Len(t, result.Evidence.Units, 1)
}

func TestPrepareAcceptsBOMPPTX(t *testing.T) {
	bom := string([]byte{0xef, 0xbb, 0xbf})
	source := pptxArchiveWithSlideXML(t, bom+validPPTXPresentation(), bom+validPPTXRelationships(), bom+validPPTXContentTypes())
	policy := testPolicy(t, 1<<20, 10)
	candidate, found := CandidateFormatByID("pptx")
	require.True(t, found)
	digest := sha256.Sum256(source)
	spool := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spool)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(source)), policy, PrepareOptions{
		Directory: spool, DeclaredMediaType: candidate.MediaType,
		ExpectedSize: int64(len(source)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: 1 << 20, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })
	assert.Equal(t, 1, prepared.localUnits)
}

type pptxTestSlide struct {
	id              string
	relationshipID  string
	target          string
	entryName       string
	contentTypeName string
	targetMode      string
	contentType     string
	hidden          bool
}

func pptxArchive(t *testing.T, slides []pptxTestSlide) []byte {
	t.Helper()
	return pptxArchiveWithEntries(t, slides, nil)
}

func pptxArchiveWithEntries(t *testing.T, slides []pptxTestSlide, extra map[string]string) []byte {
	t.Helper()
	return pptxArchiveWithFamily(t, pptxNamespaceFamilyTransitional, slides, extra)
}

func pptxArchiveWithFamily(t *testing.T, family pptxNamespaceFamily, slides []pptxTestSlide, extra map[string]string) []byte {
	t.Helper()
	presentationNamespace := pptxPresentationNamespaces.value(family)
	relationshipIDNamespace := pptxRelationshipIDNamespaces.value(family)
	slideRelationshipType := pptxSlideRelationshipTypes.value(family)
	presentation := strings.Builder{}
	presentation.WriteString(`<p:presentation xmlns:p="` + presentationNamespace + `" xmlns:r="` + relationshipIDNamespace + `"><p:sldIdLst>`)
	for _, slide := range slides {
		fmt.Fprintf(&presentation, `<p:sldId id="%s" r:id="%s"/>`, slide.id, slide.relationshipID)
	}
	presentation.WriteString(`</p:sldIdLst></p:presentation>`)

	relationships := strings.Builder{}
	relationships.WriteString(`<Relationships xmlns="` + pptxRelationshipNamespace + `">`)
	for _, slide := range slides {
		mode := ""
		if slide.targetMode != "" {
			mode = ` TargetMode="` + slide.targetMode + `"`
		}
		fmt.Fprintf(&relationships, `<Relationship Id="%s" Type="%s" Target="%s"%s/>`, slide.relationshipID, slideRelationshipType, slide.target, mode)
	}
	relationships.WriteString(`</Relationships>`)

	contentTypes := strings.Builder{}
	contentTypes.WriteString(`<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>`)
	entries := map[string]string{
		pptxPresentationPath:      presentation.String(),
		pptxPresentationRelsPath:  relationships.String(),
		pptxRootRelationshipsPath: validPPTXRootRelationshipsForFamily(family),
		ooxmlContentTypesName:     "",
	}
	for _, slide := range slides {
		contentType := slide.contentType
		if contentType == "" {
			contentType = pptxSlideContentType
		}
		target := pptxArchiveTargetName(slide.target)
		if target != "" {
			contentTypeName := "/" + target
			if slide.contentTypeName != "" {
				contentTypeName = slide.contentTypeName
			}
			fmt.Fprintf(&contentTypes, `<Override PartName="%s" ContentType="%s"/>`, contentTypeName, contentType)
			entryName := target
			if slide.entryName != "" {
				entryName = slide.entryName
			}
			show := ""
			if slide.hidden {
				show = ` show="0"`
			}
			entries[entryName] = `<p:sld xmlns:p="` + presentationNamespace + `"` + show + `/>`
		}
	}
	contentTypes.WriteString(`</Types>`)
	entries[ooxmlContentTypesName] = contentTypes.String()
	maps.Copy(entries, extra)
	return documentZIP(t, entries)
}

func pptxArchiveTargetName(target string) string {
	if target, found := strings.CutPrefix(target, "/"); found {
		return target
	}
	return path.Clean(path.Join(path.Dir(pptxPresentationPath), target))
}

func pptxArchiveWithSlideXML(t *testing.T, presentation, relationships, contentTypes string) []byte {
	t.Helper()
	return documentZIP(t, map[string]string{
		pptxPresentationPath:      presentation,
		pptxPresentationRelsPath:  relationships,
		pptxRootRelationshipsPath: validPPTXRootRelationships(),
		ooxmlContentTypesName:     contentTypes,
		"ppt/slides/slide1.xml":   `<p:sld xmlns:p="` + pptxPresentationNamespace + `"/>`,
	})
}

func validPPTXPresentation() string {
	return `<p:presentation xmlns:p="` + pptxPresentationNamespace + `" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/></p:sldIdLst></p:presentation>`
}

func validPPTXRelationships() string {
	return `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxRelationshipType + `" Target="slides/slide1.xml"/></Relationships>`
}

func validPPTXRelationshipsWithTarget(target string) string {
	return `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxRelationshipType + `" Target="` + target + `"/></Relationships>`
}

func validPPTXRootRelationships() string {
	return validPPTXRootRelationshipsForFamily(pptxNamespaceFamilyTransitional)
}

func validPPTXRootRelationshipsForFamily(family pptxNamespaceFamily) string {
	return `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rId1" Type="` + pptxOfficeDocumentRelationshipTypes.value(family) + `" Target="ppt/presentation.xml"/></Relationships>`
}

func validPPTXContentTypes() string {
	return `<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/><Override PartName="/ppt/slides/slide1.xml" ContentType="` + pptxSlideContentType + `"/></Types>`
}
