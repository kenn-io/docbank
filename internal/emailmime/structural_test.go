package emailmime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
)

func decodeFixture(t *testing.T, raw string) *Result {
	t.Helper()
	sum := sha256.Sum256([]byte(raw))
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), int64(len(raw)), bytes.NewBufferString(raw), canonicalTempDir(t))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, result.Close()) })
	return result
}

func TestDecodeSelectsHTMLFromAlternativeAndPreservesInputOrder(t *testing.T) {
	raw := "Content-Type: multipart/alternative; boundary=b\r\n\r\npreamble\r\n--b\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain\r\n--b\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<b>html</b>\r\n--b--\r\nepilogue"
	result := decodeFixture(t, raw)
	require.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
	require.Len(t, result.Evidence.Inventory.Parts, 3)
	assert.Equal(t, []string{"1", "1.1", "1.2"}, []string{result.Evidence.Inventory.Parts[0].Path, result.Evidence.Inventory.Parts[1].Path, result.Evidence.Inventory.Parts[2].Path})
	require.Equal(t, "1.2", *result.Evidence.Inventory.Messages[0].SelectedBodyPath)
	stream, err := result.OpenArtifact(t.Context(), "1.1", "decoded_payload")
	require.NoError(t, err)
	payload, readErr := io.ReadAll(stream)
	require.NoError(t, readErr)
	require.NoError(t, stream.Close())
	assert.Equal(t, "plain", string(payload))
}

func TestDecodeKeepsForwardedBodyInNestedMessage(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=m\r\n\r\n--m\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nouter\r\n--m\r\nContent-Type: message/rfc822\r\n\r\nSubject: Forwarded\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<i>inner</i>\r\n--m--\r\n"
	result := decodeFixture(t, raw)
	inventory := result.Evidence.Inventory
	require.Equal(t, document.EmailInventoryComplete, inventory.State)
	require.Len(t, inventory.Messages, 2)
	assert.Equal(t, "1.1", *inventory.Messages[0].SelectedBodyPath)
	assert.Equal(t, "1.2.1", inventory.Messages[1].Path)
	assert.Equal(t, "1.2.1", *inventory.Messages[1].SelectedBodyPath)
	assert.Equal(t, "1", inventory.Parts[2].MessagePath)
	assert.Equal(t, "1.2.1", inventory.Parts[3].MessagePath)
}

func TestDecodeRelatedGroupKeepsEqualHashDuplicateCIDCandidates(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<img src=cid:x>\r\n--r\r\nContent-ID: <x>\r\nContent-Type: image/png\r\n\r\nsame\r\n--r\r\nContent-ID: <x>\r\nContent-Disposition: inline; filename=icon.png\r\nContent-Type: image/png\r\n\r\nsame\r\n--r--\r\n"
	result := decodeFixture(t, raw)
	message := result.Evidence.Inventory.Messages[0]
	require.Len(t, message.RelatedGroups, 1)
	require.Len(t, message.RelatedGroups[0].Resources, 1)
	resource := message.RelatedGroups[0].Resources[0]
	assert.Equal(t, document.EmailResourceAmbiguous, resource.State)
	assert.Equal(t, []string{"1.2", "1.3"}, resource.Candidates)
	assert.Equal(t, result.Evidence.Inventory.Parts[2].Payload.SHA256, result.Evidence.Inventory.Parts[3].Payload.SHA256)
}

func TestDecodeRelatedGroupSelectsAvailableHTMLBeforePlain(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nplain\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<b>html</b>\r\n--r--\r\n"
	result := decodeFixture(t, raw)
	group := result.Evidence.Inventory.Messages[0].RelatedGroups[0]
	require.NotNil(t, group.BodyPath)
	assert.Equal(t, "1.2", *group.BodyPath)
}

