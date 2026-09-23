package report

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"
)

func nativeCountRow(node int64, version, generation, path string, truth ...RecordTruth) CountRecord {
	return CountRecord{
		Document: Identity{NodeID: node, VersionID: version, SHA256: strings.Repeat("a", 64)},
		Email:    &NativeEmailMessage{GenerationID: generation, Path: path},
		Truth:    truth,
	}
}

func requireExactCount(t *testing.T, got EvidenceCount, value int64) {
	t.Helper()
	if got.Value == nil || *got.Value != value || got.Observed != value || got.Unknown != 0 || got.Reason != "" {
		t.Fatalf("count=%+v, want exact %d", got, value)
	}
}

func requireUnknownCount(t *testing.T, got EvidenceCount, observed, unknown int64) {
	t.Helper()
	if got.Value != nil || got.Observed != observed || got.Unknown != unknown || got.Reason != "incomplete_term_coverage" {
		t.Fatalf("count=%+v, want observed=%d unknown=%d", got, observed, unknown)
	}
}

func TestTypedReportOverlapCompleteSets(t *testing.T) {
	records := []CountRecord{
		nativeCountRow(1, "v1", "g1", "1", RecordYes, RecordNo),
		nativeCountRow(2, "v1", "g2", "1", RecordYes, RecordYes),
		nativeCountRow(3, "v1", "g3", "1", RecordNo, RecordYes),
	}
	got, err := CalculateRecordCounts(2, records)
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].Documents, 2)
	requireExactCount(t, got.Terms[1].Documents, 2)
	requireExactCount(t, got.Union.Documents, 3)
	requireExactCount(t, got.Exclusive[0].Documents, 1)
	requireExactCount(t, got.Exclusive[1].Documents, 1)
	requireExactCount(t, got.Union.EmailMessages, 3)
}

func TestTypedReportNativeEmailIdentityAndRevision(t *testing.T) {
	a := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	revision := nativeCountRow(1, "v2", "g2", "1", RecordYes)
	got, err := CalculateRecordCounts(1, []CountRecord{a, a, revision})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].Documents, 2)
	requireExactCount(t, got.Terms[0].EmailMessages, 2)
}

func TestTypedReportNestedMIMEMessageCountsOnlySelectedEMLRoots(t *testing.T) {
	root := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	nested := nativeCountRow(1, "v1", "g1", "1.2", RecordYes)
	child := nativeCountRow(2, "v1", "g2", "1", RecordYes)
	got, err := CalculateRecordCounts(1, []CountRecord{root, nested, child})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].Documents, 2)
	requireExactCount(t, got.Terms[0].EmailMessages, 2)
	if _, err := CalculateRecordCounts(1, []CountRecord{nested}); err == nil {
		t.Fatal("nested MIME message without selected EML root was accepted")
	}
}

func TestTypedReportRejectsConflictingOrNonRootEmailIdentity(t *testing.T) {
	root := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	tests := []struct {
		name    string
		records []CountRecord
	}{
		{"conflicting generation", []CountRecord{root, nativeCountRow(1, "v1", "g2", "1.2", RecordYes)}},
		{"non-root top-level path", []CountRecord{root, nativeCountRow(1, "v1", "g1", "2", RecordYes)}},
		{"nested path without root", []CountRecord{nativeCountRow(1, "v1", "g1", "1.2", RecordYes)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CalculateRecordCounts(1, test.records); err == nil {
				t.Fatal("accepted email records outside one exact selected root generation")
			}
		})
	}
}

func TestReportUnknownNotAndCompetingTerm(t *testing.T) {
	if NegateRecordTruth(RecordUnknown) != RecordUnknown {
		t.Fatal("NOT matched missing text")
	}
	known := nativeCountRow(1, "v1", "g1", "1", RecordYes, RecordUnknown)
	missing := nativeCountRow(2, "v1", "g2", "1", RecordUnknown, RecordNo)
	got, err := CalculateRecordCounts(2, []CountRecord{known, missing})
	if err != nil {
		t.Fatal(err)
	}
	requireUnknownCount(t, got.Terms[0].Documents, 1, 1)
	requireUnknownCount(t, got.Exclusive[0].Documents, 0, 2)
	requireUnknownCount(t, got.Union.Documents, 1, 1)
}

func TestReportUnknownBooleanAlgebra(t *testing.T) {
	tests := []struct {
		op   string
		a, b RecordTruth
		want RecordTruth
	}{
		{"and", RecordNo, RecordUnknown, RecordNo},
		{"and", RecordYes, RecordUnknown, RecordUnknown},
		{"and", RecordUnknown, RecordUnknown, RecordUnknown},
		{"or", RecordYes, RecordUnknown, RecordYes},
		{"or", RecordNo, RecordUnknown, RecordUnknown},
		{"or", RecordUnknown, RecordUnknown, RecordUnknown},
		{"not", RecordUnknown, RecordNo, RecordUnknown},
	}
	for _, test := range tests {
		got, err := CombineRecordTruth(test.op, test.a, test.b)
		if err != nil || got != test.want {
			t.Errorf("%s(%s,%s)=%s, err=%v; want %s", test.op, test.a, test.b, got, err, test.want)
		}
	}
}

