package qmdexport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManifestEncodingIsExactBoundedAndRoundTrips(t *testing.T) {
	// Mutation caught: changing field order, checksum input, newline policy, or
	// checking size after buffer growth changes these canonical bytes/bounds.
	manifest := Manifest{Format: ManifestFormatV1, Collection: "synthetic", Entries: []Entry{}}
	checksum, err := manifestChecksum(manifest, 4096)
	require.NoError(t, err)
	require.Equal(t, "b0c7f1d41e964377da7ca1c4cc22f2baddc95f2508bce9e2f4e283cddc1c6034", checksum)
	manifest.Checksum = checksum
	want := []byte(`{"format":"docbank-qmd-export/v1","collection":"synthetic","entries":[],"checksum":"b0c7f1d41e964377da7ca1c4cc22f2baddc95f2508bce9e2f4e283cddc1c6034"}` + "\n")
	encoded, err := encodeManifest(manifest, int64(len(want)))
	require.NoError(t, err)
	require.Equal(t, want, encoded)
	_, err = encodeManifest(manifest, int64(len(want)-1))
	require.ErrorIs(t, err, errManifestBound)

	bounds := normalizedTestOptions(t)
	decoded, err := decodeManifest(encoded, checksum, bounds)
	require.NoError(t, err)
	require.Equal(t, manifest, decoded)
}

func TestManifestNilAndEmptyEntriesShareCanonicalBytes(t *testing.T) {
	// Mutation caught: encoding nil entries as null creates two identities for
	// the donor's valid empty manifest representation.
	nilEntries := Manifest{Format: ManifestFormatV1, Collection: "synthetic"}
	emptyEntries := Manifest{Format: ManifestFormatV1, Collection: "synthetic", Entries: []Entry{}}
	nilChecksum, err := manifestChecksum(nilEntries, 4096)
	require.NoError(t, err)
	emptyChecksum, err := manifestChecksum(emptyEntries, 4096)
	require.NoError(t, err)
	require.Equal(t, nilChecksum, emptyChecksum)
	nilEntries.Checksum = nilChecksum
	emptyEntries.Checksum = emptyChecksum
	nilEncoded, err := encodeManifest(nilEntries, 4096)
	require.NoError(t, err)
	emptyEncoded, err := encodeManifest(emptyEntries, 4096)
	require.NoError(t, err)
	require.Equal(t, nilEncoded, emptyEncoded)
}

func TestManifestEncodingCountsEscapedFrontmatterExpansion(t *testing.T) {
	// Mutation caught: budgeting input strings instead of encoded JSON misses
	// quote/backslash expansion and may grow beyond the configured cap.
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("plain body\n")})
	manifest := generation.Manifest
	manifest.Entries[0].Frontmatter = strings.Repeat("\\\"", 64)
	checksum, err := manifestChecksum(manifest, maxManifestBytes)
	require.NoError(t, err)
	manifest.Checksum = checksum
	encoded, err := encodeManifest(manifest, maxManifestBytes)
	require.NoError(t, err)
	_, err = encodeManifest(manifest, int64(len(encoded)-1))
	require.ErrorIs(t, err, errManifestBound)
}

func TestValidateManifestIncludesPersistedChecksumAndFinalLFInBudget(t *testing.T) {
	// Mutation caught: validating only the checksum form omits the populated
	// checksum plus final LF and accepts a manifest that cannot be persisted.
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("plain body\n")})
	encoded, err := encodeManifest(generation.Manifest, maxManifestBytes)
	require.NoError(t, err)
	bounds := normalizedTestOptions(t)
	bounds.MaxManifestBytes = int64(len(encoded) - 1)
	err = validateManifest(generation.Manifest, generation.ID, bounds)
	require.ErrorIs(t, err, errManifestBound)
}

func TestDecodeManifestRejectsStructuralAndCanonicalViolations(t *testing.T) {
	// Mutation caught: relaxing strict JSON or byte canonicality would admit
	// alternate manifests for the same generation identity.
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("plain body\n")})
	bounds := normalizedTestOptions(t)
	canonical, err := encodeManifest(generation.Manifest, bounds.MaxManifestBytes)
	require.NoError(t, err)
	tests := map[string][]byte{
		"unknown":         bytes.Replace(canonical, []byte(`{"format":`), []byte(`{"unknown":true,"format":`), 1),
		"duplicate":       bytes.Replace(canonical, []byte(`{"format":`), []byte(`{"format":"docbank-qmd-export/v1","format":`), 1),
		"trailing":        append(append([]byte(nil), canonical...), []byte("{}\n")...),
		"spacing":         bytes.Replace(canonical, []byte(`{"format":`), []byte("{ \"format\":"), 1),
		"no LF":           canonical[:len(canonical)-1],
		"invalid UTF-8":   append(append([]byte(nil), canonical[:len(canonical)-1]...), 0xff, '\n'),
		"unknown entry":   bytes.Replace(canonical, []byte(`{"uri":`), []byte(`{"unknown":true,"uri":`), 1),
		"duplicate entry": bytes.Replace(canonical, []byte(`{"uri":`), []byte(`{"uri":"wrong","uri":`), 1),
	}
	for name, encoded := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := decodeManifest(encoded, generation.ID, bounds)
			require.Error(t, err)
		})
	}
}

