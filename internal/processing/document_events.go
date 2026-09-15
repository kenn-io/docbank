package processing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/store"
)

const documentEventsDeriverDescriptor = store.DocumentEventsDeriverDescriptor

// DocumentEventsDeriverFingerprint aliases the single recipe identity owned
// by the durable store catalog.
const DocumentEventsDeriverFingerprint = store.DocumentEventsDeriverFingerprint

// DocumentEventInput is the complete immutable evidence snapshot used by the
// initial event derivation recipe.
type DocumentEventInput = store.DocumentEventEvidenceSnapshot

var eventDocumentKind = store.DocumentEventKindForMIMEType

// DeriveDocumentEvents runs the fixed metadata, content-version, and exact
// provenance-binding adapters and returns the canonical record value plus its
// checksum.
func DeriveDocumentEvents(input DocumentEventInput) (document.DocumentEventsV1, string, error) {
	if err := validateDocumentEventDerivationBounds(input); err != nil {
		return document.DocumentEventsV1{}, "", err
	}
	record := document.DocumentEventsV1{
		VaultUID: input.VaultUID, ContentVersionID: input.Target.ContentVersionID,
		ContractVersion: document.DocumentEventsContractV1,
		DocumentKind:    eventDocumentKind(input.Target.MIMEType),
		Diagnostics:     []document.DocumentEventDiagnosticV1{},
		Events:          []document.DocumentEventV1{},
		Primaries:       []document.DocumentEventPrimaryV1{},
		Sources:         []document.DocumentEventSourceV1{},
	}

	vaultEvent, source, err := versionAdapter(input)
	if err != nil {
		return document.DocumentEventsV1{}, "", err
	}
	record.Events = append(record.Events, vaultEvent)
	record.Sources = append(record.Sources, source)

	metadataEvents, metadataActors, metadataSource, diagnostics, err := metadataAdapter(input)
	if err != nil {
		return document.DocumentEventsV1{}, "", err
	}
	record.Diagnostics = append(record.Diagnostics, diagnostics...)
	if metadataSource != nil {
		record.Sources = append(record.Sources, *metadataSource)
	}
	record.Events = append(record.Events, metadataEvents...)
	attachMetadataActors(record.Events, metadataActors)

	provenanceEvents, provenanceSources, err := provenanceAdapter(input)
	if err != nil {
		return document.DocumentEventsV1{}, "", err
	}
	record.Events = append(record.Events, provenanceEvents...)
	record.Sources = append(record.Sources, provenanceSources...)

	for _, disclosure := range []string{"full", "safe"} {
		if primary, ok := document.SelectPrimaryEvent(record.Events, record.DocumentKind, "vault", disclosure); ok {
			record.Primaries = append(record.Primaries, primary)
		}
	}
	canonical, checksum, err := document.MarshalDocumentEventsV1(record)
	if err != nil {
		if errors.Is(err, document.ErrDocumentEventsOutputBound) {
			err = errors.Join(store.ErrDocumentEventEvidenceUnavailable, err)
		}
		return document.DocumentEventsV1{}, "", err
	}
	record, _, err = document.DecodeDocumentEventsV1(canonical)
	return record, checksum, err
}

func metadataDateKind(field document.SourceMetadataFieldV1) (document.DateKind, bool) {
	if field.Key == "created" {
		switch field.Namespace + "/" + field.SourceField {
		case "image.exif/DateTimeOriginal", "media.id3/TDRC", "media.container/mvhd.CreationTime":
			return "captured", true
		}
	}
	if match := calendarComponentDate.FindStringSubmatch(field.Key); match != nil {
		if match[1] == "start" {
			return "started", true
		}
		return "ended", true
	}
	kinds := map[string]document.DateKind{
		"created":                  "created",
		"modified":                 "modified",
		"email.sent":               "sent",
		"calendar.start":           "started",
		"calendar.end":             "ended",
		"image.exif.gps_timestamp": "captured",
	}
	kind, ok := kinds[strings.TrimSuffix(field.Key, ".raw")]
	return kind, ok
}

