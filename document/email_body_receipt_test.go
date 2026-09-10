package document

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func bodyReceiptFixture() EmailBodyReceiptV1 {
	return EmailBodyReceiptV1{ContractVersion: "docbank-email-body-receipt/v1", SourceSHA256: strings.Repeat("1", 64), SourceSize: 375, EmailGenerationID: strings.Repeat("2", 64), EmailChecksum: strings.Repeat("3", 64), PartPath: "1.1", BodySHA256: strings.Repeat("4", 64), BodySize: 22, BodyRecipeFingerprint: strings.Repeat("5", 64), EvidenceChecksum: strings.Repeat("6", 64), RenditionChecksum: strings.Repeat("7", 64), MarkdownChecksum: strings.Repeat("8", 64)}
}

func TestEmailBodyReceiptCanonicalAndIdentity(t *testing.T) {
	r := bodyReceiptFixture()
	encoded, checksum, err := MarshalEmailBodyReceiptV1(r)
	require.NoError(t, err)
	got, gotChecksum, err := DecodeEmailBodyReceiptV1(encoded)
	require.NoError(t, err)
	require.Equal(t, r, got)
	require.Equal(t, checksum, gotChecksum)
	id, err := EmailBodyBuildID(r)
	require.NoError(t, err)
	require.Equal(t, "f15ed5c98b56380d436b21479a71df0eda250fb895c51bcb69cffed80cfa8e52", id)
	auth, err := EmailBodyAuthorizationChecksum(r.SourceSHA256, r.SourceSize, r.BodyRecipeFingerprint)
	require.NoError(t, err)
	require.Equal(t, "105fcb676873879150c962d148731ce981bba4e2b440d96908bba556e9ba48b9", auth)
	op, err := EmailBodyOperationID(r.BodyRecipeFingerprint)
	require.NoError(t, err)
	require.Equal(t, "docbank-email-body:"+strings.Repeat("5", 64), op)
	for _, mutate := range []func(*EmailBodyReceiptV1){func(r *EmailBodyReceiptV1) { r.PartPath = "1.2" }, func(r *EmailBodyReceiptV1) { r.SourceSHA256 = strings.Repeat("a", 64) }, func(r *EmailBodyReceiptV1) { r.EmailGenerationID = strings.Repeat("b", 64) }, func(r *EmailBodyReceiptV1) { r.BodyRecipeFingerprint = strings.Repeat("c", 64) }, func(r *EmailBodyReceiptV1) { r.EvidenceChecksum = strings.Repeat("d", 64) }, func(r *EmailBodyReceiptV1) { r.RenditionChecksum = strings.Repeat("e", 64) }} {
		other := r
		mutate(&other)
		changed, err := EmailBodyBuildID(other)
		require.NoError(t, err)
		require.NotEqual(t, id, changed)
	}
	for _, bad := range [][]byte{append(bytes.Clone(encoded), ' '), bytes.Replace(encoded, []byte(`"source_size":375`), []byte(`"source_size":null`), 1), bytes.Replace(encoded, []byte(`,"source_size":375`), nil, 1), bytes.Replace(encoded, []byte(`"source_size":375`), []byte(`"source_size":375,"source_size":375`), 1), bytes.Replace(encoded, []byte(`"source_size":375`), []byte(`"extra":375`), 1), bytes.Repeat([]byte(" "), 4097)} {
		_, _, err := DecodeEmailBodyReceiptV1(bad)
		require.Error(t, err)
	}
	for _, mutate := range []func(*EmailBodyReceiptV1){func(r *EmailBodyReceiptV1) { r.SourceSize = 0 }, func(r *EmailBodyReceiptV1) { r.SourceSize = 128<<20 + 1 }, func(r *EmailBodyReceiptV1) { r.BodySize = 0 }, func(r *EmailBodyReceiptV1) { r.BodySize = 16<<20 + 1 }, func(r *EmailBodyReceiptV1) { r.EmailChecksum = "bad" }, func(r *EmailBodyReceiptV1) { r.PartPath = "01" }, func(r *EmailBodyReceiptV1) { r.ContractVersion = "unknown" }} {
		other := r
		mutate(&other)
		_, _, err := MarshalEmailBodyReceiptV1(other)
		require.Error(t, err)
		_, err = EmailBodyBuildID(other)
		require.Error(t, err)
	}
}
