package epub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/internal/epubutil"
	"go.kenn.io/docbank/document/internal/formatdetect"
)

type cancelAfterSpineContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelAfterSpineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelAfterSpineContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelAfterSpineContext) Value(any) any               { return nil }

func (ctx *cancelAfterSpineContext) Err() error {
	ctx.calls++
	if ctx.calls >= ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

type cancelOnSpineContext struct {
	calls    int
	cancelAt int
}

func (ctx *cancelOnSpineContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *cancelOnSpineContext) Done() <-chan struct{}       { return nil }
func (ctx *cancelOnSpineContext) Value(any) any               { return nil }

func (ctx *cancelOnSpineContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.cancelAt {
		return context.Canceled
	}
	return nil
}

type testUpload struct {
	reader   *bytes.Reader
	metadata document.AuthorizedUploadMetadata
	reads    int
	closes   int
	failure  error
}

func newTestUpload(data []byte) *testUpload {
	digest := sha256.Sum256(data)
	return &testUpload{
		reader: bytes.NewReader(data),
		metadata: document.AuthorizedUploadMetadata{
			Filename: "book.epub", MediaFamily: "ebook", MediaType: "application/epub+zip",
			ByteLength: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
			CapabilityRecordChecksum: strings.Repeat("2", 64),
			ProviderMetadataChecksum: strings.Repeat("3", 64),
			InputKind:                document.RenditionInputOriginalFile,
		},
	}
}

func (upload *testUpload) Read(buffer []byte) (int, error) {
	upload.reads++
	if upload.failure != nil {
		return 0, upload.failure
	}
	read, err := upload.reader.Read(buffer)
	if err != nil {
		return read, fmt.Errorf("read test upload: %w", err)
	}
	return read, nil
}

func (upload *testUpload) Close() error { upload.closes++; return nil }

func (upload *testUpload) Metadata() document.AuthorizedUploadMetadata { return upload.metadata }

var _ document.AuthorizedUpload = (*testUpload)(nil)
var _ io.ReadCloser = (*testUpload)(nil)

func testAuthorization(
	descriptor document.RenditionDescriptor, metadata document.AuthorizedUploadMetadata,
) document.RenditionAuthorization {
	started := time.Now().UTC().Add(-time.Minute)
	return document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64),
		SourceSHA256:                metadata.SHA256, SourceBytes: metadata.ByteLength,
		CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum,
		MediaFamily:              metadata.MediaFamily, MediaType: metadata.MediaType,
		InputKind: metadata.InputKind, MaxTotalResultBytes: 1 << 20,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}
}

