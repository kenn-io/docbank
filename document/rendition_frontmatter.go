package document

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
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
	if err := validateRenditionFrontMatterV1(frontmatter, rendition.Markdown); err != nil {
		return RenditionV1{}, RenditionFrontMatterV1{}, err
	}
	header, err := marshalRenditionFrontMatterV1(frontmatter)
	if err != nil {
		return RenditionV1{}, RenditionFrontMatterV1{}, err
	}
	body := slices.Clone(rendition.Markdown)
	rendition.Markdown = make([]byte, 0, len(header)+len(body))
	rendition.Markdown = append(rendition.Markdown, header...)
	rendition.Markdown = append(rendition.Markdown, body...)
	rendition.MarkdownChecksum = checksumBytes(rendition.Markdown)
	rendition.Checksum = renditionChecksum(rendition)
	return rendition, frontmatter, nil
}

func renditionNavigation(rendition RenditionV1) (RenditionMarkdownNavigationV1, error) {
	navigation := RenditionMarkdownNavigationV1{OffsetBase: RenditionNavigationOffsetBody,
		Complete: len(rendition.Units) <= maxRenditionNavigationEntries}
	maximum := min(len(rendition.Units), maxRenditionNavigationEntries)
	navigation.Entries = make([]RenditionNavigationEntryV1, 0, maximum)
	cursor, line := 0, 1
	for _, unit := range rendition.Units[:maximum] {
		if unit.Text == "" {
			continue
		}
		relative := bytes.Index(rendition.Markdown[cursor:], []byte(unit.Text))
		if relative < 0 {
			return RenditionMarkdownNavigationV1{}, errors.New("rendition unit is absent from its Markdown body")
		}
		offset := cursor + relative
		line += bytes.Count(rendition.Markdown[cursor:offset], []byte{'\n'})
		title := unit.Locator.Name
		if len(unit.HeadingPath) != 0 {
			title = unit.HeadingPath[len(unit.HeadingPath)-1]
		}
		navigation.Entries = append(navigation.Entries, RenditionNavigationEntryV1{
			Key: unit.EvidenceUnitID, Kind: unit.Locator.Kind, Title: title, Line: line, Byte: offset})
		cursor = offset + len(unit.Text)
		line += bytes.Count(rendition.Markdown[offset:cursor], []byte{'\n'})
	}
	return navigation, nil
}

func marshalRenditionFrontMatterV1(value RenditionFrontMatterV1) ([]byte, error) {
	if value.Contract != RenditionMarkdownContractV1 || value.Navigation.OffsetBase != RenditionNavigationOffsetBody ||
		value.Document.UnitCount < 1 || len(value.Navigation.Entries) > maxRenditionNavigationEntries {
		return nil, errors.New("rendition frontmatter is invalid")
	}
	var writer renditionFrontMatterWriter
	writer.writeString("---\ndocbank:\n")
	writer.writeYAMLString(2, "contract", value.Contract)
	writer.writeString("  source:\n")
	writer.writeYAMLString(4, "sha256", value.Source.SHA256)
	writer.writeYAMLString(4, "format", value.Source.Format)
	writer.writeYAMLString(4, "media_type", value.Source.MediaType)
	writer.writeString("  rendition:\n")
	writer.writeYAMLString(4, "build_id", value.Rendition.BuildID)
	writer.writeYAMLString(4, "rendition_request_fingerprint", value.Rendition.RenditionRequestFingerprint)
	writer.writeYAMLString(4, "evidence_lexical_fingerprint", value.Rendition.EvidenceLexicalFingerprint)
	writer.writeYAMLString(4, "normalized_evidence_contract", value.Rendition.NormalizedEvidenceContract)
	writer.writeYAMLString(4, "body_sha256", value.Rendition.BodySHA256)
	writer.writeYAMLString(4, "completeness", string(value.Rendition.Completeness))
	writer.writeYAMLBool(4, "truncated", value.Rendition.Truncated)
	writer.writeString("  document:\n")
	if value.Document.Title != "" {
		writer.writeYAMLString(4, "title", value.Document.Title)
	}
	if value.Document.Language != "" {
		writer.writeYAMLString(4, "language", value.Document.Language)
	}
	writer.writeYAMLString(4, "unit_kind", string(value.Document.UnitKind))
	writer.writeYAMLInt(4, "unit_count", value.Document.UnitCount)
	writer.writeString("  navigation:\n")
	writer.writeYAMLString(4, "offset_base", value.Navigation.OffsetBase)
	writer.writeYAMLBool(4, "complete", value.Navigation.Complete)
	writer.writeString("    entries:\n")
	for _, entry := range value.Navigation.Entries {
		writer.writeYAMLStringPrefix("      - key: ", entry.Key)
		writer.writeYAMLString(8, "kind", string(entry.Kind))
		if entry.Title != "" {
			writer.writeYAMLString(8, "title", entry.Title)
		}
		writer.writeYAMLInt(8, "line", entry.Line)
		writer.writeYAMLInt(8, "byte", entry.Byte)
	}
	writer.writeString("---\n")
	if writer.err != nil {
		return nil, writer.err
	}
	encoded := writer.builder.String()
	if !utf8.ValidString(encoded) {
		return nil, errors.New("rendition frontmatter is not UTF-8")
	}
	return []byte(encoded), nil
}

type renditionFrontMatterWriter struct {
	builder strings.Builder
	err     error
}

func (writer *renditionFrontMatterWriter) writeString(value string) {
	if writer.err != nil {
		return
	}
	if len(value) > maxRenditionFrontMatterBytes-writer.builder.Len() {
		writer.err = errors.New("rendition frontmatter exceeds its byte bound")
		return
	}
	_, writer.err = writer.builder.WriteString(value)
}

func (writer *renditionFrontMatterWriter) writeYAMLString(indent int, key, value string) {
	prefix := strings.Repeat(" ", indent) + key + ": "
	writer.writeYAMLStringPrefix(prefix, value)
}

func (writer *renditionFrontMatterWriter) writeYAMLStringPrefix(prefix, value string) {
	if writer.err != nil {
		return
	}
	minimumSyntaxBytes := len(prefix) + len(`""`) + 1
	remaining := maxRenditionFrontMatterBytes - writer.builder.Len()
	if minimumSyntaxBytes > remaining || len(value) > remaining-minimumSyntaxBytes {
		writer.err = errors.New("rendition frontmatter exceeds its byte bound")
		return
	}
	quoted := strconv.Quote(value)
	if len(prefix)+len(quoted)+1 > remaining {
		writer.err = errors.New("rendition frontmatter exceeds its byte bound")
		return
	}
	writer.writeString(prefix)
	writer.writeString(quoted)
	writer.writeString("\n")
}

func (writer *renditionFrontMatterWriter) writeYAMLBool(indent int, key string, value bool) {
	writer.writeString(strings.Repeat(" ", indent))
	writer.writeString(key)
	writer.writeString(": ")
	writer.writeString(strconv.FormatBool(value))
	writer.writeString("\n")
}

func (writer *renditionFrontMatterWriter) writeYAMLInt(indent int, key string, value int) {
	writer.writeString(strings.Repeat(" ", indent))
	writer.writeString(key)
	writer.writeString(": ")
	writer.writeString(strconv.Itoa(value))
	writer.writeString("\n")
}
