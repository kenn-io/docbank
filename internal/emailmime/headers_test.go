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
