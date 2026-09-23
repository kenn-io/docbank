package report

import (
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/docbank/document"
)

// RecordTruth is a frozen lexical result. A missing or partial body is unknown.
type RecordTruth string

const (
	RecordYes     RecordTruth = "yes"
	RecordNo      RecordTruth = "no"
	RecordUnknown RecordTruth = "unknown"
)

func NegateRecordTruth(value RecordTruth) RecordTruth {
	switch value {
	case RecordYes:
		return RecordNo
	case RecordNo:
		return RecordYes
	default:
		return RecordUnknown
	}
}

// CombineRecordTruth applies three-valued Boolean logic to frozen lexical
// results. A proved true OR or proved false AND remains known despite a gap.
func CombineRecordTruth(op string, a, b RecordTruth) (RecordTruth, error) {
	valid := func(v RecordTruth) bool { return v == RecordYes || v == RecordNo || v == RecordUnknown }
	if !valid(a) || op != "not" && !valid(b) {
		return "", errors.New("invalid record truth")
	}
	switch op {
	case "not":
		return NegateRecordTruth(a), nil
	case "and":
		if a == RecordNo || b == RecordNo {
			return RecordNo, nil
		}
		if a == RecordYes && b == RecordYes {
			return RecordYes, nil
		}
		return RecordUnknown, nil
	case "or":
		return orRecordTruth(a, b), nil
	default:
		return "", errors.New("invalid Boolean operation")
	}
}

// NativeEmailMessage identifies a MIME path within one exact decoded email
// generation. Only path 1 is the selected EML's top-level message. Nested
// paths can contribute lexical truth to that EML but never add message units.
type NativeEmailMessage struct {
	GenerationID string `json:"generation_id"`
	Path         string `json:"path"`
}

// CountRecord is frozen evidence supplied by the existing report observer.
// One document can contain several native MIME messages. The optional family
// key has already been resolved inside the selected population.
type CountRecord struct {
	Document          Identity                 `json:"document"`
	Email             *NativeEmailMessage      `json:"email,omitempty"`
	SourceOccurrences *NativeOccurrenceSummary `json:"source_occurrences,omitempty"`
	FamilyKey         string                   `json:"family_key,omitempty"`
	Truth             []RecordTruth            `json:"truth"`
}

// EvidenceCount is exact only when no unit could change its value. Observed is
// the proved positive subset; Unknown counts units with unresolved truth.
type EvidenceCount struct {
	Value    *int64 `json:"value"`
	Observed int64  `json:"observed"`
	Unknown  int64  `json:"unknown"`
	Reason   string `json:"reason,omitempty"`
}

type RecordCounts struct {
	Documents               EvidenceCount `json:"documents"`
	EmailMessages           EvidenceCount `json:"email_messages"`
	Conversations           EvidenceCount `json:"conversations"`
	SourceOccurrences       EvidenceCount `json:"source_occurrences"`
	FamilyExpandedDocuments EvidenceCount `json:"family_expanded_documents"`
}

// RecordCountResult is separate from the v1 Result and its CSV/ZIP codecs.
type RecordCountResult struct {
	Version   int            `json:"version"`
	Terms     []RecordCounts `json:"terms"`
	Union     RecordCounts   `json:"union"`
	Exclusive []RecordCounts `json:"exclusive"`
}

type countUnit struct {
	truth       []RecordTruth
	family      string
	occurrences *NativeOccurrenceSummary
}

type documentVersionKey struct {
	nodeID    int64
	versionID string
}

func copyTruth(values []RecordTruth) []RecordTruth {
	return append([]RecordTruth(nil), values...)
}

func orRecordTruth(a, b RecordTruth) RecordTruth {
	if a == RecordYes || b == RecordYes {
		return RecordYes
	}
	if a == RecordUnknown || b == RecordUnknown {
		return RecordUnknown
	}
	return RecordNo
}