func TestDecodeRelatedGroupInventoriesMissingInlineIDsOnce(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<body>html</body>\r\n--r\r\nContent-Type: image/png\r\n\r\none\r\n--r\r\nContent-Type: image/png; name=icon.png\r\nContent-Disposition: inline; filename=icon.png\r\n\r\ntwo\r\n--r--\r\n"
	result := decodeFixture(t, raw)
	message := result.Evidence.Inventory.Messages[0]
	require.Len(t, message.RelatedGroups, 1)
	assert.Equal(t, []document.EmailResourceV1{{CID: nil, Candidates: []string{}, State: document.EmailResourceMissing}}, message.RelatedGroups[0].Resources)
	for _, path := range []string{"1.2", "1.3"} {
		part := result.Evidence.Inventory.Parts[partIndexByPath(result, path)]
		require.Contains(t, part.Diagnostics, document.EmailDiagnosticV1{Code: document.EmailDiagnosticCIDMissing, Operation: document.EmailOperationCID, Path: &path, Detail: "inline related resource has no usable Content-ID"})
	}
}

func TestDecodeRelatedGroupScopesNestedCandidatesToNearestGroup(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=o\r\n\r\n--o\r\nContent-Type: text/html; charset=utf-8\r\n\r\nouter\r\n--o\r\nContent-Type: multipart/related; boundary=i\r\nContent-ID: <x>\r\n\r\n--i\r\nContent-Type: text/html; charset=utf-8\r\n\r\ninner\r\n--i\r\nContent-Type: image/png\r\nContent-ID: <x>\r\n\r\nsame\r\n--i--\r\n--o\r\nContent-Type: image/png\r\nContent-ID: <x>\r\n\r\nsame\r\n--o--\r\n"
	result := decodeFixture(t, raw)
	groups := result.Evidence.Inventory.Messages[0].RelatedGroups
	require.Len(t, groups, 2)
	assert.Equal(t, []string{"1", "1.2"}, []string{groups[0].RootPath, groups[1].RootPath})
	require.Len(t, groups[0].Resources, 1)
	assert.Equal(t, []string{"1.2", "1.3"}, groups[0].Resources[0].Candidates)
	require.Len(t, groups[1].Resources, 1)
	assert.Equal(t, []string{"1.2.2"}, groups[1].Resources[0].Candidates)
}

func TestDecodeRelatedGroupTreatsInvalidCIDAsMissingButExcludesAttachment(t *testing.T) {
	raw := "Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\nhtml\r\n--r\r\nContent-Type: image/png\r\nContent-ID: <>\r\n\r\ninline\r\n--r\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Disposition: attachment\r\n\r\nattachment\r\n--r--\r\n"
	result := decodeFixture(t, raw)
	group := result.Evidence.Inventory.Messages[0].RelatedGroups[0]
	assert.Equal(t, []document.EmailResourceV1{{CID: nil, Candidates: []string{}, State: document.EmailResourceMissing}}, group.Resources)
	invalid := result.Evidence.Inventory.Parts[partIndexByPath(result, "1.2")]
	assert.Equal(t, document.EmailInterpretationInvalid, invalid.ContentID.State)
	assert.Equal(t, 1, countDiagnostic(invalid, document.EmailDiagnosticCIDMissing))
	attachment := result.Evidence.Inventory.Parts[partIndexByPath(result, "1.3")]
	assert.Zero(t, countDiagnostic(attachment, document.EmailDiagnosticCIDMissing))
}

