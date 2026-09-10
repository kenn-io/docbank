package emailmime

import (
	"bufio"
	"bytes"
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

func TestMalformedHeaderOverflowReturnsCanonicalReceipt(t *testing.T) {
	raw := []byte(strings.Repeat("\rbad\r\n", 4097) + "\r\nbody")
	result := decodePublicFixture(t, raw)
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticHeaderFieldsLimit, result.Evidence.Inventory.Termination.Code)
}

func TestDecodedHeaderBudgetUsesActualOutput(t *testing.T) {
	for _, test := range []struct {
		name, encoded, decoded string
		budget                 int64
		state                  document.EmailInterpretationState
	}{
		{"compressed with eight-byte budget", "=?utf-8?B?YWJj?=", "abc", 8, document.EmailInterpretationDecoded},
		{"compressed at capacity", "=?utf-8?B?YWJj?=", "abc", 3, document.EmailInterpretationDecoded},
		{"compressed below capacity", "=?utf-8?B?YWJj?=", "", 2, document.EmailInterpretationUnsupported},
		{"unicode at capacity", "=?utf-8?B?w6k=?=", "é", 2, document.EmailInterpretationDecoded},
		{"unicode below capacity", "=?utf-8?B?w6k=?=", "", 1, document.EmailInterpretationUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := &decoder{limits: Recipe().Limits}
			d.limits.HeaderDisplayBytes = test.budget
			message := d.interpretMessage("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "subject", value: test.encoded, valid: true}}})
			field := message.Fields.Subject[0]
			assert.Equal(t, test.state, field.State)
			if test.state == document.EmailInterpretationDecoded {
				require.NotNil(t, field.Text)
				assert.Equal(t, test.decoded, *field.Text)
			}
		})
	}
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

func TestAddressProjectionBudgetUsesActualOutput(t *testing.T) {
	const value = "José <j@example.test>, A <a@example.test>"
	const projection = "José" + "j@example.test" + "A" + "a@example.test"
	exact := int64(len(value) + len(projection))
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = exact + delta
		message := d.interpretMessage("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "from", value: value, valid: true}}})
		field := message.Fields.From[0]
		if delta < 0 {
			assert.Equal(t, document.EmailInterpretationUnsupported, field.State)
			assert.Nil(t, field.Text)
			assert.Nil(t, field.Addresses)
			continue
		}
		assert.Equal(t, document.EmailInterpretationDecoded, field.State)
		require.NotNil(t, field.Addresses)
		assert.Len(t, *field.Addresses, 2)
		assert.Equal(t, exact, d.headerDisplayBytes)
	}
}

func TestFilenameBudgetUsesConvertedOutput(t *testing.T) {
	for _, test := range []struct {
		name, parameter, want string
	}{
		{"encoded word", "filename=\"=?utf-8?B?YWJj?=\"", "abc"},
		{"quoted escape", `filename="a\ b.txt"`, "a b.txt"},
		{"legacy charset", "filename*=iso-8859-1''caf%E9.txt", "café.txt"},
		{"shrinking charset", "filename*=utf-16be''%00a%00b%00c", "abc"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, delta := range []int64{-1, 0, 1} {
				d := &decoder{ctx: t.Context(), limits: Recipe().Limits}
				d.limits.HeaderDisplayBytes = int64(len(test.want)) + delta
				filename, diagnostics := d.interpretFilename("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "content-disposition", value: "attachment; " + test.parameter, valid: true}}})
				if delta < 0 {
					assert.Equal(t, document.EmailInterpretationUnsupported, filename.State)
					require.Len(t, diagnostics, 1)
					assert.Equal(t, document.EmailDiagnosticHeaderDisplayLimit, diagnostics[0].Code)
					continue
				}
				assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
				require.NotNil(t, filename.Decoded)
				assert.Equal(t, test.want, *filename.Decoded)
				assert.Equal(t, int64(len(test.want)), d.headerDisplayBytes)
			}
		})
	}
}

func TestBoundedDecodersStopAtDecisionByte(t *testing.T) {
	_, err := decodeHeaderBounded("=?utf-8?B?YWJjZA==?=", 3)
	require.ErrorIs(t, err, errDisplayBudget)
	_, _, state := decodeFilenameParameter(parseRawMIMEParameters("attachment; filename*=iso-8859-1''caf%E9.txt"), "filename", int64(len("café.txt")-1))
	assert.Equal(t, filenameDisplayLimitState, state)
	_, _, err = projectAddressesBounded("A <a@example.test>, B <b@example.test>", int64(len("A")+len("a@example.test")))
	require.ErrorIs(t, err, errDisplayBudget)
	reader := &zeroReader{remaining: 100}
	writer := &boundedStringWriter{limit: 8}
	require.ErrorIs(t, copyBounded(writer, reader), errDisplayBudget)
	assert.Equal(t, int64(9), reader.read)
}

func TestDiagnosticExhaustionKeepsCanonicalPartialEvidence(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" + strings.Repeat("bad\r\n", 4095) + "Content-Type: invalid@type\r\n\r\nbody\r\n--b--\r\n")
	result := decodePublicFixture(t, raw)
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticLimit, result.Evidence.Inventory.Termination.Code)
	require.Len(t, result.Evidence.Inventory.Parts, 2)
	require.Len(t, result.Evidence.Inventory.Parts[1].Media.Diagnostics, 1)
	assert.Equal(t, document.EmailDiagnosticInvalidHeader, result.Evidence.Inventory.Parts[1].Media.Diagnostics[0].Code)
}

