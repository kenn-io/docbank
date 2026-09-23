package report

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

const syntheticOccurrenceArchive = "11111111-1111-4111-8111-111111111111"

// N proves cross-root receipt ownership in storage. These pure tests cover
// only one root's key reduction and the resulting summary's root binding.

func syntheticReceiptKey(number int) NativeOccurrenceKey {
	return NativeOccurrenceKey{
		ArchiveID: syntheticOccurrenceArchive,
		ReceiptID: fmt.Sprintf("22222222-2222-4222-8222-%012x", number),
	}
}

func syntheticJobKey(number int, ordinal int64) NativeOccurrenceKey {
	key := syntheticReceiptKey(number)
	key.JobID = "33333333-3333-4333-8333-333333333333"
	key.Ordinal = ordinal
	return key
}

func reduceNativeOccurrences(t *testing.T, row CountRecord, state, reason string, keys ...NativeOccurrenceKey) *NativeOccurrenceSummary {
	t.Helper()
	summary, err := ReduceNativeOccurrenceKeys(row.Document, row.Email.GenerationID, NativeOccurrenceEvidence{
		State: state, Reason: reason, Keys: keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &summary
}

func TestTypedReportNativeOccurrencesDedupeAndWeightFrozenTruth(t *testing.T) {
	first := nativeCountRow(1, "v1", "g1", "1", RecordYes, RecordNo)
	first.SourceOccurrences = reduceNativeOccurrences(t, first, "available", "",
		syntheticReceiptKey(1), syntheticReceiptKey(1),
		syntheticJobKey(2, 1), syntheticJobKey(2, 2))
	if first.SourceOccurrences.UniqueCount != 3 {
		t.Fatalf("repeated direct receipt and two job ordinals yielded %d, want 3", first.SourceOccurrences.UniqueCount)
	}
	second := nativeCountRow(2, "v1", "g2", "1", RecordNo, RecordYes)
	second.SourceOccurrences = reduceNativeOccurrences(t, second, "available", "", syntheticReceiptKey(3))

	got, err := CalculateRecordCounts(2, []CountRecord{first, second})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].SourceOccurrences, 3)
	requireExactCount(t, got.Terms[1].SourceOccurrences, 1)
	requireExactCount(t, got.Union.SourceOccurrences, 4)
	requireExactCount(t, got.Exclusive[0].SourceOccurrences, 3)
	requireExactCount(t, got.Exclusive[1].SourceOccurrences, 1)
	if got.Union.Conversations.Value != nil || got.Union.Conversations.Reason == "" {
		t.Fatalf("native receipts fabricated conversation count: %+v", got.Union.Conversations)
	}
}

func TestTypedReportNativeOccurrenceCompletenessAndUnknown(t *testing.T) {
	empty := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	empty.SourceOccurrences = reduceNativeOccurrences(t, empty, "available", "")
	got, err := CalculateRecordCounts(1, []CountRecord{empty})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].SourceOccurrences, 0)

	known := nativeCountRow(2, "v1", "g2", "1", RecordYes)
	known.SourceOccurrences = reduceNativeOccurrences(t, known, "available", "", syntheticReceiptKey(1))
	legacy := nativeCountRow(3, "v1", "g3", "1", RecordYes)
	legacy.SourceOccurrences = reduceNativeOccurrences(t, legacy, "unavailable", "legacy_occurrence_unproven")
	got, err = CalculateRecordCounts(1, []CountRecord{known, legacy})
	if err != nil {
		t.Fatal(err)
	}
	count := got.Terms[0].SourceOccurrences
	if count.Value != nil || count.Observed != 1 || count.Unknown != 0 || count.Reason == "" {
		t.Fatalf("incomplete receipt coverage claimed exact count: %+v", count)
	}
	got, err = CalculateRecordCounts(1, []CountRecord{known, nativeCountRow(4, "v1", "g4", "1", RecordYes)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Terms[0].SourceOccurrences.Value != nil || got.Terms[0].SourceOccurrences.Reason == "" {
		t.Fatalf("omitted receipt evidence claimed zero: %+v", got.Terms[0].SourceOccurrences)
	}

	missingText := nativeCountRow(5, "v1", "g5", "1", RecordUnknown)
	missingText.SourceOccurrences = reduceNativeOccurrences(t, missingText, "available", "", syntheticReceiptKey(5), syntheticReceiptKey(6))
	got, err = CalculateRecordCounts(1, []CountRecord{missingText})
	if err != nil {
		t.Fatal(err)
	}
	requireUnknownCount(t, got.Terms[0].SourceOccurrences, 0, 2)
}

func TestTypedReportNativeOccurrenceExclusiveCompetingTermUnknown(t *testing.T) {
	root := nativeCountRow(1, "v1", "g1", "1", RecordYes, RecordUnknown)
	root.SourceOccurrences = reduceNativeOccurrences(t, root, "available", "", syntheticReceiptKey(1), syntheticReceiptKey(2))
	got, err := CalculateRecordCounts(2, []CountRecord{root})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].SourceOccurrences, 2)
	requireExactCount(t, got.Union.SourceOccurrences, 2)
	requireUnknownCount(t, got.Exclusive[0].SourceOccurrences, 0, 2)
}