func metadataAdapter(input DocumentEventInput) (
	[]document.DocumentEventV1,
	[]metadataActorClaim,
	*document.DocumentEventSourceV1,
	[]document.DocumentEventDiagnosticV1,
	error,
) {
	if input.MetadataGeneration.GenerationID == "" {
		return nil, nil, nil, nil, nil
	}
	generation := input.MetadataGeneration
	source := &document.DocumentEventSourceV1{
		EvidenceKind: "source_metadata", EvidenceID: generation.GenerationID,
		EvidenceSHA256: generation.Checksum,
	}
	events := make([]document.DocumentEventV1, 0)
	actors := make([]metadataActorClaim, 0)
	diagnostics := make([]document.DocumentEventDiagnosticV1, 0)
	for ordinal, field := range input.Metadata.Fields {
		sourceKey := fmt.Sprintf("metadata/%s/%s/%d", generation.GenerationID, field.Key, ordinal)
		kind, ok := metadataDateKind(field)
		if field.Value.Kind == document.SourceMetadataTimestamp ||
			(ok && field.Value.Kind == document.SourceMetadataString && strings.HasSuffix(field.Key, ".raw")) {
			if !ok {
				if !field.Sensitive {
					diagnostics = append(diagnostics, document.DocumentEventDiagnosticV1{
						Code: "date_kind_unknown", Detail: "timestamp field has no event mapping", SourceKey: sourceKey,
					})
				}
				continue
			}
			event, err := metadataDateEvent(field)
			if err != nil {
				if !field.Sensitive {
					diagnostics = append(diagnostics, document.DocumentEventDiagnosticV1{
						Code: "date_unparseable", Detail: "retained timestamp could not be converted", SourceKey: sourceKey,
					})
				}
				continue
			}
			event.DateKind = kind
			event.SourceKey = sourceKey
			if calendarComponentDate.MatchString(field.Key) {
				event.SourceKindRaw = field.SourceField
			}
			event.ClaimBasis = "source_asserted"
			event.ParseConfidence = "exact"
			event.EvidenceKind = source.EvidenceKind
			event.EvidenceID = source.EvidenceID
			event.EvidenceSHA256 = source.EvidenceSHA256
			event.EvidenceLocator = "field/" + field.Key
			event.Sensitive = field.Sensitive
			setEventKeys(input.VaultUID, input.Target.ContentVersionID, &event)
			events = append(events, event)
			continue
		}
		fieldActors, err := metadataActorsForField(field)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for index := range fieldActors {
			fieldActors[index].actor.EvidenceKind = source.EvidenceKind
			fieldActors[index].actor.EvidenceID = source.EvidenceID
		}
		if len(fieldActors) > document.MaxDocumentEventActors-len(actors) {
			return nil, nil, nil, nil, fmt.Errorf("metadata contains more than %d actor associations: %w",
				document.MaxDocumentEventActors, store.ErrDocumentEventEvidenceUnavailable)
		}
		actors = append(actors, fieldActors...)
	}
	return events, actors, source, diagnostics, nil
}

func validateDocumentEventDerivationBounds(input DocumentEventInput) error {
	eventCount := 1
	actorCount := 0
	for _, field := range input.Metadata.Fields {
		if field.Value.Kind == document.SourceMetadataTimestamp ||
			(field.Value.Kind == document.SourceMetadataString && strings.HasSuffix(field.Key, ".raw")) {
			if _, ok := metadataDateKind(field); !ok {
				continue
			}
			if _, err := metadataDateEvent(field); err == nil {
				eventCount++
				if eventCount > document.MaxDocumentEvents {
					return fmt.Errorf("metadata and provenance contain more than %d events: %w",
						document.MaxDocumentEvents, store.ErrDocumentEventEvidenceUnavailable)
				}
			}
			continue
		}
		if field.Key == "creators" && field.Value.Kind == document.SourceMetadataStringList {
			for _, claim := range field.Value.Strings {
				if len(claim) > document.MaxDocumentEventActorClaimBytes {
					return fmt.Errorf("metadata actor claim exceeds %d bytes: %w",
						document.MaxDocumentEventActorClaimBytes, store.ErrDocumentEventEvidenceUnavailable)
				}
			}
			actorCount += len(field.Value.Strings)
			if actorCount > document.MaxDocumentEventActors {
				return fmt.Errorf("metadata contains more than %d actor associations: %w",
					document.MaxDocumentEventActors, store.ErrDocumentEventEvidenceUnavailable)
			}
			continue
		}
		_, emailActor := metadataEmailActorRole(field.Key)
		if emailActor && field.Value.Kind == document.SourceMetadataString &&
			field.Value.String != nil && len(*field.Value.String) > document.MaxDocumentEventActorClaimBytes {
			return fmt.Errorf("metadata actor claim exceeds %d bytes: %w",
				document.MaxDocumentEventActorClaimBytes, store.ErrDocumentEventEvidenceUnavailable)
		}
	}
	for _, bound := range input.Bindings {
		eventCount++
		if bound.OriginalMTime != nil {
			eventCount++
		}
		if eventCount > document.MaxDocumentEvents {
			return fmt.Errorf("metadata and provenance contain more than %d events: %w",
				document.MaxDocumentEvents, store.ErrDocumentEventEvidenceUnavailable)
		}
	}
	return nil
}