func epubBytes(t *testing.T, overrides map[string]string) []byte {
	t.Helper()
	entries := map[string]string{
		"mimetype":               "application/epub+zip",
		"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/book.opf"/></rootfiles></container>`,
		"OPS/book.opf":           `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/><item id="b" href="b.xhtml" media-type="application/xhtml+xml"/><item id="unused" href="unused.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="b"/><itemref idref="a"/><itemref idref="b" linear="no"/></spine></package>`,
		"OPS/a.xhtml":            `<html xmlns="http://www.w3.org/1999/xhtml"><head><title>hidden metadata</title></head><body><p>alpha needle</p></body></html>`,
		"OPS/b.xhtml":            `<html xmlns="http://www.w3.org/1999/xhtml"><body><script/><p>beta</p></body></html>`,
		"OPS/unused.xhtml":       `<html xmlns="http://www.w3.org/1999/xhtml"><body>omitted nonspine</body></html>`,
	}
	for name, body := range overrides {
		if body == "" {
			delete(entries, name)
		} else {
			entries[name] = body
		}
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range entries {
		file, err := writer.Create(name)
		require.NoError(t, err)
		_, err = file.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func oneChapter(body string) map[string]string {
	return map[string]string{
		"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml" media-type="application/xhtml+xml"/></manifest><spine><itemref idref="a"/></spine></package>`,
		"OPS/a.xhtml":  `<html xmlns="http://www.w3.org/1999/xhtml"><body>` + body + `</body></html>`,
	}
}

func renderTest(t *testing.T, data []byte, maxUnits int64) (document.RenditionResult, error) {
	t.Helper()
	p, err := New(Profile{MaxDocumentBytes: max(1<<20, int64(len(data))), MaxUnits: maxUnits})
	require.NoError(t, err)
	upload := newTestUpload(data)
	result, err := document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	require.Equal(t, 1, upload.closes)
	return result, err
}

func requireClass(t *testing.T, result document.RenditionResult, err error, code document.RenditionErrorCode) {
	t.Helper()
	require.Error(t, err)
	classified, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, code, classified.Code())
	require.Equal(t, document.RenditionResult{}, result)
}

func TestProviderOrderedOccurrencesAndReceipt(t *testing.T) {
	data := epubBytes(t, nil)
	result, err := renderTest(t, data, 3)
	require.NoError(t, err)
	require.Equal(t, document.EvidenceComplete, result.Evidence.Completeness)
	require.Equal(t, document.EvidenceUnitSpine, result.Evidence.UnitKind)
	require.Len(t, result.Evidence.Units, 3)
	for index, want := range []struct{ name, text string }{{"OPS/b.xhtml", "beta"}, {"OPS/a.xhtml", "alpha needle"}, {"OPS/b.xhtml", "beta"}} {
		unit := result.Evidence.Units[index]
		require.Equal(t, index, unit.Order)
		require.Equal(t, want.text, unit.Text)
		require.Equal(t, document.SourceEvidenceLocatorV1{Kind: document.EvidenceLocatorSpine, IndexOrigin: document.EvidenceIndexOriginZero, Start: int64(index), End: int64(index), Name: want.name}, unit.Locator)
	}
	require.Equal(t, document.RenditionUsage{Requests: 1, InputBytes: int64(len(data)), OutputBytes: 20, Units: 3}, result.Receipt.Usage)
	require.Equal(t, newTestUpload(data).Metadata().SHA256, result.Receipt.SourceSHA256)
	require.Equal(t, providerID, result.Receipt.ProviderID)
	require.NotEmpty(t, result.Receipt.PolicyFingerprint)
	require.NotEmpty(t, result.Receipt.AuthorizationFingerprint)
	require.NotEmpty(t, result.Receipt.OperationID)
	require.Empty(t, result.ProviderMarkdown)
	require.Empty(t, result.Artifacts)
	policy, err := document.NewEvidencePolicy(10000)
	require.NoError(t, err)
	normalized, err := document.NormalizeEvidenceV1(result.Evidence, policy)
	require.NoError(t, err)
	renditionPolicy, err := document.NewRenditionPolicy(document.RenditionLimits{MaxDocumentChars: 10000, MaxUnitRunes: 10000, MaxSegmentRunes: 1000})
	require.NoError(t, err)
	rendition, err := document.BuildRenditionV1(normalized, renditionPolicy)
	require.NoError(t, err)
	require.Equal(t, "beta\n\n---\n\nalpha needle\n\n---\n\nbeta\n", string(rendition.Markdown))
	result, err = renderTest(t, data, 2)
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
}

func TestProviderNormalizesSpineMediaTypeAndReference(t *testing.T) {
	data := epubBytes(t, map[string]string{
		"OPS/book.opf": `<package xmlns="http://www.idpf.org/2007/opf"><manifest><item id="a" href="a.xhtml?view=reader#chapter-1" media-type="Application/XHTML+XML; charset=utf-8"/></manifest><spine><itemref idref="a"/></spine></package>`,
	})
	result, err := renderTest(t, data, 1)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Equal(t, "alpha needle", result.Evidence.Units[0].Text)
	require.Equal(t, "OPS/a.xhtml", result.Evidence.Units[0].Locator.Name)
}

func TestVirtualUnitsLiteralBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, text string
		want       int64
	}{
		{"empty", "", 0}, {"79", strings.Repeat("界", 79), 1}, {"80", strings.Repeat("界", 80), 1}, {"81", strings.Repeat("界", 81), 1},
		{"47 lines", strings.Repeat("a\n", 47), 1},
		{"48 lines", strings.Repeat("a\n", 48), 1}, {"49 lines", strings.Repeat("a\n", 49), 2},
		{"100 lines", strings.Repeat("a\n", 100), 3}, {"terminal", "a\n", 1}, {"no terminal", "a", 1},
		{"internal blank", strings.Repeat("a\n", 47) + "\na", 2},
		{"80 wrap boundary", strings.Repeat("界", 80*48), 1}, {"81 wrap boundary", strings.Repeat("界", 80*48+1), 2},
	} {
		t.Run(test.name, func(t *testing.T) { require.Equal(t, test.want, virtualUnits(test.text)) })
	}
}

func TestProviderExactVirtualLimitAndEmptyOccurrences(t *testing.T) {
	for _, test := range []struct {
		name, body string
		units      int64
		reject     bool
	}{
		{"exact", "<p>" + strings.Repeat("界", 3840) + "</p>", 1, false},
		{"next", "<p>" + strings.Repeat("界", 3841) + "</p>", 1, true},
		{"empty", "", 1, false},
		{"head only", "<head><title>hidden</title></head>", 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := epubBytes(t, oneChapter(test.body))
			result, err := renderTest(t, data, test.units)
			if test.reject {
				requireClass(t, result, err, document.RenditionErrorPolicyRejected)
				return
			}
			require.NoError(t, err)
			if test.name == "exact" {
				require.Equal(t, strings.Repeat("界", 3840), result.Evidence.Units[0].Text)
				require.Equal(t, int64(1), result.Receipt.Usage.Units)
			} else {
				require.Empty(t, result.Evidence.Units[0].Text)
				require.Zero(t, result.Receipt.Usage.Units)
			}
		})
	}
	override := oneChapter("<p>a</p>")
	override["OPS/book.opf"] = strings.Replace(override["OPS/book.opf"], `</manifest>`, `<item id="empty" href="empty.xhtml" media-type="application/xhtml+xml"/></manifest>`, 1)
	override["OPS/book.opf"] = strings.Replace(override["OPS/book.opf"], `</spine>`, `<itemref idref="empty"/></spine>`, 1)
	override["OPS/empty.xhtml"] = `<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`
	result, err := renderTest(t, epubBytes(t, override), 1)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 2)
	require.Empty(t, result.Evidence.Units[1].Text)
	require.Equal(t, int64(1), result.Receipt.Usage.Units)
}

func TestProviderRejectsUnsupportedModes(t *testing.T) {
	base := oneChapter("<p>needle</p>")["OPS/book.opf"]
	for _, test := range []struct {
		name    string
		changes map[string]string
	}{
		{"multiple rootfiles", map[string]string{"META-INF/container.xml": `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/book.opf"/><rootfile full-path="OPS/book.opf"/></rootfiles></container>`}},
		{"encrypted or font-obfuscated", map[string]string{"META-INF/encryption.xml": `<encryption><EncryptedData><EncryptionMethod Algorithm="http://www.idpf.org/2008/embedding"/></EncryptedData></encryption>`}},
		{"fixed layout", map[string]string{"OPS/book.opf": strings.Replace(base, "<manifest>", `<metadata><meta property="rendition:layout">pre-paginated</meta></metadata><manifest>`, 1)}},
		{"legacy fixed layout", map[string]string{"OPS/book.opf": strings.Replace(base, "<manifest>", `<metadata><meta name="fixed-layout" content="true"/></metadata><manifest>`, 1)}},
		{"item fixed layout", map[string]string{"OPS/book.opf": strings.Replace(base, `idref="a"`, `idref="a" properties="rendition:layout-pre-paginated"`, 1)}},
		{"malformed", map[string]string{"OPS/book.opf": "<package>"}},
		{"missing spine", map[string]string{"OPS/book.opf": strings.Replace(base, `<itemref idref="a"/>`, "", 1)}},
		{"duplicate IDs", map[string]string{"OPS/book.opf": strings.Replace(base, "</manifest>", `<item id="a" href="b.xhtml" media-type="application/xhtml+xml"/></manifest>`, 1)}},
		{"missing resource", map[string]string{"OPS/a.xhtml": ""}},
		{"unresolved", map[string]string{"OPS/book.opf": strings.Replace(base, `idref="a"`, `idref="missing"`, 1)}},
		{"unsafe", map[string]string{"OPS/book.opf": strings.Replace(base, `href="a.xhtml"`, `href="../../secret"`, 1)}},
		{"external", map[string]string{"OPS/book.opf": strings.Replace(base, `href="a.xhtml"`, `href="https://example.com/chapter"`, 1)}},
		{"unsupported media", map[string]string{"OPS/book.opf": strings.Replace(base, "application/xhtml+xml", "image/svg+xml", 1)}},
		{"invalid XHTML", map[string]string{"OPS/a.xhtml": "<html>"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := renderTest(t, epubBytes(t, test.changes), 100)
			requireClass(t, result, err, document.RenditionErrorUnsupportedInput)
		})
	}
	result, err := renderTest(t, []byte("not an EPUB despite the filename"), 1)
	requireClass(t, result, err, document.RenditionErrorUnsupportedInput)
}

func TestProviderXMLBaseUsesSharedResolution(t *testing.T) {
	for _, test := range []struct {
		name, base, chapter, want string
	}{
		{"root", "/", "a.xhtml", "a.xhtml"},
		{"parent root", "../", "a.xhtml", "a.xhtml"},
		{"directory", "/Text/", "Text/a.xhtml", "Text/a.xhtml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			override := oneChapter("<p>resolved</p>")
			override["OPS/book.opf"] = strings.Replace(override["OPS/book.opf"], "<manifest>", `<manifest xml:base="`+test.base+`">`, 1)
			override[test.chapter] = override["OPS/a.xhtml"]
			delete(override, "OPS/a.xhtml")
			result, err := renderTest(t, epubBytes(t, override), 1)
			require.NoError(t, err)
			require.Equal(t, test.want, result.Evidence.Units[0].Locator.Name)
		})
	}
}

func TestProviderProfileIdentityAndPreReadLimits(t *testing.T) {
	for _, profile := range []Profile{{0, 1}, {-1, 1}, {formatdetect.MaxDocumentBytes + 1, 1}, {1, 0}, {1, -1}, {1, 1_000_001}} {
		_, err := New(profile)
		require.Error(t, err)
	}
	p, err := New(Profile{formatdetect.MaxDocumentBytes, 1_000_000})
	require.NoError(t, err)
	d := p.Descriptor()
	require.Equal(t, document.RenditionTrustLocalProcess, d.TrustBoundary)
	require.Equal(t, providerID, d.ID)
	d.SupportedFormats[0].MediaType = "text/plain"
	d.ArtifactRoles[0] = document.EvidenceArtifactPDF
	require.Equal(t, "application/epub+zip", p.Descriptor().SupportedFormats[0].MediaType)
	require.Equal(t, document.EvidenceArtifactStructured, p.Descriptor().ArtifactRoles[0])
	for _, profile := range []Profile{{formatdetect.MaxDocumentBytes - 1, 1_000_000}, {formatdetect.MaxDocumentBytes, 999_999}} {
		other, err := New(profile)
		require.NoError(t, err)
		require.NotEqual(t, p.Descriptor().PolicyFingerprint, other.Descriptor().PolicyFingerprint)
		require.NotEqual(t, p.Descriptor().Fingerprint, other.Descriptor().Fingerprint)
	}
	upload := newTestUpload([]byte("x"))
	upload.metadata.ByteLength = formatdetect.MaxDocumentBytes + 1
	result, err := document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
	require.Zero(t, upload.reads)
	require.Equal(t, 1, upload.closes)
}

func TestProviderRejectsPreviousPolicyAuthorization(t *testing.T) {
	p, err := New(Profile{formatdetect.MaxDocumentBytes, 1_000_000})
	require.NoError(t, err)
	upload := newTestUpload(epubBytes(t, nil))
	authorization := testAuthorization(p.Descriptor(), upload.Metadata())
	previousVersion := strings.Replace(policyVersion, "epub/v3:", "epub/v2:", 1)
	identity := previousVersion + "\x00" + formatdetect.DetectionImplementationID + "\x00" + strconv.FormatInt(p.profile.MaxDocumentBytes, 10) + "\x00" + strconv.FormatInt(p.profile.MaxUnits, 10)
	previousPolicy := sha256.Sum256([]byte(identity))
	authorization.PolicyFingerprint = hex.EncodeToString(previousPolicy[:])
	_, err = document.RenderRendition(t.Context(), p, upload, authorization)
	require.ErrorContains(t, err, "policy fingerprint")
}

func TestAdmitSpineContextCancellation(t *testing.T) {
	data := epubBytes(t, nil)
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)
	records, err := epubutil.ReadPackages(archive.File, 1<<20)
	require.NoError(t, err)

	ctx := &cancelAfterSpineContext{cancelAt: 8}
	_, err = admitSpine(ctx, archive.File, records)
	require.ErrorIs(t, err, context.Canceled)
	require.GreaterOrEqual(t, ctx.calls, ctx.cancelAt)

	ctx2 := &cancelOnSpineContext{
		cancelAt: 2 + len(archive.File) + len(records[0].Metadata.Meta) + len(records[0].Manifest.Items),
	}
	_, err = admitSpine(ctx2, archive.File, records)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, ctx2.cancelAt, ctx2.calls)
}

