package emailmime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func TestDecodeKeepsRawAndDisplayIdentities(t *testing.T) {
	headers := "Subject: First\r\nSubject: Second\r\nContent-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: base64\r\n\r\n"
	raw := []byte(headers + "Y2Fm6Q==")
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	require.Equal(t, document.EmailOutcome("decoded"), result.Evidence.Outcome)
	require.Len(t, result.Evidence.Inventory.Parts, 1)
	require.Len(t, result.Evidence.Inventory.Messages[0].Fields.Subject, 2)
	for _, tc := range []struct {
		role string
		want []byte
	}{
		{"raw_headers", []byte(headers)}, {"decoded_payload", []byte{'c', 'a', 'f', 0xe9}}, {"body_utf8", []byte("café")},
	} {
		stream, err := result.OpenArtifact(t.Context(), "1", tc.role)
		require.NoError(t, err)
		got, readErr := io.ReadAll(stream)
		closeErr := stream.Close()
		require.NoError(t, errors.Join(readErr, closeErr))
		require.Equal(t, tc.want, got)
	}
	part := result.Evidence.Inventory.Parts[0]
	require.NotEqual(t, part.Payload.SHA256, part.BodyUTF8.SHA256)
}

func TestDecodeDoesNotPublishInvalidUTF8BodyPrefix(t *testing.T) {
	raw := []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nvalid\xff")
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	part := result.Evidence.Inventory.Parts[0]
	require.Nil(t, part.BodyUTF8)
	require.Equal(t, document.EmailDisplayFailed, result.Evidence.Inventory.Messages[0].Alternatives[0].DisplayState)
	for _, artifact := range result.Artifacts() {
		require.NotEqual(t, document.EmailArtifactBodyUTF8, artifact.Reference.Role)
	}
}

func TestEnclosedAttachmentOwnsItsMessageBody(t *testing.T) {
	result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nouter\r\n--m\r\nContent-Type: message/rfc822\r\nContent-Disposition: attachment; filename=forward.eml\r\n\r\nSubject: Forwarded\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<i>inner</i>\r\n--m--\r\n")
	require.Len(t, result.Evidence.Inventory.Messages, 2)
	outer, enclosed := result.Evidence.Inventory.Messages[0], result.Evidence.Inventory.Messages[1]
	require.Len(t, outer.Alternatives, 1)
	assert.Equal(t, "1.1", outer.Alternatives[0].PartPath)
	require.Len(t, enclosed.Alternatives, 1)
	assert.Equal(t, "1.2.1", enclosed.Alternatives[0].PartPath)
	require.NotNil(t, enclosed.SelectedBodyPath)
	assert.Equal(t, "1.2.1", *enclosed.SelectedBodyPath)
}

func TestMalformedHeaderOverflowReturnsCanonicalReceipt(t *testing.T) {
	raw := []byte(strings.Repeat("\rbad\r\n", 4097) + "\r\nbody")
	result := decodePublicFixture(t, raw)
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, result.Evidence.Inventory.Termination.Code)
}

func TestDecodeCompressedHeaderProducesCanonicalEvidence(t *testing.T) {
	result := decodePublicFixture(t, []byte("Subject: =?utf-8?B?YWJj?=\r\n\r\nbody"))
	field := result.Evidence.Inventory.Messages[0].Fields.Subject[0]
	assert.Equal(t, document.EmailInterpretationDecoded, field.State)
	require.NotNil(t, field.Text)
	assert.Equal(t, "abc", *field.Text)
}

func TestDecodeHeaderDisplayProductionLimitMatrix(t *testing.T) {
	const limit = 1 << 20
	for _, delta := range []int{-1, 0, 1} {
		t.Run(map[int]string{-1: "below", 0: "at", 1: "above"}[delta], func(t *testing.T) {
			first := strings.Repeat("a", limit/2)
			second := strings.Repeat("b", limit/2+delta)
			raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
				"--b\r\nContent-Type: message/rfc822\r\n\r\nSubject: " + first + "\r\nContent-Type: application/octet-stream\r\n\r\nx\r\n" +
				"--b\r\nContent-Type: message/rfc822\r\n\r\nSubject: " + second + "\r\nContent-Type: application/octet-stream\r\n\r\ny\r\n--b--\r\n")
			result := decodePublicFixture(t, raw)
			require.Len(t, result.Evidence.Inventory.Messages, 3)
			firstField := result.Evidence.Inventory.Messages[1].Fields.Subject[0]
			secondField := result.Evidence.Inventory.Messages[2].Fields.Subject[0]
			assert.Equal(t, document.EmailInterpretationDecoded, firstField.State)
			if delta <= 0 {
				assert.Equal(t, document.EmailInterpretationDecoded, secondField.State)
				require.NotNil(t, secondField.Text)
				assert.Len(t, *secondField.Text, limit/2+delta)
			} else {
				assert.Equal(t, document.EmailInterpretationUnsupported, secondField.State)
				assert.Nil(t, secondField.Text)
				require.Len(t, result.Evidence.Inventory.Messages[2].Diagnostics, 1)
				assert.Equal(t, document.EmailDiagnosticHeaderDisplayLimit, result.Evidence.Inventory.Messages[2].Diagnostics[0].Code)
			}
		})
	}
}

