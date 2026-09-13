package emailmime

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

type zeroReader struct {
	remaining int64
	read      int64
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:n])
	r.remaining -= n
	r.read += n
	return int(n), nil
}

func TestCopyVerifiedSourceStreamsProductionLimit(t *testing.T) {
	const size = int64(128 << 20)
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	reader := &zeroReader{remaining: size}
	err = copyVerifiedSource(t.Context(), reader, spool, "source", "254bcc3fc4f27172636df4bf32de9f107f620d559b20d760197e452b97453917", size, size)
	require.NoError(t, err)
	info, err := spool.root.Stat("source")
	require.NoError(t, err)
	assert.Equal(t, size, info.Size())
	assert.Equal(t, size, reader.read)
}

func TestCopyVerifiedSourceChecksSmallDecisionByte(t *testing.T) {
	const limit = int64(8)
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(map[int64]string{-1: "below", 0: "at", 1: "above"}[delta], func(t *testing.T) {
			value := make([]byte, limit+delta)
			sum := sha256.Sum256(value)
			reader := &zeroReader{remaining: int64(len(value))}
			spool, err := createSpool(canonicalTempDir(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
			err = copyVerifiedSource(t.Context(), reader, spool, "source", hex.EncodeToString(sum[:]), int64(len(value)), limit)
			if delta <= 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Equal(t, limit+1, reader.read)
			}
		})
	}
}

func TestPayloadSpoolChecksProductionDecisionByteBeforeWrite(t *testing.T) {
	const limit = int64(128 << 20)
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(map[int64]string{-1: "below", 0: "at", 1: "above"}[delta], func(t *testing.T) {
			dir, err := createSpool(canonicalTempDir(t))
			require.NoError(t, err)
			d := &decoder{ctx: t.Context(), spool: dir, limits: Recipe().Limits}
			total := int64(0)
			reader := &zeroReader{remaining: limit + delta}
			ref, err := d.storeStream("1", document.EmailArtifactDecodedPayload, reader, limit, &total)
			if delta <= 0 {
				require.NoError(t, err)
				assert.Equal(t, limit+delta, ref.Size)
			} else {
				var policy *policyLimitError
				require.ErrorAs(t, err, &policy)
				assert.Equal(t, limit+1, reader.read)
				assert.Zero(t, total)
				assert.Equal(t, document.EmailDiagnosticPartBytesLimit, policy.code)
			}
			require.NoError(t, dir.cleanup())
		})
	}
}

func TestBodySpoolChecksProductionLimitAndSeparateAggregate(t *testing.T) {
	const limit = int64(16 << 20)
	dir, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dir.cleanup()) })
	d := &decoder{ctx: t.Context(), spool: dir, limits: Recipe().Limits}
	total := int64(0)
	ref, err := d.storeStreamLimited("1", document.EmailArtifactBodyUTF8, &zeroReader{remaining: limit}, limit, &total, d.limits.AggregateBodyUTF8Bytes, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit, document.EmailOperationCharset)
	require.NoError(t, err)
	assert.Equal(t, limit, ref.Size)
	_, err = d.storeStreamLimited("2", document.EmailArtifactBodyUTF8, &zeroReader{remaining: limit + 1}, limit, &total, d.limits.AggregateBodyUTF8Bytes, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit, document.EmailOperationCharset)
	var policy *policyLimitError
	require.ErrorAs(t, err, &policy)
	assert.Equal(t, document.EmailDiagnosticBodyUTF8Limit, policy.code)
}

func TestSpoolAggregateAndHTMLBoundsCheckDecisionByte(t *testing.T) {
	for _, tc := range []struct {
		name                string
		role                document.EmailArtifactRole
		perLimit, aggregate int64
		perCode, totalCode  document.EmailDiagnosticCode
	}{
		{"decoded aggregate", document.EmailArtifactDecodedPayload, 20, 8, document.EmailDiagnosticPartBytesLimit, document.EmailDiagnosticDecodedBytesLimit},
		{"body", document.EmailArtifactBodyUTF8, 8, 20, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit},
		{"body aggregate", document.EmailArtifactBodyUTF8, 20, 8, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit},
		{"html display", document.EmailArtifactBodyUTF8, 8, 20, document.EmailDiagnosticHTMLDisplayLimit, document.EmailDiagnosticBodyUTF8TotalLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				dir, err := createSpool(canonicalTempDir(t))
				require.NoError(t, err)
				d := &decoder{ctx: t.Context(), spool: dir, limits: Recipe().Limits}
				total := int64(0)
				reader := &zeroReader{remaining: 8 + delta}
				_, err = d.storeStreamLimited("1", tc.role, reader, tc.perLimit, &total, tc.aggregate, tc.perCode, tc.totalCode, document.EmailOperationCharset)
				if delta <= 0 {
					require.NoError(t, err)
					assert.Equal(t, 8+delta, total)
				} else {
					var policy *policyLimitError
					require.ErrorAs(t, err, &policy)
					assert.Equal(t, int64(9), reader.read)
					assert.Zero(t, total)
				}
				require.NoError(t, dir.cleanup())
			}
		})
	}
}

func TestParseStopsBeforePartAndDepthLimits(t *testing.T) {
	tests := []struct {
		name, raw    string
		parts, depth int
		code         document.EmailDiagnosticCode
	}{{"parts", "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\na\r\n--b\r\n\r\nb\r\n--b--\r\n", 2, 16, document.EmailDiagnosticPartCountLimit}, {"depth", "Content-Type: message/rfc822\r\n\r\n\r\nx", 1000, 1, document.EmailDiagnosticDepthLimit}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limits := Recipe().Limits
			limits.Parts = tc.parts
			limits.Depth = tc.depth
			dir, err := createSpool(canonicalTempDir(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, dir.cleanup()) })
			d := &decoder{ctx: context.Background(), limits: limits, spool: dir, messageIndex: map[string]int{}}
			err = d.parseEntity("1", nil, 1, 1, "1", bufioReader(tc.raw), nil)
			require.NoError(t, err)
			require.NotNil(t, d.termination)
			assert.Equal(t, tc.code, d.termination.Code)
		})
	}
}

func TestDiagnosticsStopBeforeAppendPastLimit(t *testing.T) {
	dir, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dir.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: dir, messageIndex: map[string]int{}}
	d.limits.Diagnostics = 1
	err = d.parseEntity("1", nil, 1, 1, "1", bufioReader("bad\r\nbad\r\nbad\r\n\r\nbody"), nil)
	require.NoError(t, err)
	require.NotNil(t, d.termination)
	assert.Equal(t, document.EmailDiagnosticLimit, d.termination.Code)
	assert.Equal(t, int64(1), *d.termination.Limit)
	assert.Equal(t, int64(2), *d.termination.Observed)
	stored := len(d.parts[0].Diagnostics) + len(d.messages[0].Date.Diagnostics)
	assert.Equal(t, 1, stored)
}

func bufioReader(value string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(value), multipartPeekBufferSize)
}