func TestProviderRejectsSpineEvidenceUnitOverflow(t *testing.T) {
	emptyEntry := &zip.File{Name: "OPS/empty.xhtml"}
	entries := make([]*zip.File, maxSpineEvidenceUnits+1)
	for index := range entries {
		entries[index] = emptyEntry
	}
	require.NoError(t, rejectSpineEvidenceUnitOverflow(len(entries[:maxSpineEvidenceUnits])))
	err := rejectSpineEvidenceUnitOverflow(len(entries))
	classified, ok := errors.AsType[*document.RenditionProviderError](err)
	require.True(t, ok)
	require.Equal(t, document.RenditionErrorPolicyRejected, classified.Code())
}

func TestProviderSourceAuthorizationFailures(t *testing.T) {
	data := epubBytes(t, nil)
	p, err := New(Profile{int64(len(data)) + 1, 10})
	require.NoError(t, err)
	for _, test := range []struct {
		name   string
		change func(*testUpload)
		code   document.RenditionErrorCode
	}{
		{"wrong hash", func(u *testUpload) { u.metadata.SHA256 = strings.Repeat("a", 64) }, document.RenditionErrorPolicyRejected},
		{"short", func(u *testUpload) { u.metadata.ByteLength++ }, document.RenditionErrorPolicyRejected},
		{"long", func(u *testUpload) { u.metadata.ByteLength-- }, document.RenditionErrorPolicyRejected},
		{"transient", func(u *testUpload) { u.failure = io.ErrUnexpectedEOF }, document.RenditionErrorTransient},
	} {
		t.Run(test.name, func(t *testing.T) {
			upload := newTestUpload(data)
			test.change(upload)
			result, err := document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
			requireClass(t, result, err, test.code)
			require.Equal(t, 1, upload.closes)
		})
	}
	for _, mode := range []string{"canceled", "expired", "mismatch", "output"} {
		t.Run(mode, func(t *testing.T) {
			upload := newTestUpload(data)
			authorization := testAuthorization(p.Descriptor(), upload.Metadata())
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch mode {
			case "canceled":
				cancel()
			case "expired":
				authorization.ExpiresAt = time.Now().UTC().Add(-time.Second).Format("2006-01-02T15:04:05.000000000Z")
			case "mismatch":
				authorization.SourceSHA256 = strings.Repeat("b", 64)
			case "output":
				authorization.MaxTotalResultBytes = 1
			}
			result, err := document.RenderRendition(ctx, p, upload, authorization)
			require.Error(t, err)
			require.Equal(t, document.RenditionResult{}, result)
			require.Equal(t, 1, upload.closes)
			if mode != "output" {
				require.Zero(t, upload.reads)
			} else {
				requireClass(t, result, err, document.RenditionErrorPolicyRejected)
			}
		})
	}
}