func TestDecodeMalformedPartHeadersRemainPartial(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" + strings.Repeat("bad\r\n", 4095) + "Content-Type: invalid@type\r\n\r\nbody\r\n--b--\r\n")
	result := decodePublicFixture(t, raw)
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticBoundaryInvalid, result.Evidence.Inventory.Termination.Code)
	require.Len(t, result.Evidence.Inventory.Parts, 1)
}

func decodePublicFixture(t *testing.T, raw []byte) *Result {
	t.Helper()
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	require.NotNil(t, result)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	_, _, err = document.MarshalEmailV1(result.Evidence)
	require.NoError(t, err)
	return result
}

func TestDecodeDoesNotSelectAttachmentOrEncryptedDescendants(t *testing.T) {
	tests := []struct {
		name, raw, selected string
	}{
		{"attachment container", "Content-Type: multipart/mixed; boundary=o\r\n\r\n--o\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nouter\r\n--o\r\nContent-Type: multipart/alternative; boundary=i\r\nContent-Disposition: attachment\r\n\r\n--i\r\nContent-Type: text/html; charset=utf-8\r\n\r\nattachment\r\n--i--\r\n--o--\r\n", "1.1"},
		{"encrypted container", "Content-Type: multipart/encrypted; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nciphertext\r\n--b--\r\n", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := decodeFixture(t, test.raw)
			message := result.Evidence.Inventory.Messages[0]
			if test.selected == "" {
				assert.Nil(t, message.SelectedBodyPath)
				assert.Empty(t, message.Alternatives)
			} else {
				require.NotNil(t, message.SelectedBodyPath)
				assert.Equal(t, test.selected, *message.SelectedBodyPath)
				assert.Len(t, message.Alternatives, 1)
			}
		})
	}
}

func TestDecodeHeaderLimitReturnsCanonicalPartialEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  []byte
		code document.EmailDiagnosticCode
		lim  int64
	}{
		{"bytes", []byte("X: " + strings.Repeat("a", 1<<20) + "\r\n\r\nbody"), document.EmailDiagnosticHeaderBytesLimit, 1 << 20},
		{"fields", []byte(strings.Repeat("X: a\r\n", 4097) + "\r\nbody"), document.EmailDiagnosticHeaderFieldsLimit, 4096},
	} {
		t.Run(test.name, func(t *testing.T) {
			sum := sha256.Sum256(test.raw)
			result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(test.raw)), bytes.NewReader(test.raw), canonicalTempDir(t))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, result.Close()) })
			require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
			assert.Equal(t, document.EmailVerificationVerified, result.Evidence.Source.Verification)
			assert.Empty(t, result.Evidence.Inventory.Parts)
			assert.Empty(t, result.Evidence.Inventory.Messages)
			require.NotNil(t, result.Evidence.Inventory.Termination)
			assert.Equal(t, test.code, result.Evidence.Inventory.Termination.Code)
			assert.Equal(t, "1", *result.Evidence.Inventory.Termination.Path)
			assert.Equal(t, test.lim, *result.Evidence.Inventory.Termination.Limit)
			assert.Equal(t, test.lim+1, *result.Evidence.Inventory.Termination.Observed)
			_, _, err = document.MarshalEmailV1(result.Evidence)
			require.NoError(t, err)
		})
	}
}

func TestDecodeHeaderProductionBoundaryAtAndBelow(t *testing.T) {
	for _, delta := range []int{-1, 0} {
		t.Run("bytes/"+map[int]string{-1: "below", 0: "at"}[delta], func(t *testing.T) {
			const suffix = "\r\n\r\n"
			want := (1 << 20) + delta
			raw := []byte("X: " + strings.Repeat("a", want-len("X: ")-len(suffix)) + suffix + "body")
			result := decodeBytes(t, raw)
			assert.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
			assert.Equal(t, int64(want), result.Evidence.Inventory.Parts[0].HeaderBlock.Size)
		})
		t.Run("fields/"+map[int]string{-1: "below", 0: "at"}[delta], func(t *testing.T) {
			want := 4096 + delta
			raw := []byte(strings.Repeat("X: a\r\n", want) + "\r\nbody")
			result := decodeBytes(t, raw)
			assert.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
			assert.Len(t, result.Evidence.Inventory.Parts[0].Headers, want)
		})
	}
}

func decodeBytes(t *testing.T, raw []byte) *Result {
	t.Helper()
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	return result
}

