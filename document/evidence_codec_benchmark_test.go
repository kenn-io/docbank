package document

import (
	"strings"
	"testing"
)

func BenchmarkCanonicalEvidenceStringASCII(b *testing.B) {
	value := strings.Repeat("a", 1<<20)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for b.Loop() {
		if canonicalEvidenceString(value) != value {
			b.Fatal("canonical ASCII text changed")
		}
	}
}

func BenchmarkValidateEvidenceTextASCII(b *testing.B) {
	value := strings.Repeat("a", 1<<20)
	b.SetBytes(int64(len(value)))
	b.ReportAllocs()
	for b.Loop() {
		if err := validateEvidenceText(value, "unit text"); err != nil {
			b.Fatal(err)
		}
	}
}
