package document

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenditionXHTMLSemantics(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"named entities", `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "http://www.w3.org/TR/xhtml11/DTD/xhtml11.dtd"><body><p>a&nbsp;b&mdash;c&hellip;</p></body>`, "a b—c…"},
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

func TestRenditionXHTMLContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	text, err := RenditionMarkdownFromXHTMLContext(ctx, []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>text</body></html>`), 100)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, text)
}
