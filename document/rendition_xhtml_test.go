package document

import (
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"
)

type cancelAfterXHTMLReadContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelAfterXHTMLReadContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelAfterXHTMLReadContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelAfterXHTMLReadContext) Value(any) any               { return nil }

func (ctx *cancelAfterXHTMLReadContext) Err() error {
	ctx.calls++
	if ctx.calls >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

type cancelOnXHTMLReadContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelOnXHTMLReadContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelOnXHTMLReadContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelOnXHTMLReadContext) Value(any) any               { return nil }

func (ctx *cancelOnXHTMLReadContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

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

func TestRenditionXHTMLContextCancellationAfterRead(t *testing.T) {
	ctx := &cancelAfterXHTMLReadContext{cancelAt: 2}
	started := false
	reader := contextReader{ctx: ctx, reader: trackingReader{reader: strings.NewReader(strings.Repeat("text", 10000)), started: &started}}
	_, err := io.ReadAll(reader)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, started)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)
}

func TestRenditionXHTMLContextCancellationAfterDecodeRead(t *testing.T) {
	ctx := &cancelOnXHTMLReadContext{cancelAt: 3}
	text, err := RenditionMarkdownFromXHTMLContext(ctx, []byte(`<html xmlns="http://www.w3.org/1999/xhtml"><body>text</body></html>`), 100)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, text)
	require.Equal(t, ctx.cancelAt, ctx.calls)
}

func TestRenditionXHTMLContextCancellationWhileConvertingAttributes(t *testing.T) {
	var source strings.Builder
	source.WriteString(`<html xmlns="http://www.w3.org/1999/xhtml"><body`)
	for index := range 4096 {
		source.WriteString(` a`)
		source.WriteString(strconv.Itoa(index))
		source.WriteString(`="x"`)
	}
	source.WriteString(`>text</body></html>`)

	ctx := &cancelAfterXHTMLReadContext{cancelAt: 6}
	text, err := RenditionMarkdownFromXHTMLContext(ctx, []byte(source.String()), 100)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, text)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)
}

