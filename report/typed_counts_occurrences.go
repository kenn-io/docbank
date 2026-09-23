package report

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// NativeOccurrenceKey identifies one mailbox source observation. A direct
// receipt has no job or ordinal; job observations have a job and ordinal.
type NativeOccurrenceKey struct {
	ArchiveID string `json:"archive_id"`
	ReceiptID string `json:"receipt_id"`
	JobID     string `json:"job_id,omitempty"`
	Ordinal   int64  `json:"ordinal,omitempty"`
}

// NativeOccurrenceEvidence is one complete or explicitly unavailable source
// snapshot for an exact selected EML root. Callers reduce one root at a time.
type NativeOccurrenceEvidence struct {
	State  string                `json:"state"`
	Reason string                `json:"reason,omitempty"`
	Keys   []NativeOccurrenceKey `json:"keys,omitempty"`
}

// NativeOccurrenceSummary keeps the count input bounded after the caller has
// frozen the exact receipt evidence separately for later replay.
type NativeOccurrenceSummary struct {
	Document     Identity `json:"document"`
	GenerationID string   `json:"generation_id"`
	State        string   `json:"state"`
	Reason       string   `json:"reason,omitempty"`
	UniqueCount  int64    `json:"unique_count"`
}

const (
	maxNativeOccurrenceKeysPerRoot = 1000
	maxNativeOccurrenceKeyText     = 128
)

func validOccurrenceText(value string) bool {
	return value != "" && len(value) <= maxNativeOccurrenceKeyText && utf8.ValidString(value)
}

func validOccurrenceState(state, reason string, count int64) bool {
	switch state {
	case "available":
		return reason == "" && count >= 0 && count <= maxNativeOccurrenceKeysPerRoot
	case "unavailable":
		return validOccurrenceText(reason) && count == 0
	default:
		return false
	}
}

// ReduceNativeOccurrenceKeys validates and deduplicates one root's bounded key
// set. Receipt uniqueness across different roots belongs to native storage.
func ReduceNativeOccurrenceKeys(document Identity, generationID string, evidence NativeOccurrenceEvidence) (NativeOccurrenceSummary, error) {
	if len(evidence.Keys) > maxNativeOccurrenceKeysPerRoot {
		return NativeOccurrenceSummary{}, fmt.Errorf("%w: native source occurrence limit", ErrReportLimit)
	}
	if document.NodeID <= 0 || document.VersionID == "" || !validSHA256(document.SHA256) || !validOccurrenceText(generationID) ||
		!validOccurrenceState(evidence.State, evidence.Reason, 0) || evidence.State == "unavailable" && len(evidence.Keys) != 0 {
		return NativeOccurrenceSummary{}, errors.New("invalid native source occurrence evidence")
	}
	summary := NativeOccurrenceSummary{Document: document, GenerationID: generationID, State: evidence.State, Reason: evidence.Reason}
	if evidence.State == "unavailable" {
		return summary, nil
	}
	for _, key := range evidence.Keys {
		if !validOccurrenceText(key.ArchiveID) || !validOccurrenceText(key.ReceiptID) ||
			(key.JobID == "" && key.Ordinal != 0) ||
			(key.JobID != "" && (!validOccurrenceText(key.JobID) || key.Ordinal < 1)) {
			return NativeOccurrenceSummary{}, errors.New("invalid native source occurrence key")
		}
	}
	seen := make(map[NativeOccurrenceKey]struct{}, len(evidence.Keys))
	for _, key := range evidence.Keys {
		seen[key] = struct{}{}
	}
	summary.UniqueCount = int64(len(seen))
	return summary, nil
}