func TestProviderInheritsZIPLimits(t *testing.T) {
	for _, mode := range []string{"single entry", "directory", "entries", "CRC"} {
		t.Run(mode, func(t *testing.T) {
			data := epubBytes(t, nil)
			central := bytes.Index(data, []byte("PK\x01\x02"))
			end := bytes.LastIndex(data, []byte("PK\x05\x06"))
			require.NotEqual(t, -1, central)
			switch mode {
			case "single entry":
				binary.LittleEndian.PutUint32(data[central+24:], 100<<20+1)
			case "directory":
				binary.LittleEndian.PutUint32(data[end+12:], 16<<20+1)
			case "entries":
				binary.LittleEndian.PutUint16(data[end+10:], 10001)
			case "CRC":
				data[central+16] ^= 1
			}
			result, err := renderTest(t, data, 10)
			requireClass(t, result, err, document.RenditionErrorUnsupportedInput)
		})
	}
}

func TestProviderConfiguredByteCapAndInputWork(t *testing.T) {
	data := epubBytes(t, nil)
	p, err := New(Profile{MaxDocumentBytes: int64(len(data)), MaxUnits: 3})
	require.NoError(t, err)
	upload := newTestUpload(data)
	_, err = document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	require.NoError(t, err)
	require.Equal(t, 1, upload.closes)
	p, err = New(Profile{MaxDocumentBytes: int64(len(data)) - 1, MaxUnits: 3})
	require.NoError(t, err)
	upload = newTestUpload(data)
	result, err := document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
	require.Zero(t, upload.reads)
	require.Equal(t, 1, upload.closes)
	data = epubBytes(t, oneChapter("<p>"+strings.Repeat("a", 10000)+"</p>"))
	p, err = New(Profile{MaxDocumentBytes: int64(len(data)), MaxUnits: 100})
	require.NoError(t, err)
	upload = newTestUpload(data)
	result, err = document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
}