func TestDecodeInvalidUTF8HeaderRetainsRawEvidence(t *testing.T) {
	raw := []byte("Subject: bad\xff\r\n\r\nbody")
	sum := sha256.Sum256(raw)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewReader(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	field := result.Evidence.Inventory.Messages[0].Fields.Subject[0]
	assert.Equal(t, document.EmailInterpretationInvalid, field.State)
	assert.Nil(t, field.Text)
	stream, err := result.OpenArtifact(t.Context(), "1", "raw_headers")
	require.NoError(t, err)
	retained, err := io.ReadAll(stream)
	require.NoError(t, errors.Join(err, stream.Close()))
	assert.Equal(t, raw[:bytes.Index(raw, []byte("\r\n\r\n"))+4], retained)
}

func TestMultipartCancellationPropagates(t *testing.T) {
	dir, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, dir.cleanup()) })
	require.NoError(t, dir.root.WriteFile("input", []byte("--b\r\nX: y\r\n\r\nbody\r\n--b--\r\n"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d := &decoder{ctx: ctx, limits: Recipe().Limits, spool: dir}
	err = d.parseMultipart("1", 1, "1", "input", "b")
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, d.termination)
}

func TestMalformedLongDelimiterRemainsCanonicalPartial(t *testing.T) {
	result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\nfirst\r\n--b"+strings.Repeat(" ", 8192)+"invalid\r\n")
	require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	require.Equal(t, document.EmailDiagnosticCode("boundary_invalid"), result.Evidence.Inventory.Termination.Code)
}

func TestDecodeMultipartRetainsExactHeadersAndTransferBytes(t *testing.T) {
	for _, test := range []struct{ newline, boundary string }{{"\n", "b"}, {"\r\n", "b"}, {"\r\n", "b "}} {
		newline, boundary := test.newline, test.boundary
		header := "x-Custom: one" + newline + "\tfolded" + newline + "X-CUSTOM: two" + newline +
			"Content-Type: application/octet-stream" + newline + "Content-Transfer-Encoding: base64" + newline + newline
		raw := "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"" + newline + newline + "preamble" + newline +
			"--" + boundary + newline + header + "YQBi" + newline + "--" + boundary + newline + newline + "second" + newline + "--" + boundary + "--"
		result := decodeFixture(t, raw)
		require.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
		require.Len(t, result.Evidence.Inventory.Parts, 3)
		for role, want := range map[string]string{"raw_headers": header, "decoded_payload": "a\x00b"} {
			reader, err := result.OpenArtifact(t.Context(), "1.1", role)
			require.NoError(t, err)
			got, readErr := io.ReadAll(reader)
			require.NoError(t, errors.Join(readErr, reader.Close()))
			assert.Equal(t, want, string(got))
		}
		headers := result.Evidence.Inventory.Parts[1].Headers
		require.Len(t, headers, 4)
		assert.Equal(t, "x-custom", *headers[0].Name)
		assert.Equal(t, "x-custom", *headers[1].Name)
		assert.Equal(t, int64(len("x-Custom: one"+newline+"\tfolded"+newline)), headers[1].Offset)
	}
}

func TestDecodeUnsupportedMultipartLinesKeepVerifiedPartialInventory(t *testing.T) {
	for _, body := range []string{
		strings.Repeat("x", 5000) + "\r\n--b--\r\n",
		"--b" + strings.Repeat(" ", 5000) + "\r\n\r\nbody\r\n--b--\r\n",
		"--b\r\nmalformed header\r\n\r\nbody\r\n--b--\r\n",
	} {
		result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n"+body)
		assert.Equal(t, document.EmailVerificationVerified, result.Evidence.Source.Verification)
		require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
		require.NotNil(t, result.Evidence.Inventory.Termination)
		assert.Equal(t, document.EmailDiagnosticBoundaryInvalid, result.Evidence.Inventory.Termination.Code)
		reader, err := result.OpenArtifact(t.Context(), "1", "decoded_payload")
		require.NoError(t, err)
		got, readErr := io.ReadAll(reader)
		require.NoError(t, errors.Join(readErr, reader.Close()))
		assert.Equal(t, body, string(got))
	}
}

func TestDecodeTruncatedMultipartNeverClaimsCompleteInventory(t *testing.T) {
	for _, body := range []string{"", "preamble", "preamble\r\n", "--b\r\n", "--b\r\nX: y\r\n", "--b\r\n--b--", "--b\r\n\r\nbody\r\n--b\r\nX: y\r\n"} {
		result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n"+body)
		require.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State, "body %q", body)
		require.NotNil(t, result.Evidence.Inventory.Termination)
		if body == "--b\r\n--b--" {
			assert.Equal(t, document.EmailDiagnosticBoundaryInvalid, result.Evidence.Inventory.Termination.Code)
		} else {
			assert.Equal(t, document.EmailDiagnosticBoundaryUnclosed, result.Evidence.Inventory.Termination.Code, "body %q", body)
		}
	}
}

func TestDecodeMultipartUsesDelimiterLineEndings(t *testing.T) {
	for _, body := range []string{
		"--b\r\r\n--b--\r\n",
		"--b\r\n\r\nbody\n--b\nstill body\r\n--b--\r\n",
	} {
		result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n"+body)
		assert.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
	}
}
