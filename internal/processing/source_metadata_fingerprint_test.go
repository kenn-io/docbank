package processing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The extractor fingerprint decides which originals every vault re-reads.
// Changing it is a deliberate, vault-wide re-extraction; forgetting to change
// it keeps stale evidence. Pinning the value here makes both mistakes fail
// review: a parser change without a descriptor bump leaves this test green
// while the evidence goes stale, so reviewers of parser changes must expect
// this file to change too, and an accidental bump turns this test red.
func TestSourceMetadataExtractorFingerprintIsPinned(t *testing.T) {
	assert.Equal(t, "67186a2bd450858b65360cf78fff8a103ef808954fe155a40f81a7b5e7286c0a",
		SourceMetadataExtractorFingerprint)
}