func TestProviderCumulativeExpandedWorkChargesRepeatedOccurrences(t *testing.T) {
	prefix := `<html xmlns="http://www.w3.org/1999/xhtml"><head><!--`
	suffix := `--></head><body/></html>`
	override := oneChapter("")
	override["OPS/a.xhtml"] = prefix + strings.Repeat("x", (1<<20)-len(prefix)-len(suffix)) + suffix
	override["OPS/book.opf"] = strings.Replace(override["OPS/book.opf"], `<itemref idref="a"/>`, strings.Repeat(`<itemref idref="a"/>`, 501), 1)
	data := epubBytes(t, override)
	p, err := New(Profile{MaxDocumentBytes: 2 << 20, MaxUnits: 1})
	require.NoError(t, err)
	upload := newTestUpload(data)
	result, err := document.RenderRendition(t.Context(), p, upload, testAuthorization(p.Descriptor(), upload.Metadata()))
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
	require.Equal(t, 1, upload.closes)
	t.Log("501 repeated 1 MiB XHTML occurrences reject at the 500 MiB cumulative work ceiling")
}

func TestProviderHundredLinesRemainOneEvidenceUnit(t *testing.T) {
	data := epubBytes(t, oneChapter("<pre>"+strings.Repeat("a\n", 98)+"</pre>"))
	result, err := renderTest(t, data, 3)
	require.NoError(t, err)
	require.Len(t, result.Evidence.Units, 1)
	require.Equal(t, "```\n"+strings.Repeat("a\n", 98)+"```", result.Evidence.Units[0].Text)
	require.Equal(t, int64(3), result.Receipt.Usage.Units)
	result, err = renderTest(t, data, 2)
	requireClass(t, result, err, document.RenditionErrorPolicyRejected)
}

