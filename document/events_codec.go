package document

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/canonical"
	"golang.org/x/net/idna"
)

// ErrDocumentEventsOutputBound reports that an otherwise valid event record
// exceeds the aggregate text or final canonical-byte limit.
var ErrDocumentEventsOutputBound = errors.New("document events exceed an output bound")

// MarshalDocumentEventsV1 validates and returns the single durable byte form
// of a document-events/v1 record and its SHA-256 checksum.
func MarshalDocumentEventsV1(value DocumentEventsV1) ([]byte, string, error) {
	if err := validateDocumentEventsV1(value); err != nil {
		return nil, "", err
	}
	value = cloneDocumentEventsV1(value)
	slices.SortFunc(value.Events, func(a, b DocumentEventV1) int {
		if order := strings.Compare(a.SourceKey, b.SourceKey); order != 0 {
			return order
		}
		return strings.Compare(string(a.DateKind), string(b.DateKind))
	})
	for index := range value.Events {
		slices.SortFunc(value.Events[index].Actors, func(a, b DocumentEventActorV1) int {
			if order := strings.Compare(string(a.Role), string(b.Role)); order != 0 {
				return order
			}
			return cmp.Compare(a.Ordinal, b.Ordinal)
		})
	}
	slices.SortFunc(value.Primaries, func(a, b DocumentEventPrimaryV1) int {
		if order := strings.Compare(a.ScopeClass, b.ScopeClass); order != 0 {
			return order
		}
		return strings.Compare(a.Disclosure, b.Disclosure)
	})
	slices.SortFunc(value.Sources, func(a, b DocumentEventSourceV1) int {
		if order := strings.Compare(string(a.EvidenceKind), string(b.EvidenceKind)); order != 0 {
			return order
		}
		if order := strings.Compare(a.EvidenceID, b.EvidenceID); order != 0 {
			return order
		}
		return strings.Compare(a.EvidenceSHA256, b.EvidenceSHA256)
	})
	slices.SortFunc(value.Diagnostics, func(a, b DocumentEventDiagnosticV1) int {
		if order := strings.Compare(a.SourceKey, b.SourceKey); order != 0 {
			return order
		}
		if order := strings.Compare(a.Code, b.Code); order != 0 {
			return order
		}
		return strings.Compare(a.Detail, b.Detail)
	})
	encoded, err := canonical.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("encoding document events: %w", err)
	}
	if len(encoded) > MaxDocumentEventsEncodedBytes {
		return nil, "", fmt.Errorf("%w: document events are longer than %d bytes",
			ErrDocumentEventsOutputBound, MaxDocumentEventsEncodedBytes)
	}
	return encoded, sha256Hex(encoded), nil
}

// DecodeDocumentEventsV1 accepts only the exact canonical bytes emitted by
// MarshalDocumentEventsV1.
func DecodeDocumentEventsV1(encoded []byte) (DocumentEventsV1, string, error) {
	if len(encoded) > MaxDocumentEventsEncodedBytes {
		return DocumentEventsV1{}, "", fmt.Errorf("decoding document events: input is longer than %d bytes", MaxDocumentEventsEncodedBytes)
	}
	value, err := canonical.Decode[DocumentEventsV1](encoded)
	if err != nil {
		return DocumentEventsV1{}, "", fmt.Errorf("decoding document events: %w", err)
	}
	canonicalBytes, checksum, err := MarshalDocumentEventsV1(value)
	if err != nil {
		return DocumentEventsV1{}, "", fmt.Errorf("decoding document events: %w", err)
	}
	if !bytes.Equal(encoded, canonicalBytes) {
		return DocumentEventsV1{}, "", errors.New("document event bytes are not canonical")
	}
	return value, checksum, nil
}

