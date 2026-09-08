package qmdexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"unicode/utf8"

	"go.kenn.io/docbank/document"
)

const maxClaimedFrontmatterBytes = 256 << 10

func readSource(
	ctx context.Context, reader BlobReader, source Source,
) (content []byte, retErr error) {
	stream, size, err := reader.OpenStreamContext(ctx, source.BlobSHA256)
	if err != nil {
		return nil, fmt.Errorf("open retained Markdown: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, stream.Close(), ctx.Err())
		if retErr != nil {
			content = nil
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size != source.BlobSize {
		return nil, errors.New("retained Markdown size does not match catalog")
	}
	content, retErr = io.ReadAll(io.LimitReader(stream, source.BlobSize+1))
	if retErr != nil {
		return nil, fmt.Errorf("read retained Markdown: %w", retErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(content)) != source.BlobSize {
		return nil, errors.New("retained Markdown read size does not match catalog")
	}
	if retErr = stream.Verify(); retErr != nil {
		return nil, fmt.Errorf("verify retained Markdown: %w", retErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(content)
	actual := hex.EncodeToString(digest[:])
	if actual != source.BlobSHA256 || actual != source.MarkdownChecksum {
		return nil, errors.New("retained Markdown checksum does not match catalog")
	}
	if !utf8.Valid(content) {
		return nil, errors.New("retained Markdown is not valid UTF-8")
	}
	return content, nil
}

func splitFrontmatter(markdown []byte) (string, []byte, error) {
	if !claimsDocbankFrontmatter(markdown) {
		return "", slices.Clone(markdown), nil
	}
	_, body, err := document.ParseRenditionFrontMatterV1(markdown)
	if err != nil {
		return "", nil, err
	}
	headerLength := len(markdown) - len(body)
	frontmatter := string(bytes.TrimSuffix(markdown[4:headerLength-4], []byte{'\n'}))
	return frontmatter, slices.Clone(body), nil
}

func claimsDocbankFrontmatter(markdown []byte) bool {
	detect := markdown
	if len(detect) > maxClaimedFrontmatterBytes+1 {
		detect = detect[:maxClaimedFrontmatterBytes+1]
	}
	if bytes.HasPrefix(detect, []byte{0xef, 0xbb, 0xbf}) {
		detect = detect[3:]
	}
	firstLF := bytes.IndexByte(detect, '\n')
	if firstLF < 0 || !bytes.Equal(bytes.TrimSuffix(detect[:firstLF], []byte{'\r'}), []byte("---")) {
		return false
	}
	header := detect[firstLF+1:]
	claimed := false
	for len(header) != 0 {
		lineEnd := bytes.IndexByte(header, '\n')
		line := header
		if lineEnd >= 0 {
			line = header[:lineEnd]
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if bytes.Equal(line, []byte("---")) || bytes.Equal(line, []byte("...")) {
			return claimed
		}
		if bytes.HasPrefix(line, []byte("docbank:")) || bytes.HasPrefix(line, []byte("'docbank':")) ||
			bytes.HasPrefix(line, []byte("\"docbank\":")) || bytes.Contains(line, []byte(document.RenditionMarkdownContractV1)) {
			claimed = true
		}
		if lineEnd < 0 {
			break
		}
		header = header[lineEnd+1:]
	}
	return claimed
}
