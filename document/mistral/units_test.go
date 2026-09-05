package mistral

import (
	"bytes"
	"testing"
)

func TestLocalUnitCounterRegistryIsMistralOwnedAndBounded(t *testing.T) {
	for formatID, counter := range localUnitCounters {
		if counter == nil {
			t.Fatalf("localUnitCounters[%q] is nil", formatID)
		}
		if _, ok := CandidateFormatByID(formatID); !ok {
			t.Fatalf("localUnitCounters[%q] has no candidate", formatID)
		}
	}
	for formatID, method := range expectedUnitBounds {
		if method == UnitBoundLocalExact && localUnitCounters[formatID] == nil {
			t.Fatalf("local exact format %q has no local counter", formatID)
		}
	}
}

func TestCountLocalUnitsLeavesUnprovedFormatsUnbounded(t *testing.T) {
	docx, ok := CandidateFormatByID("docx")
	if !ok {
		t.Fatal("docx candidate is missing")
	}
	units, err := countLocalUnits(docx, bytes.NewReader([]byte("synthetic")), 9)
	if err != nil {
		t.Fatal(err)
	}
	if units != 0 {
		t.Fatalf("countLocalUnits(docx) = %d, want 0 for unproved format", units)
	}
}