func versionAdapter(input DocumentEventInput) (document.DocumentEventV1, document.DocumentEventSourceV1, error) {
	evidenceSHA := digestStrings("content-version", input.Target.ContentVersionID,
		strconv.FormatInt(input.Target.NodeID, 10), input.Target.BlobHash,
		strconv.FormatInt(input.Target.Size, 10), input.Target.MIMEType, input.Target.RecordedAt)
	event, err := rfc3339Event(input.Target.RecordedAt)
	if err != nil {
		return document.DocumentEventV1{}, document.DocumentEventSourceV1{},
			fmt.Errorf("converting content version recorded time: %w", err)
	}
	event.DateKind = "vault_recorded"
	event.SourceKey = "version/" + input.Target.ContentVersionID + "/recorded_at"
	event.RawValue = input.Target.RecordedAt
	event.ClaimBasis = "docbank_observed"
	event.ParseConfidence = "exact"
	event.EvidenceKind = "content_version"
	event.EvidenceID = input.Target.ContentVersionID
	event.EvidenceSHA256 = evidenceSHA
	event.EvidenceLocator = "recorded_at"
	setEventKeys(input.VaultUID, input.Target.ContentVersionID, &event)
	return event, document.DocumentEventSourceV1{
		EvidenceKind: event.EvidenceKind, EvidenceID: event.EvidenceID,
		EvidenceSHA256: event.EvidenceSHA256,
	}, nil
}

func provenanceAdapter(input DocumentEventInput) ([]document.DocumentEventV1, []document.DocumentEventSourceV1, error) {
	events := make([]document.DocumentEventV1, 0, len(input.Bindings)*2)
	sources := make([]document.DocumentEventSourceV1, 0, len(input.Bindings))
	for _, bound := range input.Bindings {
		source := document.DocumentEventSourceV1{
			EvidenceKind: "provenance_binding", EvidenceID: bound.Binding.ProvenanceIdentity,
			EvidenceSHA256: bound.EvidenceSHA256,
		}
		sources = append(sources, source)
		claims := []struct {
			field string
			kind  document.DateKind
			value *string
		}{
			{field: "observed_at", kind: "imported", value: &bound.Binding.ObservedAt},
			{field: "original_mtime", kind: "modified", value: bound.OriginalMTime},
		}
		for _, claim := range claims {
			if claim.value == nil {
				continue
			}
			event, err := rfc3339Event(*claim.value)
			if err != nil {
				return nil, nil, fmt.Errorf("converting bound provenance %s: %w", claim.field, err)
			}
			event.DateKind = claim.kind
			event.SourceKey = "provenance/" + bound.Binding.ProvenanceIdentity + "/" + claim.field
			event.RawValue = *claim.value
			event.ClaimBasis = "docbank_observed"
			event.ParseConfidence = "exact"
			event.EvidenceKind = source.EvidenceKind
			event.EvidenceID = source.EvidenceID
			event.EvidenceSHA256 = source.EvidenceSHA256
			event.EvidenceLocator = claim.field
			setEventKeys(input.VaultUID, input.Target.ContentVersionID, &event)
			events = append(events, event)
		}
	}
	return events, sources, nil
}

func setEventKeys(vaultUID, versionID string, event *document.DocumentEventV1) {
	event.EventID = document.DocumentEventID(vaultUID, versionID, event.SourceKey,
		event.DateKind, event.DateValue, event.Precision, event.TimezoneKind,
		event.EvidenceSHA256)
}

func digestStrings(values ...string) string {
	h := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintf(h, "%d:%s", len(value), value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
