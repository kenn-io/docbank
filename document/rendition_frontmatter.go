package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	RenditionMarkdownContractV1   = "docbank-sanitized-markdown/v1"
	RenditionNavigationOffsetBody = "body"
	maxRenditionNavigationEntries = 1024
	maxRenditionFrontMatterBytes  = 256 << 10
)

type RenditionMarkdownSourceV1 struct {
	SHA256    string `yaml:"sha256"`
	Format    string `yaml:"format"`
	MediaType string `yaml:"media_type"`
}

type RenditionMarkdownBuildV1 struct {
	BuildID                     string               `yaml:"build_id"`
	RenditionRequestFingerprint string               `yaml:"rendition_request_fingerprint"`
	EvidenceLexicalFingerprint  string               `yaml:"evidence_lexical_fingerprint"`
	NormalizedEvidenceContract  string               `yaml:"normalized_evidence_contract"`
	BodySHA256                  string               `yaml:"body_sha256"`
	Completeness                EvidenceCompleteness `yaml:"completeness"`
	Truncated                   bool                 `yaml:"truncated"`
}

type RenditionMarkdownDocumentV1 struct {
	Title     string           `yaml:"title,omitempty"`
	Language  string           `yaml:"language,omitempty"`
	UnitKind  EvidenceUnitKind `yaml:"unit_kind"`
	UnitCount int              `yaml:"unit_count"`
}

type RenditionNavigationEntryV1 struct {
	Key   string              `yaml:"key"`
	Kind  EvidenceLocatorKind `yaml:"kind"`
	Title string              `yaml:"title,omitempty"`
	Line  int                 `yaml:"line"`
	Byte  int                 `yaml:"byte"`
}

type RenditionMarkdownNavigationV1 struct {
	OffsetBase string                       `yaml:"offset_base"`
	Complete   bool                         `yaml:"complete"`
	Entries    []RenditionNavigationEntryV1 `yaml:"entries"`
}

type RenditionFrontMatterV1 struct {
	Contract   string                        `yaml:"contract"`
	Source     RenditionMarkdownSourceV1     `yaml:"source"`
	Rendition  RenditionMarkdownBuildV1      `yaml:"rendition"`
	Document   RenditionMarkdownDocumentV1   `yaml:"document"`
	Navigation RenditionMarkdownNavigationV1 `yaml:"navigation"`
}

type RenditionEnvelopeV1 struct {
	BuildID                     string
	SourceSHA256                string
	SourceFormat                string
	SourceMediaType             string
	RenditionRequestFingerprint string
	EvidenceLexicalFingerprint  string
	NormalizedEvidenceContract  string
	UnitKind                    EvidenceUnitKind
	Title                       string
	Language                    string
}

// EnvelopeRenditionV1 adds deterministic build-scoped YAML frontmatter to the
// retained Markdown only. Units and lexical segments remain body-derived and
// therefore never ingest frontmatter metadata.
func EnvelopeRenditionV1(rendition RenditionV1, envelope RenditionEnvelopeV1) (RenditionV1, RenditionFrontMatterV1, error) {
	if len(rendition.Markdown) == 0 || rendition.MarkdownChecksum != checksumBytes(rendition.Markdown) {
		return RenditionV1{}, RenditionFrontMatterV1{}, errors.New("rendition Markdown body authority is invalid")
	}
	for name, value := range map[string]string{
		"build ID": envelope.BuildID, "source SHA-256": envelope.SourceSHA256,
		"rendition request fingerprint": envelope.RenditionRequestFingerprint,
		"evidence lexical fingerprint":  envelope.EvidenceLexicalFingerprint,
	} {
		if len(value) != 64 {
			return RenditionV1{}, RenditionFrontMatterV1{}, fmt.Errorf("rendition frontmatter %s is invalid", name)
		}
	}
	if envelope.SourceFormat == "" || envelope.SourceMediaType == "" ||
		envelope.NormalizedEvidenceContract != NormalizedEvidenceContractV1 || envelope.UnitKind == "" {
		return RenditionV1{}, RenditionFrontMatterV1{}, errors.New("rendition frontmatter identity is incomplete")
	}
	navigation, err := renditionNavigation(rendition)
	if err != nil {
		return RenditionV1{}, RenditionFrontMatterV1{}, err
	}
	frontmatter := RenditionFrontMatterV1{Contract: RenditionMarkdownContractV1,
		Source: RenditionMarkdownSourceV1{SHA256: envelope.SourceSHA256,
			Format: envelope.SourceFormat, MediaType: envelope.SourceMediaType},
		Rendition: RenditionMarkdownBuildV1{BuildID: envelope.BuildID,
			RenditionRequestFingerprint: envelope.RenditionRequestFingerprint,
			EvidenceLexicalFingerprint:  envelope.EvidenceLexicalFingerprint,
			NormalizedEvidenceContract:  envelope.NormalizedEvidenceContract,
			BodySHA256:                  rendition.MarkdownChecksum, Completeness: rendition.Completeness,
			Truncated: slices.ContainsFunc(rendition.Warnings, func(w RenditionWarningV1) bool { return w.Code == "truncated" })},
		Document: RenditionMarkdownDocumentV1{Title: envelope.Title, Language: envelope.Language,
			UnitKind: envelope.UnitKind, UnitCount: len(rendition.Units)}, Navigation: navigation}
	header, err := marshalRenditionFrontMatterV1(frontmatter)
	if err != nil {
		return RenditionV1{}, RenditionFrontMatterV1{}, err
	}
	if len(header) > maxRenditionFrontMatterBytes {
		return RenditionV1{}, RenditionFrontMatterV1{}, errors.New("rendition frontmatter exceeds its byte bound")
	}
	body := slices.Clone(rendition.Markdown)
	rendition.Markdown = make([]byte, 0, len(header)+len(body))
	rendition.Markdown = append(rendition.Markdown, header...)
	rendition.Markdown = append(rendition.Markdown, body...)
	rendition.MarkdownChecksum = checksumBytes(rendition.Markdown)
	rendition.Checksum = renditionChecksum(rendition)
	return rendition, frontmatter, nil
}

