package emailmime

import (
	"bufio"
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestReadHeaderBlockPreservesExactLineEndingsOrderAndSpans(t *testing.T) {
	raw := []byte("X-A: one\r\n\tfold\nBroken\r\nX-A: two\n\nbody")
	var bytesUsed int64
	fieldsUsed := 0
	block, err := readHeaderBlock(context.Background(), bufio.NewReaderSize(bytes.NewReader(raw), 7), 1024, 10, &bytesUsed, 2048, &fieldsUsed, 20)
	require.NoError(t, err)
	assert.Equal(t, []byte("X-A: one\r\n\tfold\nBroken\r\nX-A: two\n\n"), block.raw)
	require.Len(t, block.fields, 3)
	assert.Equal(t, []string{"x-a", "", "x-a"}, []string{block.fields[0].name, block.fields[1].name, block.fields[2].name})
	assert.Equal(t, int64(len("X-A: one\r\n\tfold\n")), block.fields[0].length)
	assert.False(t, block.fields[1].valid)
	assert.Equal(t, int64(len(block.raw)), bytesUsed)
}

func TestReadHeaderBlockRejectsDecisionByteBeforeAppend(t *testing.T) {
	for _, limit := range []int64{7, 8, 9} {
		t.Run(string(rune('0'+limit)), func(t *testing.T) {
			raw := bytes.Repeat([]byte{'x'}, int(limit)+1)
			var bytesUsed int64
			fieldsUsed := 0
			block, err := readHeaderBlock(context.Background(), bufio.NewReaderSize(bytes.NewReader(raw), 2), limit, 10, &bytesUsed, 100, &fieldsUsed, 10)
			require.Error(t, err)
			var policy *policyLimitError
			require.ErrorAs(t, err, &policy)
			assert.Equal(t, document.EmailDiagnosticHeaderBytesLimit, policy.code)
			assert.Empty(t, block.raw)
			assert.Equal(t, limit, bytesUsed)
			assert.Equal(t, limit+1, policy.observed)
		})
	}
}

func TestReadHeaderBlockEnforcesFieldCountBeforeAppend(t *testing.T) {
	raw := []byte("A: 1\nB: 2\n\n")
	var bytesUsed int64
	fieldsUsed := 0
	_, err := readHeaderBlock(context.Background(), bufio.NewReader(bytes.NewReader(raw)), int64(len(raw)), 1, &bytesUsed, 100, &fieldsUsed, 10)
	var policy *policyLimitError
	require.ErrorAs(t, err, &policy)
	assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, policy.code)
	assert.Equal(t, 1, fieldsUsed)
	assert.Equal(t, int64(2), policy.observed)
	assert.Equal(t, int64(len("A: 1\n")), bytesUsed)
}

func TestReadHeaderBlockDoesNotFoldMalformedContinuation(t *testing.T) {
	var bytesUsed int64
	fieldsUsed := 0
	block, err := readHeaderBlock(context.Background(), bufio.NewReader(strings.NewReader("A: one\r\n bad\rending\r\n\r\n")), 100, 10, &bytesUsed, 100, &fieldsUsed, 10)
	require.NoError(t, err)
	require.Len(t, block.fields, 2)
	assert.True(t, block.fields[0].valid)
	assert.Equal(t, "one", block.fields[0].value)
	assert.False(t, block.fields[1].valid)
}

func TestHeaderReservationsMatchMalformedEntries(t *testing.T) {
	for name, raw := range map[string]string{
		"CR prefix":                "A: one\r\n\rbad\r\n\r\n",
		"embedded CR continuation": "A: one\r\n bad\rending\r\n\r\n",
		"unfinished continuation":  "A: one\r\n bad",
	} {
		t.Run(name, func(t *testing.T) {
			var bytesUsed int64
			fieldsUsed := 0
			_, err := readHeaderBlock(t.Context(), bufio.NewReader(strings.NewReader(raw)), 100, 1, &bytesUsed, 100, &fieldsUsed, 1)
			var policy *policyLimitError
			require.ErrorAs(t, err, &policy)
			assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, policy.code)
			assert.Equal(t, int64(2), policy.observed)
		})
	}
}

func TestMalformedHeaderFieldLimitMatrix(t *testing.T) {
	const limit = 3
	forms := map[string]func(int) string{
		"CR prefixed": func(fields int) string { return strings.Repeat("\rbad\r\n", fields) + "\r\n" },
		"embedded CR continuation": func(fields int) string {
			return "A: one\r\n" + strings.Repeat(" bad\rending\r\n", fields-1) + "\r\n"
		},
		"unfinished continuation": func(fields int) string {
			return strings.Repeat("A: one\r\n", fields-1) + " bad"
		},
	}
	for form, build := range forms {
		for _, aggregate := range []bool{false, true} {
			for _, delta := range []int{-1, 0, 1} {
				name := form + "/" + map[bool]string{false: "entity", true: "aggregate"}[aggregate] + "/" + map[int]string{-1: "below", 0: "at", 1: "above"}[delta]
				t.Run(name, func(t *testing.T) {
					perLimit, aggregateLimit := limit, 20
					if aggregate {
						perLimit, aggregateLimit = 20, limit
					}
					var bytesUsed int64
					fieldsUsed := 0
					block, err := readHeaderBlock(t.Context(), bufio.NewReader(strings.NewReader(build(limit+delta))), 1000, perLimit, &bytesUsed, 1000, &fieldsUsed, aggregateLimit)
					if delta <= 0 {
						require.NoError(t, err)
						assert.Len(t, block.fields, limit+delta)
						assert.Equal(t, limit+delta, fieldsUsed)
						return
					}
					var policy *policyLimitError
					require.ErrorAs(t, err, &policy)
					assert.Equal(t, int64(limit+1), policy.observed)
					if aggregate {
						assert.Equal(t, document.EmailDiagnosticHeaderTotalFieldsLimit, policy.code)
					} else {
						assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, policy.code)
					}
				})
			}
		}
	}
}