func TestCanonicalRelatedValidationRejectsTamperedScopeCIDOrderAndShape(t *testing.T) {
	baseResult := decodeFixture(t, "Content-Type: multipart/related; boundary=r\r\n\r\n--r\r\nContent-Type: text/html; charset=utf-8\r\n\r\nhtml\r\n--r\r\nContent-Type: image/png\r\nContent-ID: <x>\r\n\r\nsame\r\n--r\r\nContent-Type: image/png\r\nContent-ID: <x>\r\n\r\nsame\r\n--r--\r\n")
	encoded, _, err := document.MarshalEmailV1(baseResult.Evidence)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*document.EmailV1){
		"wrong CID part": func(value *document.EmailV1) {
			value.Inventory.Messages[0].RelatedGroups[0].Resources[0].Candidates[0] = "1.1"
		},
		"reordered": func(value *document.EmailV1) {
			candidates := value.Inventory.Messages[0].RelatedGroups[0].Resources[0].Candidates
			candidates[0], candidates[1] = candidates[1], candidates[0]
		},
		"missing entry": func(value *document.EmailV1) {
			value.Inventory.Messages[0].RelatedGroups[0].Resources = []document.EmailResourceV1{}
		},
		"extra sentinel": func(value *document.EmailV1) {
			value.Inventory.Messages[0].RelatedGroups[0].Resources = append(value.Inventory.Messages[0].RelatedGroups[0].Resources, document.EmailResourceV1{CID: nil, Candidates: []string{}, State: document.EmailResourceMissing})
		},
	} {
		t.Run(name, func(t *testing.T) {
			value, _, decodeErr := document.DecodeEmailV1(encoded)
			require.NoError(t, decodeErr)
			mutate(&value)
			_, _, marshalErr := document.MarshalEmailV1(value)
			require.Error(t, marshalErr)
		})
	}

	nested := decodeFixture(t, "Content-Type: multipart/related; boundary=o\r\n\r\n--o\r\nContent-Type: multipart/related; boundary=i\r\nContent-ID: <x>\r\n\r\n--i\r\nContent-Type: image/png\r\nContent-ID: <x>\r\n\r\nx\r\n--i--\r\n--o--\r\n")
	nested.Evidence.Inventory.Messages[0].RelatedGroups[0].Resources[0].Candidates = []string{"1.1.1"}
	_, _, err = document.MarshalEmailV1(nested.Evidence)
	require.Error(t, err)
}

func countDiagnostic(part document.EmailPartV1, code document.EmailDiagnosticCode) int {
	count := 0
	for _, diagnostic := range part.Diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}

func partIndexByPath(result *Result, path string) int {
	for index, part := range result.Evidence.Inventory.Parts {
		if part.Path == path {
			return index
		}
	}
	return -1
}

func TestDecodeClassifiesTransferFailuresWithoutPublishingPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, encoding, body string
		state                document.EmailDecodeState
		code                 document.EmailDiagnosticCode
	}{{"unknown", "x-private", "bytes", document.EmailDecodeUnsupported, document.EmailDiagnosticTransferUnsupported}, {"malformed", "base64", "YQ=!", document.EmailDecodeFailed, document.EmailDiagnosticTransferInvalid}} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "Content-Transfer-Encoding: " + tc.encoding + "\r\n\r\n" + tc.body
			result := decodeFixture(t, raw)
			part := result.Evidence.Inventory.Parts[0]
			assert.Equal(t, tc.state, part.DecodeState)
			assert.Nil(t, part.Payload)
			require.NotEmpty(t, part.Diagnostics)
			assert.Equal(t, tc.code, part.Diagnostics[len(part.Diagnostics)-1].Code)
		})
	}
}

func TestDecodeReportsUnclosedBoundaryAsPartial(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain; charset=utf-8\n\nbody"
	result := decodeFixture(t, raw)
	assert.Equal(t, document.EmailInventoryPartial, result.Evidence.Inventory.State)
	require.NotNil(t, result.Evidence.Inventory.Termination)
	assert.Equal(t, document.EmailDiagnosticBoundaryUnclosed, result.Evidence.Inventory.Termination.Code)
}

func TestDecodeMarksSignedAndEncryptedProtection(t *testing.T) {
	signed := decodeFixture(t, "Content-Type: multipart/signed; boundary=s\r\n\r\n--s\r\n\r\ndata\r\n--s--\r\n")
	assert.Equal(t, document.EmailProtectionSignedUnverified, signed.Evidence.Inventory.Parts[0].Protection)
	encrypted := decodeFixture(t, "Content-Type: application/pkcs7-mime\r\n\r\ncipher")
	assert.Equal(t, document.EmailProtectionEncrypted, encrypted.Evidence.Inventory.Parts[0].Protection)
}

