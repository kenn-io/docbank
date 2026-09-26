package document

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenditionXHTMLSemantics(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"named entities", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "http://www.w3.org/TR/xhtml11/DTD/xhtml11.dtd"><body><p>a&nbsp;b&mdash;c&hellip;</p></body>`, "a b—c…"},
		{"doctype comment", `<!DOCTYPE html <!-- <nested> [ ] -->><body>text</body>`, "text"},
		{"head", `<head><title>secret metadata</title><script>hidden</script></head><body><p>needle</p></body>`, "needle"},
		{"self closing active", `<body><script/><style/><iframe/><p>following text</p></body>`, "following text"},
		{"active", `<body><script>secret</script><svg><text>hidden</text></svg><p>visible</p></body>`, "visible"},
		{"structure", `<body><h1>Title</h1><ul><li>one</li><li>two</li></ul><p><a href="https://example.com">link</a> <code>x</code></p></body>`, "# Title\n- one\n- two\n\n[link](<https://example.com>) `x`"},
		{"table", `<body><table><tr><th>A</th><th>B</th></tr><tr><td>one</td><td>two</td></tr></table></body>`, "| A | B |\n| --- | --- |\n| one | two |"},
		{"empty", `<head><title>hidden</title></head><body/>`, ""},
		{"unicode", "<body><p>e\u0301</p></body>", "é"},
		{"newlines", "<body><pre>a\r\nb\rc</pre></body>", "```\na\nb\nc\n```"},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, err := RenditionMarkdownFromXHTML([]byte("\ufeff<html xmlns=\"http://www.w3.org/1999/xhtml\">"+test.body+"</html>"), 10000)
			require.NoError(t, err)
			require.Equal(t, test.want, text)
		})
	}
}

func TestRenditionXHTMLRejectsMalformedAndBudgetOverflow(t *testing.T) {
	for _, body := range []string{
		`<html xmlns="http://www.w3.org/1999/xhtml"><body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"/><html xmlns="http://www.w3.org/1999/xhtml"/>`,
		`<?xml version="1.0" encoding="iso-8859-1"?><html xmlns="http://www.w3.org/1999/xhtml"/>`,
		`<!DOCTYPE html [<!ENTITY x SYSTEM "https://example.com/secret">]><html xmlns="http://www.w3.org/1999/xhtml">&x;</html>`,
		string([]byte{0xff}),
	} {
		text, err := RenditionMarkdownFromXHTML([]byte(body), 100)
		require.Error(t, err)
		require.Empty(t, text)
	}
	for _, test := range []struct {
		name, body string
		limit      int
	}{
		{"output", "<p>abc</p>", 2},
		{"zero", "<p>a</p>", 0},
		{"depth", strings.Repeat("<a>", 65) + "text" + strings.Repeat("</a>", 65), 10000},
		{"intermediate", strings.Repeat("<p><span>a</span><span>b</span></p>", 100), 0},
		{"table padding", "<table><tr>" + strings.Repeat("<td/>", 1000) + "</tr>" + strings.Repeat("<tr><td/></tr>", 1000) + "</table>", 10000},
	} {
		t.Run(test.name, func(t *testing.T) {
			text, err := RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>`+test.body+`</body></html>`), test.limit)
			require.ErrorIs(t, err, ErrRenditionXHTMLBudget)
			require.Empty(t, text)
		})
	}
	text, err := RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><p>abc</p></body></html>`), 3)
	require.NoError(t, err)
	require.Equal(t, "abc", text)
	text, err = RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><head><title>metadata</title></head><body/></html>`), 0)
	require.NoError(t, err)
	require.Empty(t, text)
	text, err = RenditionMarkdownFromXHTML([]byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><p>`+strings.Repeat("e<!--split-->&#x301;", 3840)+`</p></body></html>`), 3840)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("é", 3840), text)
}

