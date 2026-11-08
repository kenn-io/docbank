package notemarkdown

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestParseAdmission(t *testing.T) {
	for name, input := range map[string][]byte{
		"utf8": {0xff}, "nul": {'a', 0, 'b'},
	} {
		t.Run(name, func(t *testing.T) {
			n, err := Parse(t.Context(), input)
			require.ErrorIs(t, err, ErrInvalidInput)
			require.Equal(t, Note{}, n)
		})
	}
	n, err := Parse(t.Context(), bytes.Repeat([]byte{'x'}, (4<<20)+1))
	requireLimit(t, err, "input_bytes")
	require.Equal(t, Note{}, n)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	n, err = Parse(ctx, []byte("test"))
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, Note{}, n)
	n, err = Parse(t.Context(), bytes.Repeat([]byte{'\n'}, 65537))
	requireLimit(t, err, "line_breaks")
	require.Equal(t, Note{}, n)
}

func requireLimit(tb testing.TB, err error, resource string) {
	tb.Helper()
	require.ErrorIs(tb, err, ErrLimit)
	var limited *LimitError
	require.ErrorAs(tb, err, &limited)
	require.Equal(tb, resource, limited.Resource)
}

func TestPropertyParsePlainIdentity(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers[int](0, 2048))
		input := []byte(strings.Repeat("é", size))
		before := bytes.Clone(input)
		a, err := Parse(ht.Context(), input)
		require.NoError(ht, err)
		b, err := Parse(ht.Context(), input)
		require.NoError(ht, err)
		require.Equal(ht, a, b)
		require.Equal(ht, before, input)
		digest := sha256.Sum256(input)
		require.Equal(ht, hex.EncodeToString(digest[:]), a.SourceSHA256)
		require.Equal(ht, len(input), a.SourceBytes)
	}, hegel.WithTestCases(1000))
}

func TestLimitErrorDoesNotEchoInput(t *testing.T) {
	input := "private-synthetic-marker" + strings.Repeat("x", 4<<20)
	_, err := Parse(t.Context(), []byte(input))
	requireLimit(t, err, "input_bytes")
	require.NotContains(t, err.Error(), "private-synthetic-marker")
}
