package uploadproof

import (
	jsonv1 "encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProofRequiresValidFactsAndKeepsDetachedSnapshot(t *testing.T) {
	proof, err := Issue(validFacts())
	require.NoError(t, err)
	require.True(t, proof.Valid())

	before := proof.Snapshot()
	changed := proof.Snapshot()
	changed.SourceBytes++
	assert.Equal(t, before, proof.Snapshot())
	assert.NotContains(t, fmt.Sprintf("%+v", proof), "synthetic-proof.txt")
}

func TestProofRejectsSerializationAndDecodingClearsAuthority(t *testing.T) {
	proof, err := Issue(validFacts())
	require.NoError(t, err)

	_, err = jsonv1.Marshal(proof)
	require.ErrorContains(t, err, "not serializable")
	_, err = jsonv2.Marshal(proof)
	require.ErrorContains(t, err, "not serializable")

	require.ErrorContains(t, jsonv1.Unmarshal([]byte(`{}`), &proof), "cannot be decoded")
	assert.False(t, proof.Valid())
	proof, err = Issue(validFacts())
	require.NoError(t, err)
	require.ErrorContains(t, jsonv2.Unmarshal([]byte(`{}`), &proof), "cannot be decoded")
	assert.False(t, proof.Valid())
}

func TestProofRejectsInvalidFacts(t *testing.T) {
	require.False(t, (Proof{}).Valid())
	tests := []struct {
		name   string
		mutate func(*Facts)
	}{
		{"zero source bytes", func(f *Facts) { f.SourceBytes = 0 }},
		{"zero max source bytes", func(f *Facts) { f.MaxSourceBytes = 0 }},
		{"source exceeds max", func(f *Facts) { f.MaxSourceBytes = f.SourceBytes - 1 }},
		{"uppercase digest", func(f *Facts) { f.SourceSHA256 = strings.Repeat("A", 64) }},
		{"short checksum", func(f *Facts) { f.CapabilityRecordChecksum = strings.Repeat("a", 63) }},
		{"bad descriptor fingerprint", func(f *Facts) { f.DescriptorFingerprint = strings.Repeat("g", 64) }},
		{"empty media family", func(f *Facts) { f.MediaFamily = "" }},
		{"invalid media family token", func(f *Facts) { f.MediaFamily = "Text Family" }},
		{"noncanonical media type", func(f *Facts) { f.MediaType = "text/plain; charset=utf-8" }},
		{"oversized media type", func(f *Facts) { f.MediaType = strings.Repeat("a", 256) }},
		{"invalid format token", func(f *Facts) { f.Format = "txt/path" }},
		{"invalid input kind", func(f *Facts) { f.InputKind = "query_text" }},
		{"negative measurement", func(f *Facts) { f.Pages = -1 }},
		{"negative limit", func(f *Facts) { f.MaxPages = -1 }},
		{"pages exceed limit", func(f *Facts) { f.Pages, f.MaxPages = 2, 1 }},
		{"pixels exceed limit", func(f *Facts) { f.Pixels, f.MaxPixels = 2, 1 }},
		{"frames exceed limit", func(f *Facts) { f.Frames, f.MaxFrames = 2, 1 }},
		{"duration exceeds limit", func(f *Facts) { f.DurationMS, f.MaxDurationMS = 2, 1 }},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			facts := validFacts()
			testCase.mutate(&facts)
			proof, err := Issue(facts)
			require.Error(t, err)
			assert.False(t, proof.Valid())
		})
	}
}

func validFacts() Facts {
	return Facts{
		SourceBytes: 21, SourceSHA256: strings.Repeat("1", 64),
		CapabilityRecordChecksum: strings.Repeat("2", 64),
		DescriptorFingerprint:    strings.Repeat("3", 64), ProfileFingerprint: strings.Repeat("4", 64),
		DisclosureFingerprint: strings.Repeat("5", 64), InputKind: "original_file",
		MediaFamily: "text", MediaType: "text/plain", Format: "txt",
		Pages: 1, Pixels: 2, Frames: 3, DurationMS: 4,
		MaxSourceBytes: 64, MaxPages: 10, MaxPixels: 10, MaxFrames: 10, MaxDurationMS: 10,
	}
}
