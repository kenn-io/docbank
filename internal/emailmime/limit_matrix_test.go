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

func TestHeaderByteLimitMatrixAtProductionBoundary(t *testing.T) {
	const limit = int64(8)
	for _, aggregate := range []bool{false, true} {
		for _, delta := range []int64{-1, 0, 1} {
			name := map[int64]string{-1: "below", 0: "at", 1: "above"}[delta]
			t.Run(map[bool]string{false: "entity/", true: "aggregate/"}[aggregate]+name, func(t *testing.T) {
				perLimit, aggregateLimit := limit, int64(100)
				if aggregate {
					perLimit, aggregateLimit = 100, limit
				}
				raw := bytes.Repeat([]byte{'x'}, int(limit+delta))
				var bytesUsed int64
				fieldsUsed := 0
				block, err := readHeaderBlock(context.Background(), bufio.NewReaderSize(bytes.NewReader(raw), 2), perLimit, 20, &bytesUsed, aggregateLimit, &fieldsUsed, 20)
				if delta <= 0 {
					require.NoError(t, err)
					assert.Len(t, block.raw, int(limit+delta))
					assert.Equal(t, limit+delta, bytesUsed)
					return
				}
				var policy *policyLimitError
				require.ErrorAs(t, err, &policy)
				assert.Equal(t, limit, bytesUsed)
				assert.Equal(t, limit+1, policy.observed)
				if aggregate {
					assert.Equal(t, document.EmailDiagnosticHeaderTotalBytesLimit, policy.code)
				} else {
					assert.Equal(t, document.EmailDiagnosticHeaderBytesLimit, policy.code)
				}
			})
		}
	}
}

func TestHeaderFieldLimitMatrixBeforeNextFieldAppend(t *testing.T) {
	const limit = 2
	for _, aggregate := range []bool{false, true} {
		for _, delta := range []int{-1, 0, 1} {
			name := map[int]string{-1: "below", 0: "at", 1: "above"}[delta]
			t.Run(map[bool]string{false: "entity/", true: "aggregate/"}[aggregate]+name, func(t *testing.T) {
				perLimit, aggregateLimit := limit, 20
				if aggregate {
					perLimit, aggregateLimit = 20, limit
				}
				raw := []byte(strings.Repeat("X: y\n", limit+delta) + "\n")
				var bytesUsed int64
				fieldsUsed := 0
				block, err := readHeaderBlock(context.Background(), bufio.NewReader(bytes.NewReader(raw)), 100, perLimit, &bytesUsed, 100, &fieldsUsed, aggregateLimit)
				if delta <= 0 {
					require.NoError(t, err)
					assert.Len(t, block.fields, limit+delta)
					assert.Equal(t, limit+delta, fieldsUsed)
					return
				}
				var policy *policyLimitError
				require.ErrorAs(t, err, &policy)
				assert.Equal(t, limit, fieldsUsed)
				assert.Equal(t, int64(limit+1), policy.observed)
				assert.Equal(t, int64(len(strings.Repeat("X: y\n", limit))), bytesUsed)
				if aggregate {
					assert.Equal(t, document.EmailDiagnosticHeaderTotalFieldsLimit, policy.code)
				} else {
					assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, policy.code)
				}
			})
		}
	}
}

func TestPartAndDepthLimitMatrixStopsBeforeAppend(t *testing.T) {
	const limit = 2
	partInputs := []string{
		"\r\nbody",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\na\r\n--b--\r\n",
		"Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\na\r\n--b\r\n\r\nb\r\n--b--\r\n",
	}
	depthInputs := []string{
		"\r\nbody",
		"Content-Type: message/rfc822\r\n\r\n\r\nbody",
		"Content-Type: message/rfc822\r\n\r\nContent-Type: message/rfc822\r\n\r\n\r\nbody",
	}
	for _, test := range []struct {
		name   string
		inputs []string
		code   document.EmailDiagnosticCode
	}{
		{"parts", partInputs, document.EmailDiagnosticPartCountLimit},
		{"depth", depthInputs, document.EmailDiagnosticDepthLimit},
	} {
		for index, raw := range test.inputs {
			delta := index - 1
			t.Run(test.name+"/"+map[int]string{-1: "below", 0: "at", 1: "above"}[delta], func(t *testing.T) {
				spool, err := createSpool(canonicalTempDir(t))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
				limits := Recipe().Limits
				limits.Parts = 100
				limits.Depth = 100
				if test.name == "parts" {
					limits.Parts = limit
				} else {
					limits.Depth = limit
				}
				d := &decoder{ctx: t.Context(), limits: limits, spool: spool, artifacts: []storedArtifact{}, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, messageIndex: map[string]int{}}
				err = d.parseEntity("1", nil, 1, 1, "1", bufioReader(raw), nil)
				require.NoError(t, err)
				if delta <= 0 {
					assert.Nil(t, d.termination)
					assert.Len(t, d.parts, limit+delta)
				} else {
					require.NotNil(t, d.termination)
					assert.Equal(t, test.code, d.termination.Code)
					assert.Len(t, d.parts, limit)
					assert.Equal(t, int64(limit), *d.termination.Limit)
					assert.Equal(t, int64(limit+1), *d.termination.Observed)
				}
			})
		}
	}
}

func TestDiagnosticAndDetailLimitMatrixBeforeAppend(t *testing.T) {
	const limit = 2
	for _, delta := range []int{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.Diagnostics = limit
		var diagnostics []document.EmailDiagnosticV1
		for range limit + delta {
			diagnostics = append(diagnostics, d.diagnostic(document.EmailDiagnosticMalformedHeader, document.EmailOperationHeaders, "1", nil, "detail")...)
		}
		assert.Len(t, diagnostics, min(limit+delta, limit))
		if delta <= 0 {
			assert.Nil(t, d.termination)
		} else {
			require.NotNil(t, d.termination)
			assert.Equal(t, document.EmailDiagnosticLimit, d.termination.Code)
		}
	}
	for _, delta := range []int{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.DiagnosticDetailBytes = limit
		value := strings.Repeat("x", limit+delta)
		diagnostic := d.diagnostic(document.EmailDiagnosticMalformedHeader, document.EmailOperationHeaders, "1", nil, value)[0]
		assert.Len(t, diagnostic.Detail, min(len(value), limit))
	}
	d := &decoder{limits: Recipe().Limits}
	d.limits.DiagnosticDetailBytes = 5
	diagnostic := d.diagnostic(document.EmailDiagnosticMalformedHeader, document.EmailOperationHeaders, "1", nil, "ééé")[0]
	assert.Equal(t, "éé", diagnostic.Detail, "truncation retains only complete UTF-8 runes")
}

func TestHeaderDisplayLimitMatrixBeforeStoredInterpretation(t *testing.T) {
	const limit = int64(8)
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = limit
		value := strings.Repeat("x", int(limit+delta))
		block := parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "subject", value: value, valid: true}}}
		field := d.interpretMessage("1", block).Fields.Subject[0]
		if delta <= 0 {
			assert.Equal(t, document.EmailInterpretationDecoded, field.State)
			assert.Equal(t, limit+delta, d.headerDisplayBytes)
		} else {
			assert.Equal(t, document.EmailInterpretationUnsupported, field.State)
			assert.Zero(t, d.headerDisplayBytes)
		}
	}
}