func validateDocumentEventsV1(value DocumentEventsV1) error {
	if value.ContractVersion != DocumentEventsContractV1 {
		return fmt.Errorf("document events contract version must be %q", DocumentEventsContractV1)
	}
	if !ValidDocumentKind(value.DocumentKind) {
		return errors.New("document events have invalid document kind")
	}
	if len(value.Events) > MaxDocumentEvents {
		return fmt.Errorf("document events contain more than %d events", MaxDocumentEvents)
	}
	if len(value.Primaries) > 2 {
		return errors.New("document events contain too many primaries")
	}
	// Each source contributes at least its 64-byte digest to the byte budget.
	// Sources may outnumber dates when evidence supplies only actors or diagnostics.
	if len(value.Sources) > MaxDocumentEventsEncodedBytes/64 || len(value.Diagnostics) > MaxDocumentEventsEncodedBytes/32 {
		return errors.New("document events contain too many child records")
	}
	if err := validStrings("document events", value.VaultUID, value.ContentVersionID, value.ContractVersion, string(value.DocumentKind)); err != nil {
		return err
	}
	if value.VaultUID == "" || value.ContentVersionID == "" {
		return errors.New("document event identity is empty")
	}

	type eventSlot struct {
		source string
		kind   DateKind
	}
	type sourceRef struct {
		kind       EventEvidenceKind
		id, digest string
	}
	eventIDs := make(map[string]struct{}, len(value.Events))
	eventSensitivity := make(map[string]bool, len(value.Events))
	slots := make(map[eventSlot]string, len(value.Events))
	sources := make(map[sourceRef]struct{}, len(value.Sources))
	sourceIdentities := make(map[documentEventSourceIdentity]struct{}, len(value.Sources))
	textBytes := len(value.VaultUID) + len(value.ContentVersionID) + len(value.ContractVersion)
	for index, source := range value.Sources {
		if !ValidEventEvidenceKind(source.EvidenceKind) || source.EvidenceID == "" || !canonical.IsSHA256Hex(source.EvidenceSHA256) {
			return fmt.Errorf("document event source %d is invalid", index)
		}
		if err := validStrings("document event source", string(source.EvidenceKind), source.EvidenceID, source.EvidenceSHA256); err != nil {
			return err
		}
		ref := sourceRef{source.EvidenceKind, source.EvidenceID, source.EvidenceSHA256}
		if _, exists := sources[ref]; exists {
			return fmt.Errorf("document event source %d is duplicated", index)
		}
		sources[ref] = struct{}{}
		sourceIdentities[documentEventSourceIdentity{source.EvidenceKind, source.EvidenceID}] = struct{}{}
		textBytes += len(source.EvidenceID) + len(source.EvidenceSHA256)
	}
	actorCount := 0
	for index, event := range value.Events {
		if !ValidDateKind(event.DateKind) || !ValidEventPrecision(event.Precision) || !ValidEventTimezoneKind(event.TimezoneKind) || !ValidEventEvidenceKind(event.EvidenceKind) {
			return fmt.Errorf("document event %d has an unknown vocabulary value", index)
		}
		if event.ClaimBasis != "source_asserted" && event.ClaimBasis != "docbank_observed" {
			return fmt.Errorf("document event %d has invalid claim basis", index)
		}
		if event.ParseConfidence != "exact" && event.ParseConfidence != "profile_interpreted" {
			return fmt.Errorf("document event %d has invalid parse confidence", index)
		}
		if len(event.RawValue) > MaxDocumentEventRawValueBytes {
			return fmt.Errorf("document event %d raw value is too long", index)
		}
		if len(event.EvidenceLocator) > MaxDocumentEventLocatorBytes {
			return fmt.Errorf("document event %d evidence locator is too long", index)
		}
		if event.SourceKey == "" || event.EvidenceID == "" || !canonical.IsSHA256Hex(event.EvidenceSHA256) {
			return fmt.Errorf("document event %d has invalid identity evidence", index)
		}
		stringsToCheck := []string{event.AxisKey, event.ClaimBasis, string(event.DateKind), event.DateValue, event.EventID, event.EvidenceID, string(event.EvidenceKind), event.EvidenceLocator, event.EvidenceSHA256, event.ParseConfidence, string(event.Precision), event.RawValue, event.SourceKey, event.SourceKindRaw, string(event.TimezoneKind), event.UTCKey, event.ZoneText}
		if err := validStrings("document event", stringsToCheck...); err != nil {
			return fmt.Errorf("document event %d: %w", index, err)
		}
		for _, text := range stringsToCheck {
			textBytes += len(text)
		}
		axis, err := EventAxisKey(event.DateValue, event.Precision, event.TimezoneKind, event.OffsetSeconds)
		if err != nil || event.AxisKey != axis {
			return fmt.Errorf("document event %d has invalid axis key", index)
		}
		utc, hasUTC, err := EventUTCKey(event.DateValue, event.Precision, event.TimezoneKind, event.OffsetSeconds)
		if err != nil || (hasUTC && event.UTCKey != utc) || (!hasUTC && event.UTCKey != "") {
			return fmt.Errorf("document event %d has invalid UTC key", index)
		}
		wantFraction := 0
		if event.Precision == "fraction" {
			wantFraction = len(event.DateValue) - strings.LastIndexByte(event.DateValue, '.') - 1
		}
		if event.FractionDigits != wantFraction {
			return fmt.Errorf("document event %d has invalid fraction digits", index)
		}
		wantID := DocumentEventID(value.VaultUID, value.ContentVersionID, event.SourceKey, event.DateKind, event.DateValue, event.Precision, event.TimezoneKind, event.EvidenceSHA256)
		if event.EventID != wantID {
			return fmt.Errorf("document event %d has invalid event ID", index)
		}
		slot := eventSlot{event.SourceKey, event.DateKind}
		if raw, exists := slots[slot]; exists {
			if raw != event.SourceKindRaw {
				return fmt.Errorf("document event %d conflicts with source kind raw", index)
			}
			return fmt.Errorf("document event %d duplicates a source slot", index)
		}
		slots[slot] = event.SourceKindRaw
		if _, exists := sources[sourceRef{event.EvidenceKind, event.EvidenceID, event.EvidenceSHA256}]; !exists {
			return fmt.Errorf("document event %d references an unknown source", index)
		}
		if _, exists := eventIDs[event.EventID]; exists {
			return fmt.Errorf("document event %d duplicates an event ID", index)
		}
		eventIDs[event.EventID] = struct{}{}
		eventSensitivity[event.EventID] = eventHasSensitiveEvidence(event)
		actorCount += len(event.Actors)
		if actorCount > MaxDocumentEventActors {
			return fmt.Errorf("document events contain more than %d actor associations", MaxDocumentEventActors)
		}
		actorTextBytes, err := validateDocumentEventActorsV1(index, event.Actors, sourceIdentities)
		if err != nil {
			return err
		}
		textBytes += actorTextBytes
	}
	type primarySlot struct{ scope, disclosure string }
	primarySlots := make(map[primarySlot]struct{}, len(value.Primaries))
	for index, primary := range value.Primaries {
		if primary.ScopeClass != "vault" {
			return fmt.Errorf("document event primary %d has invalid scope class", index)
		}
		if primary.Disclosure != "safe" && primary.Disclosure != "full" {
			return fmt.Errorf("document event primary %d has invalid disclosure", index)
		}
		if primary.RuleID != "primary-rule/v1" || primary.Reason == "" {
			return fmt.Errorf("document event primary %d has invalid rule", index)
		}
		if err := validStrings("document event primary", primary.EventID, primary.Reason, primary.RuleID, primary.ScopeClass, primary.Disclosure); err != nil {
			return err
		}
		_, exists := eventIDs[primary.EventID]
		if !exists {
			return fmt.Errorf("document event primary %d names an unknown event", index)
		}
		if primary.Disclosure == "safe" && eventSensitivity[primary.EventID] {
			return fmt.Errorf("document event primary %d points to a sensitive event", index)
		}
		key := primarySlot{primary.ScopeClass, primary.Disclosure}
		if _, exists := primarySlots[key]; exists {
			return fmt.Errorf("document event primary %d duplicates scope and disclosure", index)
		}
		primarySlots[key] = struct{}{}
		textBytes += len(primary.EventID) + len(primary.Reason) + len(primary.RuleID) + len(primary.ScopeClass) + len(primary.Disclosure)
	}
	for index, diagnostic := range value.Diagnostics {
		if err := validStrings("document event diagnostic", diagnostic.Code, diagnostic.Detail, diagnostic.SourceKey); err != nil {
			return fmt.Errorf("document event diagnostic %d: %w", index, err)
		}
		textBytes += len(diagnostic.Code) + len(diagnostic.Detail) + len(diagnostic.SourceKey)
	}
	if textBytes > MaxDocumentEventsEncodedBytes {
		return fmt.Errorf("%w: document event text is longer than %d bytes",
			ErrDocumentEventsOutputBound, MaxDocumentEventsEncodedBytes)
	}
	return nil
}

