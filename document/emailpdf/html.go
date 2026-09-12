// Package emailpdf renders one retained email representation without fetching
// resources or interpreting active email content.
package emailpdf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	stdhtml "html"
	"image"
	_ "image/gif"  // Register only supported raster image decoders.
	_ "image/jpeg" // Register only supported raster image decoders.
	_ "image/png"  // Register only supported raster image decoders.
	"io"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
	"golang.org/x/net/html"
)

const maxHTMLBytes = 64 << 20

// Bound allocations before escaping or expanding CID payloads, including all
// headers and inventory text. Once refused, no further output is allocated.
type htmlOutput struct {
	strings.Builder

	err error
}

func (o *htmlOutput) reserve(n int) bool {
	if o.err == nil && n > maxHTMLBytes-o.Len() {
		o.err = errors.New("email PDF HTML exceeds limit")
	}
	return o.err == nil
}

func (o *htmlOutput) WriteString(s string) {
	if o.reserve(len(s)) {
		o.Builder.WriteString(s)
	}
}

func (o *htmlOutput) escape(s string) string {
	n := len(s)
	for _, c := range s {
		switch c {
		case '&':
			n += 4
		case '<', '>':
			n += 3
		case '\'', '"':
			n += 4
		}
	}
	if !o.reserve(n) {
		return ""
	}
	return stdhtml.EscapeString(s)
}

func checkHTMLComplexity(ctx context.Context, body []byte) error {
	z := html.NewTokenizer(bytes.NewReader(body))
	attributes := 0
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		kind := z.Next()
		if kind == html.ErrorToken {
			if errors.Is(z.Err(), io.EOF) {
				return nil
			}
			return z.Err()
		}
		if count >= 100_000 {
			return errors.New("email PDF HTML complexity limit exceeded")
		}
		if kind == html.StartTagToken || kind == html.SelfClosingTagToken {
			_, more := z.TagName()
			for more {
				_, _, more = z.TagAttr()
				attributes++
				if attributes > 100_000 {
					return errors.New("email PDF HTML complexity limit exceeded")
				}
			}
		}
	}
}

// OpenArtifact opens only an exact retained part reference. BuildHTML also
// checks its complete bytes and digest before using it.
type OpenArtifact func(context.Context, string, document.EmailArtifactRefV1) (io.ReadCloser, error)

type HTML struct {
	Bytes      []byte
	BodyPath   string
	BodySHA256 string
	BodySize   int64
}

func verified(ctx context.Context, open OpenArtifact, path string, ref *document.EmailArtifactRefV1) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if ref == nil || ref.Size < 0 || ref.Size > 16<<20 {
		return nil, errors.New("email PDF resource unavailable or exceeds 16 MiB")
	}
	r, err := open(ctx, path, *ref)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(r, ref.Size+1))
	err = errors.Join(err, r.Close(), ctx.Err())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if int64(len(b)) != ref.Size || hex.EncodeToString(sum[:]) != ref.SHA256 {
		return nil, errors.New("email PDF resource integrity check failed")
	}
	return b, nil
}

