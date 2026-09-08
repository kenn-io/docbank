package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// ParseRenditionFrontMatterV1 validates the exact deterministic envelope and
// returns its body as a view into data. Navigation offsets are body-relative.
func ParseRenditionFrontMatterV1(data []byte) (RenditionFrontMatterV1, []byte, error) {
	const opening = "---\n"
	closing := []byte("\n---\n")
	if !bytes.HasPrefix(data, []byte(opening)) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter opening delimiter is missing")
	}
	window := data[:min(len(data), maxRenditionFrontMatterBytes)]
	relativeEnd := bytes.Index(window[len(opening):], closing)
	if relativeEnd < 0 {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter closing delimiter is missing")
	}
	headerLength := len(opening) + relativeEnd + len(closing)
	yamlEnd := len(opening) + relativeEnd + 1
	var envelope struct {
		Docbank RenditionFrontMatterV1 `yaml:"docbank"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data[len(opening):yamlEnd]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&envelope); err != nil {
		return RenditionFrontMatterV1{}, nil, errors.New("decoding rendition frontmatter failed")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter contains multiple YAML documents")
	}
	frontmatter, body := envelope.Docbank, data[headerLength:]
	if err := validateRenditionFrontMatterV1(frontmatter, body); err != nil {
		return RenditionFrontMatterV1{}, nil, err
	}
	canonical, err := marshalRenditionFrontMatterV1(frontmatter)
	if err != nil {
		return RenditionFrontMatterV1{}, nil, err
	}
	if !bytes.Equal(canonical, data[:headerLength]) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter is not canonical")
	}
	return frontmatter, body, nil
}

func validateRenditionFrontMatterV1(value RenditionFrontMatterV1, body []byte) error {
	validDigest := func(value string) bool {
		if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
			return false
		}
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == sha256.Size
	}
	for name, digest := range map[string]string{
		"source SHA-256": value.Source.SHA256, "build ID": value.Rendition.BuildID,
		"rendition request fingerprint": value.Rendition.RenditionRequestFingerprint,
		"evidence lexical fingerprint":  value.Rendition.EvidenceLexicalFingerprint,
		"body SHA-256":                  value.Rendition.BodySHA256,
	} {
		if !validDigest(digest) {
			return fmt.Errorf("rendition frontmatter %s is invalid", name)
		}
	}
	for name, text := range map[string]string{
		"source format": value.Source.Format, "source media type": value.Source.MediaType,
		"document title": value.Document.Title, "document language": value.Document.Language,
	} {
		if !utf8.ValidString(text) {
			return fmt.Errorf("rendition frontmatter %s is not UTF-8", name)
		}
	}
	if value.Contract != RenditionMarkdownContractV1 || value.Source.Format == "" ||
		value.Source.MediaType == "" || value.Rendition.NormalizedEvidenceContract != NormalizedEvidenceContractV1 ||
		!validEvidenceCompleteness(value.Rendition.Completeness) || !validEvidenceUnitKind(value.Document.UnitKind) ||
		value.Document.UnitCount < 1 || value.Document.UnitCount > maxEvidenceUnits ||
		value.Navigation.OffsetBase != RenditionNavigationOffsetBody ||
		value.Navigation.Complete != (value.Document.UnitCount <= maxRenditionNavigationEntries) ||
		len(value.Navigation.Entries) > maxRenditionNavigationEntries ||
		len(value.Navigation.Entries) > value.Document.UnitCount {
		return errors.New("rendition frontmatter contract is invalid")
	}
	if len(body) == 0 || !utf8.Valid(body) {
		return errors.New("rendition Markdown body is empty or not UTF-8")
	}
	if got := checksumBytes(body); got != value.Rendition.BodySHA256 {
		return fmt.Errorf("rendition body SHA-256 %s differs from frontmatter %s", got, value.Rendition.BodySHA256)
	}
	seen := make(map[string]struct{}, len(value.Navigation.Entries))
	priorByte, line := 0, 1
	for _, entry := range value.Navigation.Entries {
		if entry.Key == "" || !utf8.ValidString(entry.Key) || !utf8.ValidString(entry.Title) ||
			!renditionFrontMatterLocatorKind(entry.Kind) || entry.Byte < 0 ||
			entry.Byte >= len(body) || entry.Byte < priorByte || !utf8.RuneStart(body[entry.Byte]) {
			return errors.New("rendition frontmatter navigation is invalid")
		}
		line += bytes.Count(body[priorByte:entry.Byte], []byte{'\n'})
		if entry.Line != line {
			return errors.New("rendition frontmatter navigation is invalid")
		}
		if _, exists := seen[entry.Key]; exists {
			return errors.New("rendition frontmatter navigation contains a duplicate key")
		}
		seen[entry.Key] = struct{}{}
		priorByte = entry.Byte
	}
	return nil
}

func renditionFrontMatterLocatorKind(value EvidenceLocatorKind) bool {
	switch value {
	case EvidenceLocatorGeneric, EvidenceLocatorLine, EvidenceLocatorMessage,
		EvidenceLocatorPage, EvidenceLocatorRecord, EvidenceLocatorSection,
		EvidenceLocatorSheet, EvidenceLocatorSlide, EvidenceLocatorSpine:
		return true
	default:
		return false
	}
}