func mergeRecordTruth(dst, src []RecordTruth) {
	for i, value := range src {
		dst[i] = orRecordTruth(dst[i], value)
	}
}

func unitCountTruth(unit *countUnit, term int, mode string) RecordTruth {
	state := RecordNo
	switch mode {
	case "term":
		state = unit.truth[term]
	case "union":
		for _, value := range unit.truth {
			state = orRecordTruth(state, value)
		}
	case "exclusive":
		state = unit.truth[term]
		if state != RecordNo {
			for i, other := range unit.truth {
				if i == term {
					continue
				}
				if other == RecordYes {
					state = RecordNo
					break
				}
				if other == RecordUnknown {
					state = RecordUnknown
				}
			}
		}
	}
	return state
}

func evidenceCount(units []*countUnit, term int, mode string) EvidenceCount {
	var result EvidenceCount
	for _, unit := range units {
		state := unitCountTruth(unit, term, mode)
		switch state {
		case RecordYes:
			result.Observed++
		case RecordUnknown:
			result.Unknown++
		case RecordNo:
			continue
		}
	}
	if result.Unknown == 0 {
		value := result.Observed
		result.Value = &value
	} else {
		result.Reason = "incomplete_term_coverage"
	}
	return result
}

func sourceOccurrenceCount(emailMessages []*countUnit, term int, mode string) EvidenceCount {
	var result EvidenceCount
	unavailable := false
	for _, unit := range emailMessages {
		if unit.occurrences == nil || unit.occurrences.State != "available" {
			unavailable = true
			continue
		}
		switch unitCountTruth(unit, term, mode) {
		case RecordYes:
			result.Observed += unit.occurrences.UniqueCount
		case RecordUnknown:
			result.Unknown += unit.occurrences.UniqueCount
		case RecordNo:
			continue
		}
	}
	if unavailable {
		result.Reason = "source_occurrence_authority_unavailable"
	} else if result.Unknown != 0 {
		result.Reason = "incomplete_term_coverage"
	} else {
		value := result.Observed
		result.Value = &value
	}
	return result
}

func recordCounts(documents, emailMessages, expanded []*countUnit, term int, mode string) RecordCounts {
	return RecordCounts{
		Documents:     evidenceCount(documents, term, mode),
		EmailMessages: evidenceCount(emailMessages, term, mode),
		// Native MIME evidence has no verified conversation identity. Source
		// receipts count only after the observer freezes their separate proof.
		Conversations:           EvidenceCount{Reason: "conversation_authority_unavailable"},
		SourceOccurrences:       sourceOccurrenceCount(emailMessages, term, mode),
		FamilyExpandedDocuments: evidenceCount(expanded, term, mode),
	}
}