type documentEventSourceIdentity struct {
	kind EventEvidenceKind
	id   string
}

func validateDocumentEventActorsV1(
	index int,
	actors []DocumentEventActorV1,
	sourceIdentities map[documentEventSourceIdentity]struct{},
) (int, error) {
	textBytes := 0
	type actorSlot struct {
		role    EventRole
		ordinal int
	}
	actorSlots := make(map[actorSlot]struct{}, len(actors))
	for actorIndex, actor := range actors {
		if !ValidEventRole(actor.Role) || actor.Ordinal < 0 {
			return 0, fmt.Errorf("document event %d actor %d is invalid", index, actorIndex)
		}
		if (actor.EvidenceKind == "") != (actor.EvidenceID == "") ||
			(actor.EvidenceKind != "" && !ValidEventEvidenceKind(actor.EvidenceKind)) {
			return 0, fmt.Errorf("document event %d actor %d has invalid actor evidence", index, actorIndex)
		}
		if actor.EvidenceKind != "" {
			if _, exists := sourceIdentities[documentEventSourceIdentity{actor.EvidenceKind, actor.EvidenceID}]; !exists {
				return 0, fmt.Errorf("document event %d actor %d references unknown actor evidence", index, actorIndex)
			}
		}
		if len(actor.Claim) > MaxDocumentEventActorClaimBytes || len(actor.ActorKey) > MaxActorKeyBytes {
			return 0, fmt.Errorf("document event %d actor %d exceeds a bound", index, actorIndex)
		}
		if err := validStrings("document event actor", actor.ActorKey, actor.Address, actor.Claim,
			actor.DisplayName, actor.EvidenceID, string(actor.EvidenceKind), string(actor.Role)); err != nil {
			return 0, err
		}
		textBytes += len(actor.ActorKey) + len(actor.Address) + len(actor.Claim) + len(actor.DisplayName) +
			len(actor.EvidenceID) + len(actor.EvidenceKind)
		if actor.ActorKey != "" {
			if err := ValidateActorKeyV1(actor.ActorKey); err != nil {
				return 0, fmt.Errorf("document event %d actor %d has an invalid actor key", index, actorIndex)
			}
		}
		key := actorSlot{actor.Role, actor.Ordinal}
		if _, exists := actorSlots[key]; exists {
			return 0, fmt.Errorf("document event %d duplicates actor role and ordinal", index)
		}
		actorSlots[key] = struct{}{}
	}
	return textBytes, nil
}

