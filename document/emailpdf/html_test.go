package emailpdf

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/emailmime"
	"golang.org/x/net/html"
)

func decodedHTML(t *testing.T, source, paper string) (HTML, error) {
	t.Helper()
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	d, err := emailmime.Decode(t.Context(), digest, int64(len(source)), strings.NewReader(source), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return BuildHTML(t.Context(), d.Evidence, paper, func(ctx context.Context, path string, ref document.EmailArtifactRefV1) (io.ReadCloser, error) {
		return d.OpenArtifact(ctx, path, string(ref.Role))
	})
}

func TestHTMLPreservesHeadersQuotesWhitespaceAndInventory(t *testing.T) {
	source := "Subject: =?UTF-8?Q?R=C3=A9sum=C3=A9?=\r\nFrom: Author <author@example.test>\r\nTo: Reader <reader@example.test>\r\nBcc: Private <private@example.test>\r\nDate: Mon, 01 Jan 2024 12:34:56 +0530\r\nMessage-ID: <one@example.test>\r\nContent-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello 世界\r\n  indented\r\n> Full original quote\r\n--m\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=appendix.txt\r\n\r\nsecret attachment bytes\r\n--m--\r\n"
	got, err := decodedHTML(t, source, "A4")
	require.NoError(t, err)
	for _, want := range []string{"Résumé", "author@example.test", "reader@example.test", "private@example.test", "+0530", "one@example.test", "Hello 世界", "  indented", "Full original quote", "appendix.txt", "decoded", "23 bytes", "size:A4 portrait", "margin:12mm", "white-space:pre-wrap"} {
		require.Contains(t, string(got.Bytes), want)
	}
	require.NotContains(t, string(got.Bytes), "secret attachment bytes")
	require.Equal(t, "1.1", got.BodyPath)
}

func TestHTMLSanitizerShowsUnsafeResourcesAndExpandedHiddenQuotes(t *testing.T) {
	source := "Subject: unsafe\r\nDate: nonsense date\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p onclick='alert(1)'>body</p><blockquote hidden style='display:none'>full quote</blockquote><img src='https://example.test/track'><img src='cid:absent'><script>danger()</script><iframe src='file:///etc/passwd'></iframe><a href='https://example.test/long'>link</a>"
	got, err := decodedHTML(t, source, "Letter")
	require.NoError(t, err)
	s := string(got.Bytes)
	for _, want := range []string{"full quote", "nonsense date", "date_invalid", "remote resource", "missing inline image", "active content", "size:Letter portrait", "https://example.test/long"} {
		require.Contains(t, s, want)
	}
	for _, forbidden := range []string{"onclick", "display:none", "<script", "<iframe", "src=\"https:", "file:///etc/passwd"} {
		require.NotContains(t, s, forbidden)
	}
}

func TestHTMLRejectsIncompleteAndInvalidPageSize(t *testing.T) {
	_, err := decodedHTML(t, "Content-Type: multipart/mixed; boundary=missing\r\n\r\ntruncated", "A4")
	require.Error(t, err)
	_, err = decodedHTML(t, "Content-Type: text/plain\r\n\r\nbody", "A3")
	require.Error(t, err)
}

func TestHTMLRejectsExcessiveParserComplexity(t *testing.T) {
	var attributes strings.Builder
	for i := range 100_001 {
		fmt.Fprintf(&attributes, "x%d='y' ", i)
	}
	for name, body := range map[string]string{
		"tokens":     strings.Repeat("<br>", 100_001),
		"depth":      strings.Repeat("<div>", 513) + "body" + strings.Repeat("</div>", 513),
		"attributes": "<p " + attributes.String() + ">body</p>",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodedHTML(t, "Content-Type: text/html\r\n\r\n"+body, "A4")
			require.ErrorContains(t, err, "complexity limit")
		})
	}
}

func TestHTMLBoundsEscapingAndCIDBeforeAllocation(t *testing.T) {
	var out htmlOutput
	out.WriteString(strings.Repeat("x", maxHTMLBytes-100))
	before := out.Len()
	message := document.EmailMessageV1{SelectedBodyPath: new("1.1"), RelatedGroups: []document.EmailRelatedGroupV1{{BodyPath: new("1.1"), Resources: []document.EmailResourceV1{{CID: new("image"), State: document.EmailResourceUnique, Candidates: []string{"1.2"}}}}}}
	parts := map[string]document.EmailPartV1{"1.2": {Path: "1.2", DecodeState: document.EmailDecodeDecoded, Payload: &document.EmailArtifactRefV1{Size: 1024}}}
	err := inlineImage(t.Context(), &out, &html.Node{Attr: []html.Attribute{{Key: "src", Val: "cid:image"}}}, message, parts, func(context.Context, string, document.EmailArtifactRefV1) (io.ReadCloser, error) {
		t.Fatal("oversized CID must be refused before opening or encoding its payload")
		return nil, errors.New("unexpected payload open")
	})
	require.ErrorContains(t, err, "exceeds limit")
	require.Equal(t, before, out.Len())
	out.err = nil
	require.Empty(t, out.escape(strings.Repeat("&", 21)))
	require.ErrorContains(t, out.err, "exceeds limit")
	out.WriteString("must not append after refusal")
	require.Equal(t, before, out.Len())
}
