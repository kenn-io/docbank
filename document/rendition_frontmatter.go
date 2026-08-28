package document

import (
	"bytes"
	"errors"
	"fmt"
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
	SHA256    string
	Format    string
	MediaType string
}

type RenditionMarkdownBuildV1 struct {
	BuildID                     string
	RenditionRequestFingerprint string
	EvidenceLexicalFingerprint  string
	NormalizedEvidenceContract  string
	BodySHA256                  string
	Completeness                EvidenceCompleteness
	Truncated                   bool
}

type RenditionMarkdownDocumentV1 struct {
	Title     string
	Language  string
	UnitKind  EvidenceUnitKind
	UnitCount int
}

type RenditionNavigationEntryV1 struct {
	Key   string
	Kind  EvidenceLocatorKind
	Title string
	Line  int
	Byte  int
}

type RenditionMarkdownNavigationV1 struct {
	OffsetBase string
	Complete   bool
	Entries    []RenditionNavigationEntryV1
}

type RenditionFrontMatterV1 struct {
	Contract   string
	Source     RenditionMarkdownSourceV1
	Rendition  RenditionMarkdownBuildV1
	Document   RenditionMarkdownDocumentV1
	Navigation RenditionMarkdownNavigationV1
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