// BuildHTML chooses the decoder's complete outer body, retaining whitespace,
// quotes and header evidence. Attachment contents remain separate originals.
func BuildHTML(ctx context.Context, e document.EmailV1, paper string, open OpenArtifact) (HTML, error) {
	if paper == "" {
		paper = "A4"
	}
	if paper != "A4" && paper != "Letter" {
		return HTML{}, errors.New("email PDF paper must be A4 or Letter")
	}
	if e.Outcome != document.EmailOutcomeDecoded || e.Source.Verification != document.EmailVerificationVerified || e.Inventory == nil || e.Inventory.State != document.EmailInventoryComplete || e.Inventory.Termination != nil || open == nil {
		return HTML{}, errors.New("email PDF requires a complete verified email inventory")
	}
	parts := map[string]document.EmailPartV1{}
	for _, p := range e.Inventory.Parts {
		parts[p.Path] = p
	}
	var message *document.EmailMessageV1
	for i := range e.Inventory.Messages {
		if e.Inventory.Messages[i].Path == "1" {
			message = &e.Inventory.Messages[i]
			break
		}
	}
	if message == nil || message.SelectedBodyPath == nil {
		return HTML{}, errors.New("email PDF body unavailable")
	}
	part, ok := parts[*message.SelectedBodyPath]
	if !ok || part.Protection == document.EmailProtectionEncrypted || part.DecodeState != document.EmailDecodeDecoded || part.BodyUTF8 == nil {
		return HTML{}, errors.New("email PDF body unavailable")
	}
	var kind document.EmailBodyKind
	for _, a := range message.Alternatives {
		if a.PartPath == part.Path && a.DisplayState == document.EmailDisplayAvailable {
			kind = a.Kind
		}
	}
	if kind != document.EmailBodyHTML && kind != document.EmailBodyPlain {
		return HTML{}, errors.New("email PDF complete body display unavailable")
	}
	for _, d := range part.Diagnostics {
		if d.Code == document.EmailDiagnosticCharsetReplacement || d.Code == document.EmailDiagnosticCharsetInvalid {
			return HTML{}, errors.New("email PDF body character decoding is incomplete")
		}
	}
	body, err := verified(ctx, open, part.Path, part.BodyUTF8)
	if err != nil {
		return HTML{}, err
	}
	if len(bytes.TrimSpace(body)) == 0 || !utf8.Valid(body) {
		return HTML{}, errors.New("email PDF body is empty or invalid UTF-8")
	}
	root := parts["1"]
	headers, err := verified(ctx, open, "1", root.HeaderBlock)
	if err != nil {
		return HTML{}, err
	}
	var out htmlOutput
	out.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src 'none'; base-uri 'none'; form-action 'none'"><title>Email</title><style>@page{size:` + paper + ` portrait;margin:12mm}*{box-sizing:border-box}body{font-family:"Noto Sans",sans-serif;font-size:10pt;line-height:1.4;overflow-wrap:anywhere;word-break:normal}pre{white-space:pre-wrap;font-family:"Noto Sans Mono",monospace;overflow-wrap:anywhere}blockquote{border-left:2px solid #888;margin:8px 0;padding-left:12px}table{border-collapse:collapse;width:100%;table-layout:fixed}td,th{border:1px solid #aaa;padding:4px;overflow-wrap:anywhere}tr{break-inside:avoid}thead{display:table-header-group}img{max-width:100%;height:auto}h1,h2,h3{break-after:avoid}header{border-bottom:1px solid #777;margin-bottom:12px}header p{margin:3px 0;white-space:pre-wrap}.warning{color:#653600;border:1px solid #b87;padding:4px}.inventory{break-before:auto}a{color:inherit}</style></head><body><header>`)
	wanted := map[string]bool{"subject": true, "from": true, "to": true, "cc": true, "bcc": true, "date": true, "message-id": true}
	decoded := map[int]string{}
	for _, fields := range [][]document.EmailDecodedFieldV1{message.Fields.Subject, message.Fields.From, message.Fields.To, message.Fields.Cc, message.Fields.Bcc, message.Fields.MessageID} {
		for _, f := range fields {
			if f.State == document.EmailInterpretationDecoded && f.Text != nil {
				decoded[f.HeaderIndex] = *f.Text
			}
		}
	}
	for _, h := range root.Headers {
		name := "Malformed header"
		if h.Name != nil {
			name = *h.Name
		}
		if !wanted[strings.ToLower(name)] && h.State != document.EmailHeaderMalformed {
			continue
		}
		if h.Offset < 0 || h.Length < 0 || h.Offset > int64(len(headers))-h.Length {
			return HTML{}, errors.New("email PDF header evidence bounds invalid")
		}
		value := strings.TrimSpace(string(headers[h.Offset : h.Offset+h.Length]))
		if i := strings.IndexByte(value, ':'); i >= 0 && h.Name != nil {
			value = strings.TrimSpace(value[i+1:])
		}
		if display, ok := decoded[h.Index]; ok {
			value = display
		}
		out.WriteString("<p><b>" + out.escape(name) + ":</b> " + out.escape(value) + "</p>")
	}
	out.WriteString("</header>")
	for _, d := range e.Inventory.Diagnostics {
		out.WriteString(`<p class="warning">Warning: ` + out.escape(string(d.Code)) + "</p>")
	}
	for _, d := range message.Diagnostics {
		out.WriteString(`<p class="warning">Warning: ` + out.escape(string(d.Code)) + "</p>")
	}
	for _, d := range message.Date.Diagnostics {
		out.WriteString(`<p class="warning">Warning: ` + out.escape(string(d.Code)) + "</p>")
	}
	if kind == document.EmailBodyPlain {
		out.WriteString("<pre>" + out.escape(string(body)) + "</pre>")
	} else {
		if err := sanitize(ctx, &out, body, *message, parts, open); err != nil {
			return HTML{}, err
		}
	}
	out.WriteString(`<section class="inventory"><h2>Attachment inventory</h2><p>Attachment contents are not included in this PDF.</p><ul>`)
	for _, p := range document.EmailAttachmentParts(e) {
		size := "size unavailable"
		if p.Payload != nil {
			size = fmt.Sprintf("%d bytes", p.Payload.Size)
		}
		out.WriteString("<li>" + out.escape(p.Filename.SafeName) + " — " + size + " — " + document.EmailDocumentPartOutcome(p) + "</li>")
	}
	out.WriteString("</ul></section></body></html>")
	if out.err != nil {
		return HTML{}, out.err
	}
	return HTML{Bytes: []byte(out.String()), BodyPath: part.Path, BodySHA256: part.BodyUTF8.SHA256, BodySize: part.BodyUTF8.Size}, nil
}