// ParseRenditionFrontMatterV1 validates the exact deterministic envelope and
// returns its body as a view into data. Navigation offsets are body-relative.
func ParseRenditionFrontMatterV1(data []byte) (RenditionFrontMatterV1, []byte, error) {
	const opening = "---\n"
	closing := []byte("\n---\n")
	if !bytes.HasPrefix(data, []byte(opening)) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter opening delimiter is missing")
	}
	relativeEnd := bytes.Index(data[len(opening):], closing)
	if relativeEnd < 0 {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter closing delimiter is missing")
	}
	headerLength := len(opening) + relativeEnd + len(closing)
	if headerLength > maxRenditionFrontMatterBytes {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter exceeds its byte bound")
	}
	yamlEnd := len(opening) + relativeEnd + 1
	var envelope struct {
		Docbank RenditionFrontMatterV1 `yaml:"docbank"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data[len(opening):yamlEnd]))
	decoder.KnownFields(true)
	if err := decoder.Decode(&envelope); err != nil {
		return RenditionFrontMatterV1{}, nil, fmt.Errorf("decoding rendition frontmatter: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition frontmatter contains multiple YAML documents")
	}
	frontmatter, body := envelope.Docbank, data[headerLength:]
	if len(body) == 0 || !utf8.Valid(body) {
		return RenditionFrontMatterV1{}, nil, errors.New("rendition Markdown body is empty or not UTF-8")
	}
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
	if value.Contract != RenditionMarkdownContractV1 || value.Source.Format == "" ||
		value.Source.MediaType == "" || value.Rendition.NormalizedEvidenceContract != NormalizedEvidenceContractV1 ||
		!validEvidenceCompleteness(value.Rendition.Completeness) || !validEvidenceUnitKind(value.Document.UnitKind) ||
		value.Document.UnitCount < 1 || value.Navigation.OffsetBase != RenditionNavigationOffsetBody ||
		len(value.Navigation.Entries) > maxRenditionNavigationEntries ||
		len(value.Navigation.Entries) > value.Document.UnitCount {
		return errors.New("rendition frontmatter contract is invalid")
	}
	if got := checksumBytes(body); got != value.Rendition.BodySHA256 {
		return fmt.Errorf("rendition body SHA-256 %s differs from frontmatter %s", got, value.Rendition.BodySHA256)
	}
	seen := make(map[string]struct{}, len(value.Navigation.Entries))
	priorByte := -1
	for _, entry := range value.Navigation.Entries {
		if entry.Key == "" || !renditionFrontMatterLocatorKind(entry.Kind) || entry.Byte < 0 ||
			entry.Byte >= len(body) || entry.Byte < priorByte || !utf8.RuneStart(body[entry.Byte]) ||
			entry.Line != 1+bytes.Count(body[:entry.Byte], []byte{'\n'}) {
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

func renditionNavigation(rendition RenditionV1) (RenditionMarkdownNavigationV1, error) {
	navigation := RenditionMarkdownNavigationV1{OffsetBase: RenditionNavigationOffsetBody,
		Complete: len(rendition.Units) <= maxRenditionNavigationEntries}
	maximum := min(len(rendition.Units), maxRenditionNavigationEntries)
	navigation.Entries = make([]RenditionNavigationEntryV1, 0, maximum)
	cursor := 0
	for _, unit := range rendition.Units[:maximum] {
		if unit.Text == "" {
			continue
		}
		relative := bytes.Index(rendition.Markdown[cursor:], []byte(unit.Text))
		if relative < 0 {
			return RenditionMarkdownNavigationV1{}, errors.New("rendition unit is absent from its Markdown body")
		}
		offset := cursor + relative
		title := unit.Locator.Name
		if len(unit.HeadingPath) != 0 {
			title = unit.HeadingPath[len(unit.HeadingPath)-1]
		}
		navigation.Entries = append(navigation.Entries, RenditionNavigationEntryV1{
			Key: unit.EvidenceUnitID, Kind: unit.Locator.Kind, Title: title,
			Line: 1 + bytes.Count(rendition.Markdown[:offset], []byte{'\n'}), Byte: offset})
		cursor = offset + len(unit.Text)
	}
	return navigation, nil
}

func marshalRenditionFrontMatterV1(value RenditionFrontMatterV1) ([]byte, error) {
	if value.Contract != RenditionMarkdownContractV1 || value.Navigation.OffsetBase != RenditionNavigationOffsetBody ||
		value.Document.UnitCount < 1 || len(value.Navigation.Entries) > maxRenditionNavigationEntries {
		return nil, errors.New("rendition frontmatter is invalid")
	}
	var builder strings.Builder
	builder.WriteString("---\ndocbank:\n")
	writeYAMLString(&builder, 2, "contract", value.Contract)
	builder.WriteString("  source:\n")
	writeYAMLString(&builder, 4, "sha256", value.Source.SHA256)
	writeYAMLString(&builder, 4, "format", value.Source.Format)
	writeYAMLString(&builder, 4, "media_type", value.Source.MediaType)
	builder.WriteString("  rendition:\n")
	writeYAMLString(&builder, 4, "build_id", value.Rendition.BuildID)
	writeYAMLString(&builder, 4, "rendition_request_fingerprint", value.Rendition.RenditionRequestFingerprint)
	writeYAMLString(&builder, 4, "evidence_lexical_fingerprint", value.Rendition.EvidenceLexicalFingerprint)
	writeYAMLString(&builder, 4, "normalized_evidence_contract", value.Rendition.NormalizedEvidenceContract)
	writeYAMLString(&builder, 4, "body_sha256", value.Rendition.BodySHA256)
	writeYAMLString(&builder, 4, "completeness", string(value.Rendition.Completeness))
	writeYAMLBool(&builder, 4, "truncated", value.Rendition.Truncated)
	builder.WriteString("  document:\n")
	if value.Document.Title != "" {
		writeYAMLString(&builder, 4, "title", value.Document.Title)
	}
	if value.Document.Language != "" {
		writeYAMLString(&builder, 4, "language", value.Document.Language)
	}
	writeYAMLString(&builder, 4, "unit_kind", string(value.Document.UnitKind))
	writeYAMLInt(&builder, 4, "unit_count", value.Document.UnitCount)
	builder.WriteString("  navigation:\n")
	writeYAMLString(&builder, 4, "offset_base", value.Navigation.OffsetBase)
	writeYAMLBool(&builder, 4, "complete", value.Navigation.Complete)
	builder.WriteString("    entries:\n")
	for _, entry := range value.Navigation.Entries {
		builder.WriteString("      - key: ")
		builder.WriteString(strconv.Quote(entry.Key))
		builder.WriteByte('\n')
		writeYAMLString(&builder, 8, "kind", string(entry.Kind))
		if entry.Title != "" {
			writeYAMLString(&builder, 8, "title", entry.Title)
		}
		writeYAMLInt(&builder, 8, "line", entry.Line)
		writeYAMLInt(&builder, 8, "byte", entry.Byte)
	}
	builder.WriteString("---\n")
	encoded := builder.String()
	if !utf8.ValidString(encoded) {
		return nil, errors.New("rendition frontmatter is not UTF-8")
	}
	return []byte(encoded), nil
}

func writeYAMLString(builder *strings.Builder, indent int, key, value string) {
	builder.WriteString(strings.Repeat(" ", indent))
	builder.WriteString(key)
	builder.WriteString(": ")
	builder.WriteString(strconv.Quote(value))
	builder.WriteByte('\n')
}

func writeYAMLBool(builder *strings.Builder, indent int, key string, value bool) {
	builder.WriteString(strings.Repeat(" ", indent))
	builder.WriteString(key)
	builder.WriteString(": ")
	builder.WriteString(strconv.FormatBool(value))
	builder.WriteByte('\n')
}

func writeYAMLInt(builder *strings.Builder, indent int, key string, value int) {
	builder.WriteString(strings.Repeat(" ", indent))
	builder.WriteString(key)
	builder.WriteString(": ")
	builder.WriteString(strconv.Itoa(value))
	builder.WriteByte('\n')
}