func TestTypedReportNativeOccurrenceEvidenceBindsExactRoot(t *testing.T) {
	root := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	summary := reduceNativeOccurrences(t, root, "available", "", syntheticReceiptKey(1))
	tests := []struct {
		name    string
		records []CountRecord
	}{
		{"different version", []CountRecord{func() CountRecord {
			row := nativeCountRow(1, "v2", "g1", "1", RecordYes)
			row.SourceOccurrences = summary
			return row
		}()}},
		{"different generation", []CountRecord{func() CountRecord {
			row := nativeCountRow(1, "v1", "g2", "1", RecordYes)
			row.SourceOccurrences = summary
			return row
		}()}},
		{"nested MIME part", []CountRecord{root, func() CountRecord {
			row := nativeCountRow(1, "v1", "g1", "1.2", RecordYes)
			row.SourceOccurrences = summary
			return row
		}()}},
		{"second packet for same EML", []CountRecord{func() CountRecord {
			row := root
			row.SourceOccurrences = summary
			return row
		}(), func() CountRecord {
			row := root
			row.SourceOccurrences = summary
			return row
		}()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CalculateRecordCounts(1, test.records); err == nil {
				t.Fatal("accepted occurrence evidence outside one exact selected EML root")
			}
		})
	}
}

func TestTypedReportNativeOccurrenceKeyAndCoverageLimits(t *testing.T) {
	root := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	invalid := []struct {
		name     string
		evidence NativeOccurrenceEvidence
	}{
		{"available with reason", NativeOccurrenceEvidence{State: "available", Reason: "legacy_occurrence_unproven"}},
		{"unavailable without reason", NativeOccurrenceEvidence{State: "unavailable"}},
		{"unavailable with keys", NativeOccurrenceEvidence{State: "unavailable", Reason: "occurrence_limit", Keys: []NativeOccurrenceKey{syntheticReceiptKey(1)}}},
		{"direct with ordinal", NativeOccurrenceEvidence{State: "available", Keys: []NativeOccurrenceKey{func() NativeOccurrenceKey {
			key := syntheticReceiptKey(1)
			key.Ordinal = 1
			return key
		}()}}},
		{"job without ordinal", NativeOccurrenceEvidence{State: "available", Keys: []NativeOccurrenceKey{syntheticJobKey(1, 0)}}},
		{"missing receipt", NativeOccurrenceEvidence{State: "available", Keys: []NativeOccurrenceKey{{ArchiveID: syntheticOccurrenceArchive}}}},
		{"oversized archive", NativeOccurrenceEvidence{State: "available", Keys: []NativeOccurrenceKey{{ArchiveID: strings.Repeat("a", 1025), ReceiptID: syntheticReceiptKey(1).ReceiptID}}}},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ReduceNativeOccurrenceKeys(root.Document, root.Email.GenerationID, test.evidence); err == nil {
				t.Fatal("accepted malformed native occurrence evidence")
			}
		})
	}

	keys := make([]NativeOccurrenceKey, 1000)
	for i := range keys {
		keys[i] = syntheticReceiptKey(i + 1)
	}
	if _, err := ReduceNativeOccurrenceKeys(root.Document, root.Email.GenerationID, NativeOccurrenceEvidence{State: "available", Keys: keys}); err != nil {
		t.Fatalf("rejected N's complete 1000-key bound: %v", err)
	}
	tooMany := make([]NativeOccurrenceKey, len(keys)+1)
	copy(tooMany, keys)
	tooMany[len(keys)] = syntheticReceiptKey(1001)
	if _, err := ReduceNativeOccurrenceKeys(root.Document, root.Email.GenerationID, NativeOccurrenceEvidence{State: "available", Keys: tooMany}); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("1001 supplied keys error = %v, want ErrReportLimit", err)
	}
	tooMany[len(keys)] = syntheticReceiptKey(1)
	if _, err := ReduceNativeOccurrenceKeys(root.Document, root.Email.GenerationID, NativeOccurrenceEvidence{State: "available", Keys: tooMany}); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("1001 supplied duplicate keys error = %v, want ErrReportLimit", err)
	}
}