func sanitize(ctx context.Context, out *htmlOutput, body []byte, message document.EmailMessageV1, parts map[string]document.EmailPartV1, open OpenArtifact) error {
	if err := checkHTMLComplexity(ctx, body); err != nil {
		return err
	}
	n, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parsing selected email HTML (complexity limit): %w", err)
	}
	allowed := " p div span pre blockquote br hr b strong i em u s sub sup h1 h2 h3 h4 h5 h6 ul ol li dl dt dd table thead tbody tfoot tr td th caption code "
	var walk func(*html.Node, int) error
	walk = func(n *html.Node, depth int) error {
		if depth > 512 {
			return errors.New("email PDF HTML complexity limit exceeded")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if out.err != nil {
			return out.err
		}
		if n.Type == html.TextNode {
			out.WriteString(out.escape(n.Data))
			return nil
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "iframe", "object", "embed", "svg", "canvas", "audio", "video", "link", "meta", "base":
				out.WriteString(`<span class="warning">[active content omitted: ` + out.escape(n.Data) + `]</span>`)
				return nil
			case "img":
				return inlineImage(ctx, out, n, message, parts, open)
			}
		}
		tag := n.Type == html.ElementNode && strings.Contains(allowed, " "+n.Data+" ")
		if tag {
			out.WriteString("<" + n.Data + ">")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c, depth+1); err != nil {
				return err
			}
		}
		if tag && n.Data != "br" && n.Data != "hr" {
			out.WriteString("</" + n.Data + ">")
		}
		if n.Type == html.ElementNode && n.Data == "a" {
			for _, a := range n.Attr {
				if a.Key == "href" && (strings.HasPrefix(a.Val, "https://") || strings.HasPrefix(a.Val, "http://") || strings.HasPrefix(a.Val, "mailto:")) {
					out.WriteString(" (" + out.escape(a.Val) + ")")
				}
			}
		}
		return nil
	}
	return walk(n, 0)
}

func inlineImage(ctx context.Context, out *htmlOutput, n *html.Node, message document.EmailMessageV1, parts map[string]document.EmailPartV1, open OpenArtifact) error {
	src, alt := "", ""
	for _, a := range n.Attr {
		if a.Key == "src" {
			src = a.Val
		}
		if a.Key == "alt" {
			alt = a.Val
		}
	}
	placeholder := func(reason string) {
		out.WriteString(`<span class="warning">[` + reason + ": " + out.escape(alt) + "]</span>")
	}
	if !strings.HasPrefix(src, "cid:") {
		placeholder("remote resource unavailable")
		return nil
	}
	cid := strings.TrimPrefix(src, "cid:")
	var candidate string
	for _, g := range message.RelatedGroups {
		if g.BodyPath == nil || *g.BodyPath != *message.SelectedBodyPath {
			continue
		}
		for _, r := range g.Resources {
			if r.CID == nil || *r.CID != cid {
				continue
			}
			if r.State != document.EmailResourceUnique || len(r.Candidates) != 1 || candidate != "" {
				placeholder("ambiguous inline image")
				return nil
			}
			candidate = r.Candidates[0]
		}
	}
	p, ok := parts[candidate]
	if !ok || p.Payload == nil || p.DecodeState != document.EmailDecodeDecoded || p.Protection == document.EmailProtectionEncrypted {
		placeholder("missing inline image")
		return nil
	}
	if p.Payload.Size < 0 || p.Payload.Size > 16<<20 {
		return errors.New("email PDF resource unavailable or exceeds 16 MiB")
	}
	if !out.reserve(base64.StdEncoding.EncodedLen(int(p.Payload.Size)) + 64 + len(alt)*6) {
		return out.err
	}
	b, err := verified(ctx, open, p.Path, p.Payload)
	if err != nil {
		return err
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 || (format != "png" && format != "jpeg" && format != "gif") {
		placeholder("unsupported inline image")
		return nil //nolint:nilerr // Unsupported raster formats are explicit visible placeholders, never rendered unchecked.
	}
	out.WriteString(`<img alt="` + out.escape(alt) + `" src="data:image/` + format + `;base64,` + base64.StdEncoding.EncodeToString(b) + `">`)
	return nil
}
