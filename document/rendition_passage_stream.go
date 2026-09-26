package document

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// ReadRenditionPassageV1 validates a retained rendition and extracts one
// passage while retaining only the frontmatter, a read buffer, and the quote.
// The caller must independently verify the complete artifact's blob digest.
func ReadRenditionPassageV1(
	source io.Reader, artifactBytes int64, ref PassageRefV1,
) (RenditionFrontMatterV1, []byte, error) {
	if err := ValidatePassageIdentityV1(ref); err != nil {
		return RenditionFrontMatterV1{}, nil, err
	}
	if ref.ByteEnd-ref.ByteStart > 256<<10 {
		return RenditionFrontMatterV1{}, nil, errors.New("passage exceeds byte bound")
	}
	reader := bufio.NewReaderSize(source, 32<<10)
	header := make([]byte, 0, 4096)
	for len(header) < maxRenditionFrontMatterBytes {
		b, err := reader.ReadByte()
		if err != nil {
			return RenditionFrontMatterV1{}, nil, fmt.Errorf("reading rendition frontmatter: %w", err)
		}
		header = append(header, b)
		if bytes.HasSuffix(header, []byte("\n---\n")) {
			break
		}
	}
	if !bytes.HasSuffix(header, []byte("\n---\n")) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter exceeds its byte bound or is incomplete")
	}
	frontmatter, headerLength, err := ParseRenditionFrontMatterHeaderV1(header)
	if err != nil {
		return RenditionFrontMatterV1{}, nil, err
	}
	bodyBytes := artifactBytes - int64(headerLength)
	if bodyBytes < 1 || int64(ref.ByteEnd) > bodyBytes {
		return RenditionFrontMatterV1{}, nil, errors.New("passage range exceeds rendition body")
	}
	if frontmatter.Rendition.BodySHA256 != ref.BodySHA256 {
		return RenditionFrontMatterV1{}, nil, errors.New("passage body SHA-256 differs from rendition frontmatter")
	}
	quote := make([]byte, 0, ref.ByteEnd-ref.ByteStart)
	bodyHash := sha256.New()
	var buffer [32 << 10]byte
	var decoder [32<<10 + 4]byte
	var pending [4]byte
	pendingLength := 0
	navigation := frontmatter.Navigation.Entries
	navigationIndex, line := 0, 1
	seen := make(map[string]struct{}, len(navigation))
	for offset := int64(0); offset < bodyBytes; {
		length := int(min(bodyBytes-offset, int64(len(buffer))))
		if _, err := io.ReadFull(reader, buffer[:length]); err != nil {
			return RenditionFrontMatterV1{}, nil, fmt.Errorf("reading rendition body: %w", err)
		}
		chunk := buffer[:length]
		_, _ = bodyHash.Write(chunk)
		copy(decoder[:pendingLength], pending[:pendingLength])
		copy(decoder[pendingLength:], chunk)
		validation := decoder[:pendingLength+length]
		validThrough := 0
		for validThrough < len(validation) && utf8.FullRune(validation[validThrough:]) {
			r, width := utf8.DecodeRune(validation[validThrough:])
			if r == utf8.RuneError && width == 1 {
				return RenditionFrontMatterV1{}, nil, errors.New("rendition Markdown body is not UTF-8")
			}
			validThrough += width
		}
		pendingLength = copy(pending[:], validation[validThrough:])
		cursor := 0
		for navigationIndex < len(navigation) && int64(navigation[navigationIndex].Byte) < offset+int64(length) {
			entry := navigation[navigationIndex]
			position := int64(entry.Byte) - offset
			if position < int64(cursor) || entry.Key == "" || !renditionFrontMatterLocatorKind(entry.Kind) ||
				entry.Byte < 0 || position >= int64(length) {
				return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter navigation is invalid")
			}
			line += bytes.Count(chunk[cursor:position], []byte{'\n'})
			if !utf8.RuneStart(chunk[position]) || entry.Line != line {
				return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter navigation is invalid")
			}
			if _, exists := seen[entry.Key]; exists {
				return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter navigation contains a duplicate key")
			}
			seen[entry.Key] = struct{}{}
			cursor = int(position)
			navigationIndex++
		}
		line += bytes.Count(chunk[cursor:], []byte{'\n'})
		if int64(ref.ByteStart) >= offset && int64(ref.ByteStart) < offset+int64(length) &&
			!utf8.RuneStart(chunk[int64(ref.ByteStart)-offset]) {
			return RenditionFrontMatterV1{}, nil, errors.New("passage range splits UTF-8")
		}
		if int64(ref.ByteEnd) >= offset && int64(ref.ByteEnd) < offset+int64(length) &&
			!utf8.RuneStart(chunk[int64(ref.ByteEnd)-offset]) {
			return RenditionFrontMatterV1{}, nil, errors.New("passage range splits UTF-8")
		}
		start := max(int64(ref.ByteStart), offset)
		end := min(int64(ref.ByteEnd), offset+int64(length))
		if end > start {
			quote = append(quote, chunk[start-offset:end-offset]...)
		}
		offset += int64(length)
	}
	if pendingLength != 0 || navigationIndex != len(navigation) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition body or navigation is incomplete")
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition artifact size differs from authority")
	}
	if got := hex.EncodeToString(bodyHash.Sum(nil)); got != frontmatter.Rendition.BodySHA256 {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition body SHA-256 differs from frontmatter")
	}
	if got := sha256.Sum256(quote); hex.EncodeToString(got[:]) != ref.QuoteSHA256 {
		return RenditionFrontMatterV1{}, nil, errors.New("passage quote SHA-256 differs from reference")
	}
	return frontmatter, quote, nil
}