func TestTypedReportFamilyExpansionStaysInsideSelectedPopulation(t *testing.T) {
	a := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	a.FamilyKey = "frozen-family"
	b := nativeCountRow(2, "v1", "g2", "1", RecordNo)
	b.FamilyKey = "frozen-family"
	c := nativeCountRow(3, "v1", "g3", "1", RecordNo)
	c.FamilyKey = "other-family"
	got, err := CalculateRecordCounts(1, []CountRecord{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].Documents, 1)
	requireExactCount(t, got.Terms[0].FamilyExpandedDocuments, 2)
	requireExactCount(t, got.Union.FamilyExpandedDocuments, 2)
}

func TestTypedReportRejectsConflictingDocumentDigest(t *testing.T) {
	a := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	b := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	b.Document.SHA256 = strings.Repeat("b", 64)
	if _, err := CalculateRecordCounts(1, []CountRecord{a, b}); err == nil {
		t.Fatal("same node/version with conflicting bytes was counted twice")
	}
}

func TestTypedReportRejectsMalformedFrozenCountRecords(t *testing.T) {
	badNode := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	badNode.Document.NodeID = 0
	badVersion := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	badVersion.Document.VersionID = ""
	badDigest := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	badDigest.Document.SHA256 = "short"
	badArity := nativeCountRow(1, "v1", "g1", "1", RecordYes, RecordNo)
	badTruth := nativeCountRow(1, "v1", "g1", "1", RecordTruth("maybe"))
	familyA := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	familyA.FamilyKey = "family-a"
	familyB := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	familyB.FamilyKey = "family-b"

	tests := []struct {
		name    string
		records []CountRecord
		wantErr string
	}{
		{"non-positive node", []CountRecord{badNode}, "invalid count record"},
		{"missing version", []CountRecord{badVersion}, "invalid count record"},
		{"truncated digest", []CountRecord{badDigest}, "invalid count record"},
		{"truth vector length", []CountRecord{badArity}, "invalid count record"},
		{"unknown truth value", []CountRecord{badTruth}, "invalid term truth"},
		{"conflicting family", []CountRecord{familyA, familyB}, "conflicting frozen document family"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := CalculateRecordCounts(1, test.records); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("CalculateRecordCounts error = %v; want %q", err, test.wantErr)
			}
		})
	}
}

func TestTypedReportNestedUnknownDoesNotEraseProvedRootHit(t *testing.T) {
	matched := nativeCountRow(1, "v1", "g1", "1", RecordYes)
	missing := nativeCountRow(1, "v1", "g1", "1.2", RecordUnknown)
	got, err := CalculateRecordCounts(1, []CountRecord{matched, missing})
	if err != nil {
		t.Fatal(err)
	}
	requireExactCount(t, got.Terms[0].Documents, 1)
	requireExactCount(t, got.Terms[0].EmailMessages, 1)
}

func TestTypedReportUnavailableDimensionsAreExplicit(t *testing.T) {
	got, err := CalculateRecordCounts(1, []CountRecord{nativeCountRow(1, "v1", "g1", "1", RecordYes)})
	if err != nil {
		t.Fatal(err)
	}
	for _, counts := range []RecordCounts{got.Terms[0], got.Union, got.Exclusive[0]} {
		if counts.Conversations.Value != nil || counts.Conversations.Reason != "conversation_authority_unavailable" {
			t.Fatalf("conversation count claimed as zero: %+v", counts.Conversations)
		}
		if counts.SourceOccurrences.Value != nil || counts.SourceOccurrences.Reason != "source_occurrence_authority_unavailable" {
			t.Fatalf("occurrence count claimed as zero: %+v", counts.SourceOccurrences)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"conversations":{"value":null`, `"source_occurrences":{"value":null`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("missing explicit unavailable field %s: %s", field, encoded)
		}
	}
}

func TestTypedReportRejectsOversizedPopulationAndTermCount(t *testing.T) {
	if _, err := CalculateRecordCounts(maxTerms+1, nil); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("term limit error = %v, want ErrReportLimit", err)
	}
	records := make([]CountRecord, 50001)
	for i := range records {
		records[i] = CountRecord{Document: Identity{NodeID: int64(i + 1), VersionID: "v1", SHA256: strings.Repeat("a", 64)}, Truth: []RecordTruth{RecordNo}}
	}
	if _, err := CalculateRecordCounts(maxTerms, records); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("record by term limit error = %v, want ErrReportLimit", err)
	}
	if _, err := CalculateRecordCounts(1, records); !errors.Is(err, ErrReportLimit) {
		t.Fatalf("document limit error = %v, want ErrReportLimit", err)
	}
}
