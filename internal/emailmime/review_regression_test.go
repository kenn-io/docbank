package emailmime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

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

func TestDecodeExtendedFilenameCharsetAndRawReference(t *testing.T) {
	result := decodeFixture(t, "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=ISO-8859-1''caf%E9.txt\r\n\r\nbody")
	filename := result.Evidence.Inventory.Parts[0].Filename
	assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
	assert.Equal(t, []int{1}, filename.Fields)
	require.NotNil(t, filename.Decoded)
	assert.Equal(t, "café.txt", *filename.Decoded)
}

func TestDecodeExtendedFilenameFailureRetainsRawReference(t *testing.T) {
	for _, test := range []struct {
		name, parameter string
		state           document.EmailInterpretationState
	}{
		{"unsupported charset", "filename*=x-unknown''name.txt", document.EmailInterpretationUnsupported},
		{"invalid percent escape", "filename*=utf-8''bad%XX.txt", document.EmailInterpretationInvalid},
		{"invalid continuation index", "filename*bad*=utf-8''name.txt", document.EmailInterpretationInvalid},
		{"unclosed quoted value", `filename="name.txt`, document.EmailInterpretationInvalid},
		{"invalid unquoted space", "filename=bad name.txt", document.EmailInterpretationInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := decodeFixture(t, "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; "+test.parameter+"\r\n\r\nbody")
			filename := result.Evidence.Inventory.Parts[0].Filename
			assert.Equal(t, test.state, filename.State)
			assert.Equal(t, []int{1}, filename.Fields)
			assert.Nil(t, filename.Decoded)
		})
	}
}

func TestDecodeQuotedFilenameUsesMIMEBackslashEscaping(t *testing.T) {
	result := decodeFixture(t, "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"a\\ b.txt\"\r\n\r\nbody")
	filename := result.Evidence.Inventory.Parts[0].Filename
	assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
	require.NotNil(t, filename.Decoded)
	assert.Equal(t, "a b.txt", *filename.Decoded)
	assert.Equal(t, []int{1}, filename.Fields)
}

func TestDecodeRejectsEmbeddedSignNumericTimezone(t *testing.T) {
	result := decodeFixture(t, "Date: 09 Sep 2026 12:34:56 +-100\r\n\r\nbody")
	date := result.Evidence.Inventory.Messages[0].Date
	assert.Equal(t, document.EmailDateInvalid, date.State)
	assert.Nil(t, date.UTC)
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

type dataThenErrorReader struct {
	data []byte
	err  error
}

func (r *dataThenErrorReader) Read(value []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(value, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestPayloadInfrastructureReadAndBodySpoolErrorsPropagate(t *testing.T) {
	sentinel := errors.New("synthetic payload read failure")
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, artifacts: []storedArtifact{}, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, messageIndex: map[string]int{}}
	err = d.parseEntity("1", nil, 1, 1, "1", &dataThenErrorReader{data: []byte("prefix"), err: sentinel}, &parsedHeaderBlock{raw: []byte{}, fields: []parsedHeader{}})
	require.ErrorIs(t, err, sentinel)
	assert.Nil(t, d.parts[0].Payload)
	assert.Len(t, d.artifacts, 1, "only the completed raw-header artifact remains")

	ref, err := d.storeBytes("1", document.EmailArtifactDecodedPayload, []byte("body"))
	require.NoError(t, err)
	name := d.artifactFilename("1", document.EmailArtifactDecodedPayload)
	require.NoError(t, spool.remove(name))
	bodyRef, state, diagnostics, err := d.deriveBody("1", "text/plain", "utf-8", name)
	require.Error(t, err)
	assert.Nil(t, bodyRef)
	assert.Equal(t, document.EmailDisplayFailed, state)
	assert.Empty(t, diagnostics)
	assert.Equal(t, document.EmailArtifactDecodedPayload, ref.Role)

	spool2, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool2.cleanup()) })
	d2 := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool2, artifacts: []storedArtifact{}}
	payload, err := d2.storeBytes("1", document.EmailArtifactDecodedPayload, []byte("body"))
	require.NoError(t, err)
	require.NoError(t, spool2.root.Mkdir("artifact-000001", 0o700))
	bodyRef, state, diagnostics, err = d2.deriveBody("1", "text/plain", "utf-8", d2.artifactFilename("1", document.EmailArtifactDecodedPayload))
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	assert.Nil(t, bodyRef)
	assert.Equal(t, document.EmailDisplayFailed, state)
	assert.Empty(t, diagnostics)
	assert.Equal(t, document.EmailArtifactDecodedPayload, payload.Role)
}

func TestSpoolRootRejectsSymlinkedAncestor(t *testing.T) {
	owner := t.TempDir()
	actual := filepath.Join(owner, "actual")
	require.NoError(t, os.MkdirAll(filepath.Join(actual, "spools"), 0o700))
	alias := filepath.Join(owner, "alias")
	require.NoError(t, os.Symlink(actual, alias))
	_, err := createSpool(filepath.Join(alias, "spools"))
	require.Error(t, err)
	_, err = RecoverStale(t.Context(), filepath.Join(alias, "spools"))
	require.Error(t, err)
}
