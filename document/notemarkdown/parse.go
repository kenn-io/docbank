package notemarkdown

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"unicode/utf8"
)

// Parse interprets original authored bytes, without changing them or doing I/O.
// Any error returns a zero Note; no partial artifact is publishable.
func Parse(ctx context.Context, original []byte) (Note, error) {
	if err := ctx.Err(); err != nil {
		return Note{}, err
	}
	if err := limitCount("input_bytes", len(original), maxInput); err != nil {
		return Note{}, err
	}
	b := &budget{}
	lines := 0
	for i := 0; i < len(original); {
		r, width := utf8.DecodeRune(original[i:])
		if r == 0 || r == utf8.RuneError && width == 1 {
			return Note{}, ErrInvalidInput
		}
		if err := b.step(ctx, width); err != nil {
			return Note{}, err
		}
		if r == '\r' || r == '\n' && (i == 0 || original[i-1] != '\r') {
			if err := addLimit("line_breaks", &lines, 1, maxLines); err != nil {
				return Note{}, err
			}
		}
		i += width
	}
	hasher := sha256.New()
	for i := 0; i < len(original); i += 1024 {
		end := min(i+1024, len(original))
		if err := b.step(ctx, end-i); err != nil {
			return Note{}, err
		}
		_, _ = hasher.Write(original[i:end])
	}
	var digest [32]byte
	hasher.Sum(digest[:0])
	n := Note{SourceSHA256: hex.EncodeToString(digest[:]), SourceBytes: len(original),
		Recipe: Recipe, FrontmatterState: "absent"}
	var err error
	n.FrontmatterState, n.Frontmatter, n.BodyStart, n.Metadata, n.Diagnostics, err = parseFrontmatter(ctx, original, b)
	if err != nil {
		return Note{}, err
	}
	blocks, err := scanBlocks(ctx, original, n.BodyStart, b)
	if err != nil {
		return Note{}, err
	}
	n.Links, err = extractLinks(ctx, original, blocks, b)
	if err != nil {
		return Note{}, err
	}
	n.Headings, err = extractHeadings(ctx, original, blocks, n.Links, b)
	if err != nil {
		return Note{}, err
	}
	if _, err := canonicalArtifact(ctx, n, b); err != nil {
		return Note{}, err
	}
	return n, nil
}
