package renderpdf

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdmissionAllowsLocalLinksFormulasAndRasterData(t *testing.T) {
	data := flatODF(FlatSpreadsheetKind, `<table:table xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" table:formula="of:=SUM([.A1:.A2])"><text:p xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"><xlink:a xlink:href="#sheet1"/><draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xlink:href="data:image/png;base64,AA=="/></text:p></table:table>`)
	admission, err := Scan(data, FlatSpreadsheetKind, DefaultLimits())
	require.NoError(t, err)
	assert.Equal(t, FlatSpreadsheetKind, admission.Kind)
	assert.Positive(t, admission.Elements)
}

func TestAdmissionRejectsLinkedAndActiveConstructs(t *testing.T) {
	for _, testCase := range []struct {
		name string
		body string
	}{
		{name: "external href", body: `<draw:image xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0" xmlns:xlink="http://www.w3.org/1999/xlink" xlink:href="../outside.png"/>`},
		{name: "script", body: `<office:script/>`},
		{name: "event", body: `<office:event-listeners/>`},
		{name: "dde", body: `<office:dde-link/>`},
		{name: "database", body: `<table:database-range xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"/>`},
		{name: "section", body: `<text:section-source xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0"/>`},
		{name: "ole", body: `<draw:object-ole xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
		{name: "plugin", body: `<draw:plugin xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
		{name: "applet", body: `<draw:applet xmlns:draw="urn:oasis:names:tc:opendocument:xmlns:drawing:1.0"/>`},
		{name: "nested document", body: `<office:embedded-document/>`},
		{name: "webservice element text", body: `<f>WEBSERVICE("https://example.test")</f>`},
		{name: "external formula", body: `<table:table-cell xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" table:formula="of:=DDE(\"https://example.test\")"/>`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Scan(flatODF(FlatTextKind, testCase.body), FlatTextKind, DefaultLimits())
			require.Error(t, err)
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
	_, err = Scan(flatODF(FlatTextKind, `<office:body/>`), FlatSpreadsheetKind, DefaultLimits())
	require.Error(t, err)
	_, err = Scan([]byte(strings.Repeat("x", 8)), FlatTextKind, DefaultLimits())
	require.Error(t, err)
}