type removalFailureReader struct {
	d     *decoder
	reads int
	t     *testing.T
}

type sourceCleanupFailureReader struct {
	spool *ownedSpool
	done  bool
	err   error
	t     *testing.T
}

func (r *sourceCleanupFailureReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	require.NoError(r.t, r.spool.root.Rename("source", "saved-source"))
	require.NoError(r.t, r.spool.root.Mkdir("source", 0o700))
	require.NoError(r.t, r.spool.root.WriteFile("source/child", []byte("synthetic"), 0o600))
	p[0] = 'x'
	return 1, nil
}

func (r *removalFailureReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 2 {
		require.NoError(r.t, r.d.spool.root.Rename("artifact-000001", "saved-payload"))
		require.NoError(r.t, r.d.spool.root.Mkdir("artifact-000001", 0o700))
		require.NoError(r.t, r.d.spool.root.WriteFile("artifact-000001/child", []byte("synthetic"), 0o600))
	}
	if r.reads > 2 {
		return 0, io.EOF
	}
	p[0] = 'x'
	return 1, nil
}

func TestJoinedStorageAndPolicyErrorRemainsOperational(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, parts: []document.EmailPartV1{}, messages: []document.EmailMessageV1{}, artifacts: []storedArtifact{}, messageIndex: map[string]int{}}
	d.limits.PartBytes = 1
	err = d.parseEntity("1", nil, 1, 1, "1", &removalFailureReader{d: d, t: t}, &parsedHeaderBlock{raw: []byte{}, fields: []parsedHeader{}})
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	assert.Nil(t, d.termination)
}

func TestSourceFailureKeepsJoinedCleanupErrorIdentity(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	sentinel := errors.New("synthetic source read failure")
	err = copyVerifiedSource(t.Context(), &sourceCleanupFailureReader{spool: spool, err: sentinel, t: t}, spool, "source", strings.Repeat("0", 64), 2, 8)
	require.ErrorIs(t, err, sentinel)
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
}

func TestJoinedBodyPolicyAndStorageErrorIsOperational(t *testing.T) {
	spool, err := createSpool(canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, spool.cleanup()) })
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits, spool: spool, artifacts: []storedArtifact{{}}}
	d.limits.BodyUTF8Bytes = 1
	var total int64
	_, err = d.storeStreamLimited("1", document.EmailArtifactBodyUTF8, &removalFailureReader{d: d, t: t}, 1, &total, 8, document.EmailDiagnosticBodyUTF8Limit, document.EmailDiagnosticBodyUTF8TotalLimit, document.EmailOperationCharset)
	var storage *spoolIOError
	require.ErrorAs(t, err, &storage)
	var policy *policyLimitError
	require.ErrorAs(t, err, &policy)
	assert.True(t, hasOperationalFailure(err))
}

func TestLongMultipartPreambleRetainsChildPayload(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n" + strings.Repeat("x", 5000) + "\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nbody\r\n--b--\r\n")
	result := decodePublicFixture(t, raw)
	require.Len(t, result.Evidence.Inventory.Parts, 2)
	assert.Equal(t, "1.1", result.Evidence.Inventory.Parts[1].Path)
	stream, err := result.OpenArtifact(t.Context(), "1.1", string(document.EmailArtifactDecodedPayload))
	require.NoError(t, err)
	payload, readErr := io.ReadAll(stream)
	require.NoError(t, errors.Join(readErr, stream.Close()))
	assert.Equal(t, "body", string(payload))
}

func TestRFC2231ContinuationForms(t *testing.T) {
	for _, test := range []struct{ name, parameters, want string }{
		{"plain", "filename*0=long; filename*1=name.txt", "longname.txt"},
		{"encoded", "filename*0*=utf-8''long; filename*1*=name%2Etxt", "longname.txt"},
		{"mixed", "filename*0*=utf-8''long; filename*1=name.txt", "longname.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := decodeFixture(t, "Content-Type: application/octet-stream\r\nContent-Disposition: attachment; "+test.parameters+"\r\n\r\nbody")
			filename := result.Evidence.Inventory.Parts[0].Filename
			assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
			require.NotNil(t, filename.Decoded)
			assert.Equal(t, test.want, *filename.Decoded)
		})
	}
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
