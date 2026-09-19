package renderpdf

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmissionAllowsLocalLinksFormulasAndRasterData(t *testing.T) {
	data := flatODF(FlatTextKind, `<office:body xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:xlink="http://www.w3.org/1999/xlink"><text:p xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><xlink:a xlink:href="#local"/><draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xlink:href="data:image/png;base64,AA=="/></text:p><text:p><f>SUM([.A1:.A2])</f></text:p></office:body>`)
	admission, err := Scan(data, FlatTextKind, DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, FlatTextKind, admission.Kind)
	assert.Positive(t, admission.Elements)
}

func TestAdmissionMatchesFlatKindMimeType(t *testing.T) {
	tests := []struct {
		kind string
		mime string
	}{
		{kind: FlatTextKind, mime: "application/vnd.oasis.opendocument.text"},
		{kind: FlatPresKind, mime: "application/vnd.oasis.opendocument.presentation"},
		{kind: FlatCalcKind, mime: "application/vnd.oasis.opendocument.spreadsheet"},
	}
	for _, testCase := range tests {
		t.Run(testCase.kind, func(t *testing.T) {
			admission, err := Scan(flatODF(testCase.kind, `<office:body/>`), testCase.kind, DefaultLimits())
			require.NoError(t, err)
			assert.Equal(t, testCase.kind, admission.Kind)
			for _, other := range tests {
				if other.kind == testCase.kind {
					continue
				}
				_, err := Scan(flatODF(other.kind, `<office:body/>`), testCase.kind, DefaultLimits())
				require.Error(t, err)
			}
		})
	}
}

func TestAdmissionAllowsLibreOfficeMetadata(t *testing.T) {
	textMetadata := `<office:scripts xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:script="urn:oasis:names:tc:opendocument:xmlns:script:1.0" xmlns:ooo="http://openoffice.org/2004/office"><office:script script:language="ooo:Basic"><ooo:libraries><ooo:library-embedded ooo:name="Standard"/></ooo:libraries></office:script></office:scripts>`
	_, err := Scan(flatODF(FlatTextKind, textMetadata), FlatTextKind, DefaultLimits())
	require.NoError(t, err)
	emptyTextMetadata := `<office:scripts xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:script="urn:oasis:names:tc:opendocument:xmlns:script:1.0" xmlns:ooo="http://openoffice.org/2004/office"><office:script script:language="ooo:Basic"><ooo:libraries/></office:script></office:scripts>`
	_, err = Scan(flatODF(FlatTextKind, emptyTextMetadata), FlatTextKind, DefaultLimits())
	require.NoError(t, err)

	presentationMetadata := `<presentation:placeholder xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0" presentation:object="handout"/><presentation:placeholder xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0" presentation:object="title"/><presentation:placeholder xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0" presentation:object="subtitle"/>`
	_, err = Scan(flatODF(FlatPresKind, presentationMetadata), FlatPresKind, DefaultLimits())
	require.NoError(t, err)
}

func TestAdmissionRejectsLinkedAndActiveConstructs(t *testing.T) {
	for _, kind := range []string{FlatTextKind, FlatPresKind, FlatCalcKind} {
		t.Run(kind, func(t *testing.T) {
			for _, testCase := range []struct {
				name string
				body string
			}{
				{name: "external href", body: `<draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="../outside.png"/>`},
				{name: "inherited XML Base fragment", body: `<text:p xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xml:base="https://example.test/links/"><xlink:a xlink:href="#local"/></text:p>`},
				{name: "script", body: `<office:script/>`},
				{name: "script code", body: `<office:script xmlns:script="urn:oasis:names:tc:opendocument:xmlns:script:1.0" script:language="ooo:Basic"><ooo:libraries xmlns:ooo="http://openoffice.org/2004/office"><ooo:library-embedded ooo:name="Standard"/></ooo:libraries><text:p xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0">code</text:p></office:script>`},
				{name: "event", body: `<office:event-listeners/>`},
				{name: "dde", body: `<office:dde-link/>`},
				{name: "database", body: `<table:database-range xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"/>`},
				{name: "section", body: `<text:section-source xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"/>`},
				{name: "ole", body: `<draw:object-ole xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
				{name: "chart", body: `<chart:chart xmlns:chart="urn:oasis:names:tc:opendocument:xmlns:chart:1.0"/>`},
				{name: "presentation object", body: `<presentation:placeholder xmlns:presentation="urn:oasis:names:tc:opendocument:xmlns:presentation:1.0" presentation:object="slide"/>`},
				{name: "plugin", body: `<draw:plugin xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
				{name: "applet", body: `<draw:applet xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
				{name: "nested document", body: `<office:embedded-document/>`},
				{name: "webservice element text", body: `<f>WEBSERVICE("https://example.test")</f>`},
				{name: "external formula", body: `<table:table-cell xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" table:formula="of:=DDE(\"https://example.test\")"/>`},
				{name: "SVG data", body: `<draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="data:image/svg+xml;base64,PHN2Zy8+"/>`},
				{name: "unknown image data", body: `<draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="data:image/tiff;base64,AA=="/>`},
			} {
				t.Run(testCase.name, func(t *testing.T) {
					_, err := Scan(flatODF(kind, testCase.body), kind, DefaultLimits())
					require.Error(t, err)
				})
			}
		})
	}
}

func TestAdmissionEnforcesBoundsAndKind(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxXMLDepth = 1
	_, err := Scan(flatODF(FlatTextKind, `<office:body/>`), FlatTextKind, limits)
	require.ErrorContains(t, err, "depth")
	limits = DefaultLimits()
	limits.MaxXMLElements = 1
	_, err = Scan(flatODF(FlatTextKind, `<office:body/>`), FlatTextKind, limits)
	require.ErrorContains(t, err, "element")
	_, err = Scan([]byte(strings.Repeat("x", 8)), FlatTextKind, DefaultLimits())
	require.Error(t, err)
}