func TestRenditionFinalizationContextCancellation(t *testing.T) {
	tableBlocks := []renditionBlock{{kind: renditionTable, rows: [][][]renditionInline{make([][]renditionInline, 1024)}}}
	listItems := make([]renditionListItem, 1024)
	for index := range listItems {
		listItems[index].present = true
	}
	listBlocks := []renditionBlock{{kind: renditionListBlock, list: &renditionList{ordered: true, start: "1", items: listItems}}}
	var ctx context.Context = &cancelOnXHTMLReadContext{cancelAt: 4}
	err := canonicalizeRenditionBlocks(ctx, tableBlocks)
	require.ErrorIs(t, err, context.Canceled)

	tableCellBlocks := []renditionBlock{{kind: renditionTable, rows: [][][]renditionInline{{{}}}}}
	ctx = &cancelOnXHTMLReadContext{cancelAt: 5}
	err = canonicalizeRenditionBlocks(ctx, tableCellBlocks)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 4}
	err = canonicalizeRenditionBlocks(ctx, listBlocks)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelAfterXHTMLReadContext{cancelAt: 3}
	_, err = renditionXHTMLSerializationFits(ctx, tableBlocks, 1<<20)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelAfterXHTMLReadContext{cancelAt: 3}
	_, err = renditionXHTMLSerializationFits(ctx, listBlocks, 1<<20)
	require.ErrorIs(t, err, context.Canceled)

	largeText := []renditionBlock{{inlines: []renditionInline{{kind: renditionText, text: strings.Repeat("x", 1<<20)}}}}
	ctx = &cancelAfterXHTMLReadContext{cancelAt: 4}
	_, err = renditionXHTMLSerializationFits(ctx, largeText, 2<<20)
	require.ErrorIs(t, err, context.Canceled)

	largeUnicode := []renditionBlock{{inlines: []renditionInline{{kind: renditionText, text: "x" + strings.Repeat("é", 1<<19)}}}}
	ctx = &cancelAfterXHTMLReadContext{cancelAt: 4}
	_, err = renditionXHTMLSerializationFits(ctx, largeUnicode, 2<<20)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelAfterXHTMLReadContext{cancelAt: 4}
	_, _, err = serializeRenditionBlocksContext(ctx, largeText, 2<<20)
	require.ErrorIs(t, err, context.Canceled)

	largeCode := []renditionBlock{{inlines: []renditionInline{{kind: renditionInlineCode, text: strings.Repeat("`", 1<<20)}}}}
	ctx = &cancelAfterXHTMLReadContext{cancelAt: 4}
	_, err = renditionXHTMLSerializationFits(ctx, largeCode, 2<<20)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelAfterXHTMLReadContext{cancelAt: 4}
	_, _, err = serializeRenditionBlocksContext(ctx, largeCode, 2<<20)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 3}
	_, err = canonicalEvidenceLineEndingsContext(ctx, strings.Repeat("\r\n", 1<<19))
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 4}
	_, err = canonicalEvidenceStringContext(ctx, strings.Repeat("e\u0301", 256))
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 7}
	writer := renditionHTMLWriter{work: &renditionXHTMLWork{remaining: 1 << 20}}
	err = writer.writeTextContext(ctx, strings.Repeat("x", 1024)+" "+strings.Repeat("y", 1024))
	require.ErrorIs(t, err, context.Canceled)
	require.NotEmpty(t, writer.current)

	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}}
	writer.startTag(html.Token{Data: "img", Attr: []html.Attribute{{Key: "alt", Val: strings.Repeat("x", 1<<20)}}}, 0, false)
	require.ErrorIs(t, writer.err, context.Canceled)

	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}, inPre: true}
	writer.preText.WriteString(strings.Repeat("x", 1<<20))
	writer.endTag("pre")
	require.ErrorIs(t, writer.err, context.Canceled)

	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}, inlineCode: true}
	writer.inlineText.WriteString(strings.Repeat("x", 1<<20))
	writer.endTag("code")
	require.ErrorIs(t, writer.err, context.Canceled)

	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}}
	writer.startLink("")
	writer.links[0].children = make([]renditionInline, 1024)
	for index := range writer.links[0].children {
		writer.links[0].children[index] = renditionInline{kind: renditionText, text: "x"}
	}
	writer.endTag("a")
	require.ErrorIs(t, writer.err, context.Canceled)

	attributes := make([]html.Attribute, 4096)
	for index := range attributes {
		attributes[index] = html.Attribute{Key: "data-" + strconv.Itoa(index), Val: "x"}
	}
	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}}
	writer.startTag(html.Token{Data: "ol", Attr: attributes}, 0, false)
	require.ErrorIs(t, writer.err, context.Canceled)

	writer = renditionHTMLWriter{ctx: &cancelAfterXHTMLReadContext{cancelAt: 2}, work: &renditionXHTMLWork{remaining: 1 << 20}}
	writer.startTag(html.Token{Data: "ol", Attr: []html.Attribute{{Key: "start", Val: strings.Repeat("9", 1<<20)}}}, 0, false)
	require.ErrorIs(t, writer.err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 2}
	_, err = degradeOrderedListItem(ctx, serializedOrderedListItem{ordinal: "999999999", value: strings.Repeat("line\n", 1<<12)}, 0, false)
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelOnXHTMLReadContext{cancelAt: 2}
	_, err = prefixRenditionLinesContext(ctx, strings.Repeat("line\n", 1<<12), "", "  ")
	require.ErrorIs(t, err, context.Canceled)

	ctx = &cancelAfterXHTMLReadContext{cancelAt: 3}
	_, _, err = serializeRenditionBlocksContext(ctx, tableBlocks, 1<<20)
	require.ErrorIs(t, err, context.Canceled)

	ordered := renditionList{ordered: true, start: "999999998", tight: true, items: []renditionListItem{
		{present: true, blocks: []renditionBlock{{kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "x"}}}}},
		{present: true, blocks: []renditionBlock{{kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "x"}}}}},
		{present: true, blocks: []renditionBlock{{kind: renditionParagraph, inlines: []renditionInline{{kind: renditionText, text: "x"}}}}},
	}}
	ctx = &cancelAfterXHTMLReadContext{cancelAt: 12}
	output := renditionBuffer{ctx: ctx}
	result := appendRenditionList(&output, ordered, 100, 0, false)
	require.True(t, result.truncated)
	require.ErrorIs(t, output.err, context.Canceled)
	require.Contains(t, output.String(), "999999998. x")
	require.Contains(t, output.String(), "999999999. x")
	require.NotContains(t, output.String(), "- 999999998\\.")
}

type trackingReader struct {
	reader  io.Reader
	started *bool
}

func (reader trackingReader) Read(buffer []byte) (int, error) {
	*reader.started = true
	return reader.reader.Read(buffer)
}
