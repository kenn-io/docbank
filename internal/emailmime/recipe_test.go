package emailmime

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/docbank/document"
)

func TestRecipePinsImplementationAndProductionLimits(t *testing.T) {
	want := document.EmailLimitsV1{SourceBytes: 128 << 20, PartBytes: 128 << 20, DecodedBytes: 256 << 20, Parts: 1000, Depth: 16, HeaderBytes: 1 << 20, AggregateHeaderBytes: 8 << 20, HeaderFields: 4096, AggregateHeaderFields: 16384, BodyUTF8Bytes: 16 << 20, AggregateBodyUTF8Bytes: 256 << 20, HTMLDisplayBytes: 16 << 20, InventoryBytes: 8 << 20, Diagnostics: 4096, DiagnosticDetailBytes: 1024, HeaderDisplayBytes: 1 << 20}
	recipe := Recipe()
	assert.Equal(t, want, recipe.Limits)
	assert.Equal(t, "1a5aa86641f98021fd6f37466c07e62f2662f4f55d88b9d09ebf032988e5d110", recipe.MultipartSourceSHA256)
	recipe.Limits.SourceBytes = 1
	assert.Equal(t, int64(128<<20), Recipe().Limits.SourceBytes)
}