func TestDecodeInterpretsEncodedHeaderAndRFC2231Filename(t *testing.T) {
	raw := "Subject: =?ISO-8859-1?Q?caf=E9?=\r\nContent-Type: application/octet-stream; name*=UTF-8''caf%C3%A9.txt\r\nContent-Disposition: attachment; filename*0*=UTF-8''caf%C3; filename*1*=%A9.txt\r\n\r\ndata"
	result := decodeFixture(t, raw)
	part := result.Evidence.Inventory.Parts[0]
	require.Equal(t, document.EmailInterpretationDecoded, part.Filename.State)
	require.NotNil(t, part.Filename.Decoded)
	assert.Equal(t, "café.txt", *part.Filename.Decoded)
	assert.Equal(t, []int{1, 2}, part.Filename.Fields)
	subject := result.Evidence.Inventory.Messages[0].Fields.Subject
	require.Len(t, subject, 1)
	assert.Equal(t, "café", *subject[0].Text)
}

func TestDecodePreservesRepeatedFilenameOccurrences(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Disposition: attachment; filename=same.txt\r\n\r\none\r\n--b\r\nContent-Disposition: attachment; filename=same.txt\r\n\r\ntwo\r\n--b--\r\n"
	result := decodeFixture(t, raw)
	parts := result.Evidence.Inventory.Parts
	require.Len(t, parts, 3)
	assert.Equal(t, parts[1].Filename.SafeName, parts[2].Filename.SafeName)
	assert.NotEqual(t, parts[1].Path, parts[2].Path)
}

func TestDecodeAcceptsEmptyPartsAndClosingBoundaryWithoutNewline(t *testing.T) {
	result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n\r\n--b--")
	require.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
	require.Len(t, result.Evidence.Inventory.Parts, 2)
	assert.Zero(t, result.Evidence.Inventory.Parts[1].Payload.Size)
}

func TestDecodeAcceptsLFBoundariesWithCRLFChildHeaders(t *testing.T) {
	result := decodeFixture(t, "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\nContent-Type: application/octet-stream\r\n\r\n"+string(bytes.Repeat([]byte{'x'}, 64<<10))+"\n--b--")
	require.Equal(t, document.EmailInventoryComplete, result.Evidence.Inventory.State)
	require.Len(t, result.Evidence.Inventory.Parts, 2)
	assert.Equal(t, int64(64<<10), result.Evidence.Inventory.Parts[1].Payload.Size)
}

func TestHeaderDisplayBudgetChargesDuplicateStoredStrings(t *testing.T) {
	d := &decoder{limits: Recipe().Limits}
	d.limits.HeaderDisplayBytes = 5
	block := parsedHeaderBlock{fields: []parsedHeader{{index: 0, name: "subject", value: "abc", valid: true}, {index: 1, name: "subject", value: "abc", valid: true}}}
	message := d.interpretMessage("1", block)
	require.Len(t, message.Fields.Subject, 2)
	assert.Equal(t, document.EmailInterpretationDecoded, message.Fields.Subject[0].State)
	assert.Equal(t, document.EmailInterpretationUnsupported, message.Fields.Subject[1].State)
}

func TestDecodeCatalogRefusalDoesNotReadOrCreateSpool(t *testing.T) {
	parent := t.TempDir()
	reader := &countingReader{}
	sum := sha256.Sum256(nil)
	result, err := Decode(t.Context(), hex.EncodeToString(sum[:]), (128<<20)+1, reader, parent)
	require.NoError(t, err)
	assert.Zero(t, reader.reads)
	assert.Equal(t, document.EmailOutcomeUnavailable, result.Evidence.Outcome)
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

type countingReader struct{ reads int }

func (r *countingReader) Read([]byte) (int, error) { r.reads++; return 0, io.EOF }