func validStrings(field string, values ...string) error {
	for _, value := range values {
		if !utf8.ValidString(value) {
			return fmt.Errorf("%s is not valid UTF-8", field)
		}
	}
	return nil
}

func cloneDocumentEventsV1(value DocumentEventsV1) DocumentEventsV1 {
	value.Events = slices.Clone(value.Events)
	value.Primaries = slices.Clone(value.Primaries)
	value.Sources = slices.Clone(value.Sources)
	value.Diagnostics = slices.Clone(value.Diagnostics)
	if value.Events == nil {
		value.Events = []DocumentEventV1{}
	}
	if value.Primaries == nil {
		value.Primaries = []DocumentEventPrimaryV1{}
	}
	if value.Sources == nil {
		value.Sources = []DocumentEventSourceV1{}
	}
	if value.Diagnostics == nil {
		value.Diagnostics = []DocumentEventDiagnosticV1{}
	}
	for index := range value.Events {
		value.Events[index].Actors = slices.Clone(value.Events[index].Actors)
		if value.Events[index].Actors == nil {
			value.Events[index].Actors = []DocumentEventActorV1{}
		}
		if value.Events[index].OffsetSeconds != nil {
			offset := *value.Events[index].OffsetSeconds
			value.Events[index].OffsetSeconds = &offset
		}
	}
	return value
}

