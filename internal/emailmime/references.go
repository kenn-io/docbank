package emailmime

import (
	"errors"
	"io"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// htmlImageContentIDs returns the content IDs an HTML body shows with
// <img src="cid:...">, percent-decoded as a browser resolves them.
func htmlImageContentIDs(body io.Reader) (map[string]bool, error) {
	ids := map[string]bool{}
	tokenizer := html.NewTokenizer(body)
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			if err := tokenizer.Err(); !errors.Is(err, io.EOF) {
				return nil, err
			}
			return ids, nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttributes := tokenizer.TagName()
			if string(name) != "img" {
				continue
			}
			for hasAttributes {
				var key, value []byte
				key, value, hasAttributes = tokenizer.TagAttr()
				if string(key) != "src" {
					continue
				}
				source := strings.TrimSpace(string(value))
				if len(source) < 4 || !strings.EqualFold(source[:4], "cid:") {
					continue
				}
				if id, err := url.PathUnescape(source[4:]); err == nil && id != "" {
					ids[id] = true
				}
			}
		case html.TextToken, html.EndTagToken, html.CommentToken, html.DoctypeToken:
		}
	}
}
