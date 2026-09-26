package document

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSlicePassage(t *testing.T) {
	body := []byte("a😀b")
	got, err := SlicePassage(body, 1, 5)
	if err != nil || string(got) != "😀" {
		t.Fatal(string(got), err)
	}
	for _, r := range [][2]int{{2, 5}, {1, 4}, {-1, 2}, {0, 99}, {1, 1}} {
		if _, err := SlicePassage(body, r[0], r[1]); err == nil {
			t.Fatal(r)
		}
	}
}

func TestSlicePassagePreservesExactBytes(t *testing.T) {
	for _, body := range [][]byte{
		[]byte("first\r\nsecond"),
		[]byte("e\u0301"),
		[]byte("😀"),
	} {
		got, err := SlicePassage(body, 0, len(body))
		require.NoError(t, err)
		assert.Equal(t, body, got)
		got[0] ^= 1
		assert.NotEqual(t, body, got, "returned passage must not alias the retained body")
	}
	_, err := SlicePassage([]byte{'a', 0xff, 'b'}, 0, 1)
	require.ErrorContains(t, err, "invalid passage range")
}

func TestPassageRefV1DeterministicExactIdentity(t *testing.T) {
	body := []byte("line one\r\ne\u0301 and 😀\n")
	start := bytes.Index(body, []byte("e\u0301"))
	end := len(body) - 1
	base := validPassageIdentity()
	ref, err := NewPassageRefV1(base, body, start, end)
	require.NoError(t, err)
	require.NoError(t, ValidatePassageRefV1(ref, body))

	first, err := PassageIdentityV1(ref)
	require.NoError(t, err)
	second, err := PassageIdentityV1(ref)
	require.NoError(t, err)
	assert.Equal(t, first, second)

	changedBuild := ref
	changedBuild.RenditionBuildID = passageTestDigest("other build")
	other, err := PassageIdentityV1(changedBuild)
	require.NoError(t, err)
	assert.NotEqual(t, first, other, "same body under a different build is a different passage")

	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("e\u0301"), []byte("é"))
	require.Error(t, ValidatePassageRefV1(ref, normalized), "reference creation must not normalize source bytes")

	tampered := ref
	tampered.QuoteSHA256 = passageTestDigest("different quote")
	require.ErrorContains(t, ValidatePassageRefV1(tampered, body), "quote SHA-256")
}

func TestRuneRangeToPassageBytes(t *testing.T) {
	body := []byte("a😀e\u0301b")
	start, end, err := RuneRangeToPassageBytes(body, 1, 4)
	require.NoError(t, err)
	assert.Equal(t, "😀e\u0301", string(body[start:end]))
	for _, bounds := range [][2]int{{-1, 1}, {1, 1}, {1, 99}} {
		_, _, err := RuneRangeToPassageBytes(body, bounds[0], bounds[1])
		require.Error(t, err)
	}
}

func validPassageIdentity() PassageRefV1 {
	return PassageRefV1{
		VaultUID:         "11111111-1111-4111-8111-111111111111",
		DocumentUID:      "22222222-2222-4222-8222-222222222222",
		ContentVersionID: "33333333-3333-4333-8333-333333333333",
		SourceSHA256:     passageTestDigest("source"),
		RenditionBuildID: passageTestDigest("build"),
		AttachmentID:     passageTestDigest("attachment"),
	}
}

func passageTestDigest(value string) string { return passageSHA256([]byte(value)) }
