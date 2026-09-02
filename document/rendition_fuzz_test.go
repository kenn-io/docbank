package document

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// FuzzSanitizeRenditionMarkdown checks the invariants the example tests rely
// on: sanitized Markdown renders without active HTML, carries no raw HTML
// nodes beyond the sanitizer's own list separator comment, stays within the
// rune budget, and keeps its rendered meaning when sanitized again.
func FuzzSanitizeRenditionMarkdown(f *testing.F) {
	for _, seed := range []string{
		"# Heading\n\nparagraph with **bold** and [link](https://example.test/safe)\n\n" +
			"- one\n- two\n\n```go\ncode\n```\n\n| a | b |\n|---|---|\n| 1 | 2 |\n",
		`<a href="javascript:alert(1)">label</a> and <iframe src="https://example.test"></iframe>`,
		"<code>code", "<pre><code>block", "<table><tr><td>cell",
		"1. first\n2. second\n\n3. third\n\n- [x] done\n- [ ] open\n",
		"text `a | b` more ``code `` with `` backticks``\n\n> quote\n\n---\n",
		"![private_image](https://example.test/private.png) <https://example.test/auto>\n",
		"<div>block <b>bold</b> <script>alert(1)</script></div>\n\n<!-- -->\n\n* tight\n* list\n",
		"\\*escaped\\* \\<not a tag\\> \\[not a link\\]\n",
		"## Heading with `code` and <span onclick=x>span</span>\n\nParagraph with separator\n",
		"0\n\n*", "onclick=", "a\n-", "* \n\n1.\n", "* >\x19", "0\n\n1)", "`\x10`\x10`\x18`",
		"<https://example.test/auto>", "<javascript:alert(1)>", "<JAVASCRIPT:alert(1)>", "<data:text/html,x>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, markdown string) {
		const maxRunes = 4_000
		out, _, renditionTruncated, err := sanitizeRenditionMarkdown(markdown, 2_048, 64<<10, maxRunes)
		if err != nil {
			assert.False(t, utf8.ValidString(markdown), "valid UTF-8 must sanitize: %v", err)
			return
		}
		require.True(t, utf8.ValidString(out))
		assert.LessOrEqual(t, utf8.RuneCountInString(out), maxRunes)
		if out == "" {
			return
		}
		html := renderSanitizedMarkdown(t, out)
		for _, active := range []string{"<script", "<iframe", `href="javascript:`} {
			assert.NotContains(t, html, active)
		}
		assertNoRawHTML(t, out)
		if renditionTruncated {
			return
		}
		again, _, _, err := sanitizeRenditionMarkdown(out, 2_048, 64<<10, maxRunes)
		require.NoError(t, err)
		assert.Equal(t, renderedMeaning(html), renderedMeaning(renderSanitizedMarkdown(t, again)),
			"sanitizing already sanitized Markdown must not change what it renders")
	})
}

// renderedMeaning drops the sanitizer's own list separator comment and all
// whitespace so two renderings compare on element structure and text. The
// sanitizer's whitespace placement is cosmetic and known to drift.
func renderedMeaning(html string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(html, "<!-- -->", "")), "")
}

func renderSanitizedMarkdown(t *testing.T, markdown string) string {
	t.Helper()
	parser := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(goldmarkhtml.WithUnsafe()),
	)
	var rendered bytes.Buffer
	require.NoError(t, parser.Convert([]byte(markdown), &rendered))
	return rendered.String()
}

func assertNoRawHTML(t *testing.T, markdown string) {
	t.Helper()
	source := []byte(markdown)
	root := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source))
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.RawHTML:
			t.Errorf("sanitized Markdown contains raw inline HTML in %q", markdown)
		case *ast.HTMLBlock:
			block := strings.TrimSpace(string(typed.Lines().Value(source)))
			if block != "<!-- -->" {
				t.Errorf("sanitized Markdown contains HTML block %q", block)
			}
		}
		return ast.WalkContinue, nil
	})
	require.NoError(t, err)
}