func TestDecodeManifestAcceptsExactEntryCapAndRejectsNext(t *testing.T) {
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("one\n"), []byte("two\n")})
	encoded, err := encodeManifest(generation.Manifest, maxManifestBytes)
	require.NoError(t, err)
	bounds := normalizedTestOptions(t)
	bounds.MaxDocuments = 2
	_, err = decodeManifest(encoded, generation.ID, bounds)
	require.NoError(t, err)
	bounds.MaxDocuments = 1
	_, err = decodeManifest(encoded, generation.ID, bounds)
	require.Error(t, err)
}

func TestDecodeManifestEnforcesEntryCapWhileDecoding(t *testing.T) {
	// Mutation caught: decoding an unbounded []Entry before validating its count
	// would materialize entry N+1 despite a smaller configured maximum.
	encoded := []byte(`{"format":"docbank-qmd-export/v1","collection":"synthetic","entries":[{},{},{}],"checksum":"` + strings.Repeat("a", 64) + `"}` + "\n")
	bounds := normalizedTestOptions(t)
	bounds.MaxDocuments = 2
	_, err := decodeManifest(encoded, strings.Repeat("a", 64), bounds)
	require.Error(t, err)
}

func TestValidateManifestRejectsIdentityOrderingAndBoundsDrift(t *testing.T) {
	// Mutation caught: omitting semantic validation admits wrong source/body
	// identities, noncanonical ordering, or source/header budget excess.
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("one\n"), []byte("two\n")})
	bounds := normalizedTestOptions(t)

	tests := map[string]func(*Manifest, *Options){
		"body digest format": func(manifest *Manifest, _ *Options) { manifest.Entries[0].ExportedMarkdownSHA256 = "bad" },
		"relative path":      func(manifest *Manifest, _ *Options) { manifest.Entries[0].RelativePath = "documents/wrong.md" },
		"order": func(manifest *Manifest, _ *Options) {
			manifest.Entries[0], manifest.Entries[1] = manifest.Entries[1], manifest.Entries[0]
		},
		"document size": func(_ *Manifest, options *Options) { options.MaxDocumentBytes = 3 },
		"total size":    func(_ *Manifest, options *Options) { options.MaxTotalBytes = 7 },
		"header size": func(manifest *Manifest, _ *Options) {
			manifest.Entries[0].Frontmatter = strings.Repeat("x", maxClaimedFrontmatterBytes+1)
		},
		"duplicate URI": func(manifest *Manifest, _ *Options) { manifest.Entries[1] = manifest.Entries[0] },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			manifest := generation.Manifest
			manifest.Entries = append([]Entry(nil), generation.Manifest.Entries...)
			options := bounds
			mutate(&manifest, &options)
			checksum, checksumErr := manifestChecksum(manifest, maxManifestBytes)
			require.NoError(t, checksumErr)
			manifest.Checksum = checksum
			err := validateManifest(manifest, checksum, options)
			require.Error(t, err)
		})
	}

	err := validateManifest(generation.Manifest, strings.Repeat("f", 64), bounds)
	require.Error(t, err)
}

func TestDecodeManifestRejectsNoncanonicalEntryOrderWithMatchingChecksum(t *testing.T) {
	generation := buildSyntheticGeneration(t, [][]byte{[]byte("one\n"), []byte("two\n")})
	manifest := generation.Manifest
	manifest.Entries = append([]Entry(nil), manifest.Entries...)
	manifest.Entries[0], manifest.Entries[1] = manifest.Entries[1], manifest.Entries[0]
	checksum, err := manifestChecksum(manifest, maxManifestBytes)
	require.NoError(t, err)
	manifest.Checksum = checksum
	encoded, err := encodeManifest(manifest, maxManifestBytes)
	require.NoError(t, err)
	_, err = decodeManifest(encoded, checksum, normalizedTestOptions(t))
	require.ErrorContains(t, err, "canonical")
}

func buildSyntheticGeneration(t *testing.T, bodies [][]byte) Generation {
	t.Helper()
	sources := make([]Source, len(bodies))
	reader := make(syntheticReader, len(bodies))
	for index, body := range bodies {
		versionID := "00000000-0000-4000-8000-" + strings.Repeat("0", 11) + string(rune('1'+index))
		sources[index] = syntheticSource(int64(index+1), versionID, body)
		reader[sources[index].BlobSHA256] = body
	}
	generation, err := Build(t.Context(), "synthetic", sources, reader, Options{})
	require.NoError(t, err)
	return generation
}

func normalizedTestOptions(t *testing.T) Options {
	t.Helper()
	bounds, err := normalizeOptions(Options{})
	require.NoError(t, err)
	return bounds
}
