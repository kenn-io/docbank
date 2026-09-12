package emailmime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

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

func TestQuotedPairEncodedFilenameUsesDecodedCapacity(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		d := &decoder{ctx: t.Context(), limits: Recipe().Limits}
		d.limits.HeaderDisplayBytes = int64(len("abc")) + delta
		filename, diagnostics := d.interpretFilename("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "content-disposition", value: `attachment; filename="\=?utf-8?B?YWJj?="`, valid: true}}})
		if delta < 0 {
			assert.Equal(t, document.EmailInterpretationUnsupported, filename.State)
			require.Len(t, diagnostics, 1)
			assert.Equal(t, document.EmailDiagnosticHeaderDisplayLimit, diagnostics[0].Code)
			continue
		}
		assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
		require.NotNil(t, filename.Decoded)
		assert.Equal(t, "abc", *filename.Decoded)
		assert.Equal(t, int64(3), d.headerDisplayBytes)
	}
}

func TestQuotedPairEncodedFilenameAtPublicAggregateBoundary(t *testing.T) {
	const limit = 1 << 20
	raw := "Subject: " + strings.Repeat("a", limit/2) + "\r\nContent-Type: multipart/mixed; boundary=outer\r\n\r\n" +
		"--outer\r\nContent-Type: message/rfc822\r\n\r\nSubject: " + strings.Repeat("b", limit/2-3) + "\r\nContent-Type: multipart/mixed; boundary=inner\r\n\r\n" +
		"--inner\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"\\=?utf-8?B?YWJj?=\"\r\n\r\nbody\r\n--inner--\r\n--outer--\r\n"
	result := decodePublicFixture(t, []byte(raw))
	filename := result.Evidence.Inventory.Parts[len(result.Evidence.Inventory.Parts)-1].Filename
	assert.Equal(t, document.EmailInterpretationDecoded, filename.State)
	require.NotNil(t, filename.Decoded)
	assert.Equal(t, "abc", *filename.Decoded)
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

func TestFilenameDisplayLimitPrecedesUTF8Validation(t *testing.T) {
	d := &decoder{ctx: t.Context(), limits: Recipe().Limits}
	d.limits.HeaderDisplayBytes = 1
	filename, diagnostics := d.interpretFilename("1", parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "content-disposition", value: "attachment; filename*=utf-8''%E2%82%AC", valid: true}}})
	require.Equal(t, document.EmailInterpretationUnsupported, filename.State)
	require.Len(t, diagnostics, 1)
	assert.Equal(t, document.EmailDiagnosticHeaderDisplayLimit, diagnostics[0].Code)
}