func TestProviderInheritsAggregateZIPExpansionLimit(t *testing.T) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	book := epubBytes(t, oneChapter("<p>needle</p>"))
	archive, err := zip.NewReader(bytes.NewReader(book), int64(len(book)))
	require.NoError(t, err)
	for _, file := range archive.File {
		require.NoError(t, writer.Copy(file))
	}
	block := make([]byte, 1<<20)
	for index := range 6 {
		file, err := writer.Create(fmt.Sprintf("padding-%d", index))
		require.NoError(t, err)
		for range 90 {
			_, err = file.Write(block)
			require.NoError(t, err)
		}
	}
	require.NoError(t, writer.Close())
	result, err := renderTest(t, buffer.Bytes(), 1)
	requireClass(t, result, err, document.RenditionErrorUnsupportedInput)
	t.Log("six 90 MiB ZIP entries exceed the detector's 500 MiB expanded aggregate ceiling")
}

func TestProviderCanonicalUnicodeAtExactVirtualLimit(t *testing.T) {
	result, err := renderTest(t, epubBytes(t, oneChapter("<p>"+strings.Repeat("e\u0301", 3840)+"</p>")), 1)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("é", 3840), result.Evidence.Units[0].Text)
	require.Equal(t, int64(1), result.Receipt.Usage.Units)
}