func TestRenditionXHTMLWhitespaceAllocationBudget(t *testing.T) {
	source := []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><p>` + strings.Repeat("a ", 2<<20) + `</p></body></html>`)
	_, err := RenditionMarkdownFromXHTML(source, 16<<20)
	require.ErrorIs(t, err, ErrRenditionXHTMLBudget)
}

func TestRenditionXHTMLStructuralAllocationBudget(t *testing.T) {
	source := []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>` + strings.Repeat(`<ul></ul>`, 1<<20) + `</body></html>`)
	_, err := RenditionMarkdownFromXHTML(source, 16<<20)
	require.ErrorIs(t, err, ErrRenditionXHTMLBudget)
}

func TestRenditionXHTMLAttributeAllocationBudget(t *testing.T) {
	var source strings.Builder
	source.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml"`)
	for index := range maxRenditionXHTMLAttributes + 1 {
		source.WriteString(` a`)
		source.WriteString(strconv.Itoa(index))
		source.WriteString(`="x"`)
	}
	source.WriteString(`><body>text</body></html>`)

	_, err := RenditionMarkdownFromXHTML([]byte(source.String()), 16<<20)
	require.ErrorIs(t, err, ErrRenditionXHTMLBudget)
}

func TestRenditionXHTMLAttributePreflightHandlesProcessingInstructions(t *testing.T) {
	var source strings.Builder
	source.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml"><?p '?><body`)
	for index := range maxRenditionXHTMLAttributes + 1 {
		source.WriteString(` a`)
		source.WriteString(strconv.Itoa(index))
		source.WriteString(`="x"`)
	}
	source.WriteString(`>text</body></html>`)

	require.ErrorIs(t, checkRenditionXHTMLAttributeBound([]byte(source.String())), ErrRenditionXHTMLBudget)

	source.Reset()
	source.WriteString(`<!DOCTYPE html [<!ENTITY x "<!--">]><html xmlns="http://www.w3.org/1999/xhtml"><body`)
	for index := range maxRenditionXHTMLAttributes + 1 {
		source.WriteString(` a`)
		source.WriteString(strconv.Itoa(index))
		source.WriteString(`="x"`)
	}
	source.WriteString(`>text</body></html>`)
	require.Error(t, checkRenditionXHTMLAttributeBound([]byte(source.String())))

	source.Reset()
	source.WriteString(`<!"><html xmlns='http://www.w3.org/1999/xhtml'`)
	for index := range 4096 {
		source.WriteString(` a`)
		source.WriteString(strconv.Itoa(index))
		source.WriteString(`='x'`)
	}
	source.WriteString(`><body>text</body></html>`)
	_, err := RenditionMarkdownFromXHTML([]byte(source.String()), 16<<20)
	require.Error(t, err)
}

func TestRenditionXHTMLTablePreformattedAllocationBudget(t *testing.T) {
	source := []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body><table><tr><td><pre>` + strings.Repeat("a ", 16<<20) + `</pre></td></tr></table></body></html>`)
	_, err := RenditionMarkdownFromXHTML(source, 16<<20)
	require.ErrorIs(t, err, ErrRenditionXHTMLBudget)
}

func TestRenditionXHTMLRejectsNestedMarkupInDoctype(t *testing.T) {
	const body = `<html xmlns="http://www.w3.org/1999/xhtml"><body>text</body></html>`
	for _, prefix := range []string{`<!DOCTYPE a <x> <? >>`, `<!DOCTYPE html <x> [ ]>`} {
		t.Run(prefix, func(t *testing.T) {
			text, err := RenditionMarkdownFromXHTML([]byte(prefix+body), 100)
			require.Error(t, err)
			require.Empty(t, text)
		})
	}
}

func TestRenditionXHTMLAttributePreflightRejectsDoctypeBypass(t *testing.T) {
	source := []byte(`<!DOCTYPE a <x> <? >><html xmlns="http://www.w3.org/1999/xhtml"` +
		strings.Repeat(` a="x"`, maxRenditionXHTMLAttributes+1) + `><body>text</body></html>`)
	require.Error(t, checkRenditionXHTMLAttributeBound(source))
}