// ValidateActorKeyV1 checks a persisted key without applying person-entry policy.
func ValidateActorKeyV1(key string) error {
	if len(key) > MaxActorKeyBytes {
		return errors.New("actor key exceeds the byte limit")
	}
	kind, value, ok := strings.Cut(key, ":")
	canonicalKey, err := ActorKeyV1(kind, value)
	if !ok || err != nil || canonicalKey != key {
		return errors.New("invalid actor key")
	}
	return nil
}

// ActorKeyV1 preserves the document-events/v1 normalization contract. Person
// entry validation is stricter and must not change how persisted keys are read.
func ActorKeyV1(kind, value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errors.New("actor key value is not valid UTF-8")
	}
	var normalized string
	var err error
	switch kind {
	case "email":
		normalized, err = normalizeActorEmail(value)
	case "phone":
		normalized = normalizeActorPhone(value)
	case "handle":
		if canonical.IsSHA256Hex(value) {
			normalized = value
		} else {
			normalized, err = normalizeActorHandle(value)
		}
	case "external_uid":
		if !canonical.IsSHA256Hex(value) {
			return "", errors.New("external actor key is not a digest")
		}
		normalized = value
	case "name_alias":
		normalized = FoldPersonName(value)
	default:
		return "", errors.New("actor key kind is unknown")
	}
	if err != nil {
		return "", err
	}
	if normalized == "" {
		return "", errors.New("actor key value is empty")
	}
	key := kind + ":" + normalized
	if len(key) > MaxActorKeyBytes {
		return "", fmt.Errorf("actor key is longer than %d bytes", MaxActorKeyBytes)
	}
	return key, nil
}

func asciiLower(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, value)
}

func normalizeActorEmail(value string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(value), "<>")
	if trimmed == "" {
		return "", nil
	}
	at := strings.LastIndex(trimmed, "@")
	if at <= 0 || at == len(trimmed)-1 {
		return "", errors.New("actor key email has no domain")
	}
	local, domain := trimmed[:at], trimmed[at+1:]
	if strings.ContainsAny(domain, "[]") {
		return "", errors.New("actor key email domain is a literal")
	}
	ascii, err := idna.Lookup.ToASCII(strings.ToLower(domain))
	if err != nil {
		return "", fmt.Errorf("actor key email domain is not resolvable: %w", err)
	}
	return asciiLower(local) + "@" + ascii, nil
}

func normalizeActorPhone(value string) string {
	var digits strings.Builder
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	bare := digits.String()
	if bare == "" {
		return ""
	}
	if strings.HasPrefix(strings.TrimSpace(value), "+") && len(bare) >= 7 && len(bare) <= 15 {
		return "+" + bare
	}
	return bare
}

func normalizeActorHandle(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	slash := strings.Index(trimmed, "/")
	if slash <= 0 || slash == len(trimmed)-1 {
		return "", errors.New("actor key handle is not service/value")
	}
	return asciiLower(trimmed[:slash]) + "/" + trimmed[slash+1:], nil
}
