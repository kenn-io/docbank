package report

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	maxRawDateBytes   = 4 << 10
	maxAdaptedFields  = 200000
	maxDateFieldBytes = 16 << 10
)

// AdaptDateFields retains source date assertions, including values rejected by
// the existing event derivation. Callers must supply disclosure-filtered fields
// from one frozen read; this function never reads or rebuilds live metadata.
func AdaptDateFields(ctx context.Context, budget Budget, fields []RawDateField) (_ []DateCandidate, err error) {
	if budget == nil {
		return nil, errors.New("missing report budget")
	}
	if len(fields) > maxAdaptedFields {
		return nil, fmt.Errorf("%w: too many date fields", ErrReportLimit)
	}
	result := make([]DateCandidate, 0, len(fields))
	releases := make([]func(), 0, len(fields))
	defer func() {
		if err != nil {
			for _, release := range releases {
				release()
			}
		}
	}()
	for _, field := range fields {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		if field.Sensitive {
			return nil, errors.New("sensitive date field has not passed disclosure filtering")
		}
		if len(field.Raw) > maxRawDateBytes || len(field.Normalized) > maxRawDateBytes {
			return nil, fmt.Errorf("%w: date field exceeds raw value limit", ErrReportLimit)
		}
		fieldBytes := len(field.Namespace) + len(field.SourceField) + len(field.Key) +
			len(field.Raw) + len(field.Normalized) + len(field.Timezone) + len(field.Precision) +
			len(field.ClaimBasis) + len(field.GenerationID) + len(field.GenerationSHA256)
		if fieldBytes > maxDateFieldBytes {
			return nil, fmt.Errorf("%w: date field exceeds evidence limit", ErrReportLimit)
		}
		if !validSHA256(field.GenerationSHA256) {
			return nil, errors.New("date field lacks a valid generation checksum")
		}
		release, reserveErr := budget.Reserve(ctx, int64(2048+fieldBytes))
		if reserveErr != nil {
			return nil, reserveErr
		}
		releases = append(releases, release)
		role, sourceClass := classifyRawDateField(field)
		candidate := DateCandidate{
			Document: field.Document,
			Role:     role, SourceClass: sourceClass,
			Raw: strings.Clone(field.Raw), Value: strings.Clone(field.Normalized),
			Precision: strings.Clone(field.Precision), Timezone: strings.Clone(field.Timezone),
			Confidence: "source_asserted", SourceNamespace: strings.Clone(field.Namespace),
			SourceField: strings.Clone(field.SourceField), ClaimBasis: strings.Clone(field.ClaimBasis),
			Locator: Locator{EvidenceID: strings.Clone(field.GenerationID), EvidenceSHA256: strings.Clone(field.GenerationSHA256)},
		}
		candidate.ID = dateCandidateID(field, role)
		candidate.Rejection = rawFieldRejection(candidate)
		result = append(result, candidate)
	}
	return result, nil
}

func classifyRawDateField(field RawDateField) (role, sourceClass string) {
	switch field.Namespace + "/" + field.SourceField {
	case "email/Date":
		if field.Key == "email.sent" {
			return "sent", "native"
		}
	case "pdf.info/CreationDate", "xmp/CreateDate", "office.core/created", "office.core/dcterms:created":
		if field.Key == "created" {
			return "created", "native"
		}
	case "image.exif/DateTimeOriginal", "media.id3/TDRC", "media.container/mvhd.CreationTime":
		if field.Key == "created" {
			return "captured", "native"
		}
	case "filesystem/birth_time":
		if field.ClaimBasis == "original_source" {
			return "created", "source_metadata"
		}
	case "docbank/added_at":
		return "imported", "vault_addition"
	case "docbank/recorded_at":
		return "vault_recorded", "vault_observation"
	}
	return "unclassified", "source_metadata"
}

func rawFieldRejection(candidate DateCandidate) string {
	value := candidate.Value
	if value == "" {
		value = candidate.Raw
	}
	if value == "" {
		return "missing_date_value"
	}
	if _, err := parseISODate(value); err == nil {
		return ""
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return ""
	}
	if strings.Count(value, "/") == 2 {
		return "ambiguous_numeric_date"
	}
	if len(value) > 10 && strings.Contains(value, "T") && !strings.HasSuffix(value, "Z") &&
		!strings.ContainsAny(value[10:], "+-") {
		return "timezone_omitted"
	}
	return "invalid_calendar_date"
}

func dateCandidateID(field RawDateField, role string) string {
	hash := sha256.New()
	for _, value := range []string{
		strconv.FormatInt(field.Document.NodeID, 10), field.Document.VersionID, field.Document.SHA256,
		role, field.Namespace, field.SourceField, field.Key, field.Raw,
		field.GenerationID, field.GenerationSHA256,
	} {
		_, _ = fmt.Fprintf(hash, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