// CalculateRecordCounts uses complete frozen truth vectors. Capped display
// hits, current heads, and mutable family relationships cannot affect it.
func CalculateRecordCounts(termCount int, records []CountRecord) (RecordCountResult, error) {
	if termCount < 1 || termCount > maxTerms || len(records) > 1_000_000 ||
		len(records) > 50000*maxTerms/termCount {
		return RecordCountResult{}, fmt.Errorf("%w: report count limit", ErrReportLimit)
	}
	documentCapacity := min(len(records), 50000)
	documentByKey := make(map[Identity]*countUnit, documentCapacity)
	versionDigests := make(map[documentVersionKey]string, documentCapacity)
	emailByDocument := make(map[Identity]*countUnit)
	emailGeneration := make(map[Identity]string)
	emailRootSeen := make(map[Identity]bool)
	for index, record := range records {
		if record.Document.NodeID <= 0 || record.Document.VersionID == "" || !validSHA256(record.Document.SHA256) || len(record.Truth) != termCount {
			return RecordCountResult{}, fmt.Errorf("invalid count record %d", index)
		}
		for _, value := range record.Truth {
			if value != RecordYes && value != RecordNo && value != RecordUnknown {
				return RecordCountResult{}, fmt.Errorf("invalid term truth in record %d", index)
			}
		}
		versionKey := documentVersionKey{nodeID: record.Document.NodeID, versionID: record.Document.VersionID}
		if digest, seen := versionDigests[versionKey]; seen && digest != record.Document.SHA256 {
			return RecordCountResult{}, errors.New("conflicting frozen document digest")
		}
		versionDigests[versionKey] = record.Document.SHA256
		if existing := documentByKey[record.Document]; existing != nil {
			if existing.family != record.FamilyKey {
				return RecordCountResult{}, errors.New("conflicting frozen document family")
			}
			mergeRecordTruth(existing.truth, record.Truth)
		} else {
			if len(documentByKey) == 50000 {
				return RecordCountResult{}, fmt.Errorf("%w: report document limit", ErrReportLimit)
			}
			documentByKey[record.Document] = &countUnit{truth: copyTruth(record.Truth), family: record.FamilyKey}
		}
		if record.Email != nil {
			if record.Email.GenerationID == "" || document.ValidateEmailPartPath(record.Email.Path) != nil ||
				record.Email.Path != "1" && !strings.HasPrefix(record.Email.Path, "1.") {
				return RecordCountResult{}, errors.New("incomplete native email message identity")
			}
			if prior := emailGeneration[record.Document]; prior != "" && prior != record.Email.GenerationID {
				return RecordCountResult{}, errors.New("conflicting frozen email generation")
			}
			emailGeneration[record.Document] = record.Email.GenerationID
			if record.Email.Path == "1" {
				emailRootSeen[record.Document] = true
			}
			if existing := emailByDocument[record.Document]; existing != nil {
				mergeRecordTruth(existing.truth, record.Truth)
			} else {
				emailByDocument[record.Document] = &countUnit{truth: copyTruth(record.Truth)}
			}
		}
		if record.SourceOccurrences != nil {
			summary := record.SourceOccurrences
			if record.Email == nil || record.Email.Path != "1" || summary.Document != record.Document ||
				summary.GenerationID != record.Email.GenerationID ||
				!validOccurrenceState(summary.State, summary.Reason, summary.UniqueCount) {
				return RecordCountResult{}, errors.New("native source occurrences do not match selected EML root")
			}
			unit := emailByDocument[record.Document]
			if unit.occurrences != nil {
				return RecordCountResult{}, errors.New("duplicate native source occurrence packet")
			}
			unit.occurrences = summary
		}
	}
	for identity := range emailByDocument {
		if !emailRootSeen[identity] {
			return RecordCountResult{}, errors.New("selected email lacks top-level root")
		}
	}
	documents := make([]*countUnit, 0, len(documentByKey))
	expanded := make([]*countUnit, 0, len(documentByKey))
	families := make(map[string][]*countUnit)
	for _, unit := range documentByKey {
		documents = append(documents, unit)
		if unit.family == "" {
			expanded = append(expanded, unit)
		} else {
			families[unit.family] = append(families[unit.family], unit)
		}
	}
	for _, family := range families {
		truth := make([]RecordTruth, termCount)
		for i := range truth {
			truth[i] = RecordNo
		}
		for _, unit := range family {
			mergeRecordTruth(truth, unit.truth)
		}
		for range family {
			expanded = append(expanded, &countUnit{truth: truth})
		}
	}
	emailMessages := make([]*countUnit, 0, len(emailByDocument))
	for _, unit := range emailByDocument {
		emailMessages = append(emailMessages, unit)
	}
	result := RecordCountResult{Version: 2, Terms: make([]RecordCounts, termCount), Exclusive: make([]RecordCounts, termCount)}
	for term := range termCount {
		result.Terms[term] = recordCounts(documents, emailMessages, expanded, term, "term")
		result.Exclusive[term] = recordCounts(documents, emailMessages, expanded, term, "exclusive")
	}
	result.Union = recordCounts(documents, emailMessages, expanded, 0, "union")
	return result, nil
}
