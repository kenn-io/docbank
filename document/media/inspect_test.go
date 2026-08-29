package media_test

import (
	"archive/zip"
	"bytes"
	"compress/zlib"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
)

func TestInspectBindsFiniteTextToPolicyAndSource(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\nbeta\n")
	policy := inspectionPolicy(data, "notes.txt", "text/plain")
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible)
	assert.Equal(t, "text", record.MediaFamily)
	assert.Equal(t, "text/plain", record.MediaType)
	assert.Equal(t, int64(2), record.Measurements.TextLines)
	assert.Equal(t, int64(11), record.Measurements.Characters)
	assert.Equal(t, sha256Hex(data), record.SourceSHA256)
	assert.NotEmpty(t, record.PolicyFingerprint)
	assert.NotEmpty(t, record.Checksum)
	require.NoError(t, media.ValidateCapabilityRecord(record))

	mutated := record
	mutated.Measurements.TextLines++
	require.ErrorContains(t, media.ValidateCapabilityRecord(mutated), "checksum")

	// Decoding never restores local authority, including over a record that
	// already held it: the bytes described afterwards are the decoded ones.
	foreign := []byte("gamma\n")
	other, err := media.InspectCapability(bytes.NewReader(foreign),
		inspectionPolicy(foreign, "other.txt", "text/plain"))
	require.NoError(t, err)
	transferred, err := json.Marshal(other, json.Deterministic(true))
	require.NoError(t, err)
	overwritten := record
	require.NoError(t, json.Unmarshal(transferred, &overwritten))
	_, retained := overwritten.InspectionPolicy()
	assert.False(t, retained, "decoding must not carry local upload authority")
	assert.Equal(t, other.SourceSHA256, overwritten.SourceSHA256)

	encoded, err := json.Marshal(record, json.Deterministic(true))
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"max_text_lines":1000`)
	var decoded media.CapabilityRecord
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, policy, decoded.Policy)
	require.NoError(t, media.ValidateCapabilityRecord(decoded))
	_, local := decoded.InspectionPolicy()
	assert.False(t, local, "a portable record must not recreate local upload authority")
}

func TestInspectRejectsDeclaredSourceMismatchAndExcessiveUnits(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\nbeta\n")
	policy := inspectionPolicy(data, "notes.txt", "text/plain")
	policy.ExpectedSHA256 = strings.Repeat("0", 64)
	_, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.ErrorContains(t, err, "SHA-256")

	policy = inspectionPolicy(data, "notes.txt", "text/plain")
	policy.MaxTextLines = 1
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)

	jsonl := []byte("{\"a\":1}\n{\"b\":2}\n")
	policy = inspectionPolicy(jsonl, "records.jsonl", "application/x-ndjson")
	policy.MaxRecords = 1
	record, err = media.InspectCapability(bytes.NewReader(jsonl), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, int64(2), record.Measurements.Records)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
}

func TestInspectBoundsZIPContainers(t *testing.T) {
	t.Parallel()
	emptyZIP := zipBytes(t, nil)

	tests := []struct {
		name   string
		data   []byte
		policy func([]byte) media.InspectionPolicy
		reason media.CapabilityReason
	}{
		{
			name: "aggregate expansion",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "slides/one.xml", body: strings.Repeat("x", 32)})),
			policy: func(data []byte) media.InspectionPolicy {
				p := inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
				p.MaxExpandedBytes = 16
				return p
			},
			reason: media.CapabilityReasonExpandedBytes,
		},
		{
			name: "nested archive",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "nested.zip", body: "PK\x03\x04nested"})),
			policy: func(data []byte) media.InspectionPolicy {
				return inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
			},
			reason: media.CapabilityReasonNestedContainer,
		},
		{
			name: "empty nested archive with neutral filename",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "payload.bin", body: string(emptyZIP)})),
			policy: func(data []byte) media.InspectionPolicy {
				return inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
			},
			reason: media.CapabilityReasonNestedContainer,
		},
		{
			name: "external relationship",
			data: zipBytes(t, validPPTXEntries(zipEntry{name: "_rels/.rels", body: `<Relationships><Relationship TargetMode="External" Target="https://example.invalid"/></Relationships>`})),
			policy: func(data []byte) media.InspectionPolicy {
				return inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation")
			},
			reason: media.CapabilityReasonExternalReference,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, err := media.InspectCapability(bytes.NewReader(tt.data), tt.policy(tt.data))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, tt.reason, record.Reason)
			if tt.reason == media.CapabilityReasonNestedContainer {
				assert.Equal(t, int64(2), record.Measurements.NestingDepth)
			}
		})
	}
}

func TestInspectRejectsEncryptedZIPEntry(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, validPPTXEntries(zipEntry{name: "slides/one.xml", body: "safe"}))
	// Both the local and central directory general-purpose flags carry bit 0.
	data[6] |= 1
	central := bytes.Index(data, []byte("PK\x01\x02"))
	require.NotEqual(t, -1, central)
	data[central+8] |= 1

	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "deck.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonEncryptedContainer, record.Reason)
}

func TestInspectRejectsMalformedAndExternallyReferentialContainerXML(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		reason media.CapabilityReason
	}{
		{name: "malformed XML", body: `<worksheet><c></worksheet>`, reason: media.CapabilityReasonMalformed},
		{name: "multiple roots", body: `<worksheet/><worksheet/>`, reason: media.CapabilityReasonMalformed},
		{name: "external DTD", body: `<!DOCTYPE worksheet SYSTEM "https://example.invalid/sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
		{name: "public DTD", body: `<!DOCTYPE worksheet PUBLIC "-//EXAMPLE//DTD Sheet//EN" "sheet.dtd"><worksheet/>`, reason: media.CapabilityReasonExternalReference},
		{name: "no-namespace schema", body: `<worksheet xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:noNamespaceSchemaLocation="https://example.invalid/sheet.xsd"/>`, reason: media.CapabilityReasonExternalReference},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validXLSXEntries(zipEntry{name: "xl/worksheets/sheet1.xml", body: tt.body}))
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, tt.reason, record.Reason)
		})
	}
}

func TestInspectRejectsExternalReferenceInEPUBContent(t *testing.T) {
	t.Parallel()
	content := []string{
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/tracker.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml" xml:base="https://example.invalid/"><body><img src="tracker.png"/></body></html>`,
		`<?xml-stylesheet href="https://example.invalid/book.css"?><html xmlns="http://www.w3.org/1999/xhtml"/>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img srcset="cover.png 1x, https://example.invalid/cover.png 2x"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body style="background: url(https://example.invalid/paper.png)"/></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><head><style>@import url("https://example.invalid/book.css");</style></head><body/></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><head><style>body { background: url(https://example.invalid/paper.png) }</style></head><body/></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><video poster="https://example.invalid/poster.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><object data="https://example.invalid/embed.bin"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><embed src="https://example.invalid/embed.bin"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><div background="https://example.invalid/paper.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml" xmlns:xlink="http://www.w3.org/1999/xlink"><body><svg xmlns="http://www.w3.org/2000/svg"><use xlink:href="https://example.invalid/icons.svg#a"/></svg></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="file:///etc/passwd"/></body></html>`,
		// Hostless scheme forms still reach the network.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="http:169.254.169.254/latest/meta-data"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https:/tracker.example"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="http:/\\169.254.169.254\latest"/></body></html>`,
		// UNC is the Windows spelling of a protocol-relative reference.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="\\169.254.169.254\latest\meta-data"/></body></html>`,
		// A drive letter parses as a one-letter scheme, which no allowlist holds.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="C:\secret.txt"/></body></html>`,
		// A backslash-rooted path is the third spelling of the same idea.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="\Users\victim\secret.txt"/></body></html>`,
		// Alt text is exempt, but a MathML image reference is a real locator.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><math xmlns="http://www.w3.org/1998/Math/MathML" altimg="https://example.invalid/f.png"><mi>x</mi></math></body></html>`,
		// A consumer decodes before resolving, so each spelling also encodes.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="C%3A%5Csecret.txt"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="%5CUsers%5Cvictim%5Csecret.txt"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="%2f%2fhost%2fpath"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="\Users/victim/secret.txt"/></body></html>`,
		// A separator-normalizing consumer reads "/\host" as "//host", so both
		// mirrored spellings name a host. Only "\/" used to be caught.
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="/\host\share\x"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="%2f%5chost%5cshare"/></body></html>`,
		// A base is a locator too, and is classified by the same rule.
		`<html xmlns="http://www.w3.org/1999/xhtml" xml:base="C:/secret/"><body><img src="cover.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml" xml:base="C:\secret\"><body><img src="cover.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml" xml:base="\\host\share\"><body><img src="cover.png"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="C:/secret.txt"/></body></html>`,
		`<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="d:/secret.txt"/></body></html>`,
	}
	for _, chapter := range content {
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="chapter" href="chapter.xhtml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
			zipEntry{name: "OPS/chapter.xhtml", body: chapter},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	}
}

// Every resource a package names lives inside the container, so a reference
// that climbs past the root names a file on the host. How far is far enough
// depends on where the entry sits, so the rule counts depth rather than
// matching "..": the same value escapes from one entry and not from another.
func TestInspectRejectsArchiveEscapingReference(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, entry, reference string
		eligible               bool
	}{
		{name: "escapes from chapter", entry: "OPS/text/chapter.xhtml",
			reference: "../../../../etc/passwd"},
		{name: "escapes with backslashes", entry: "OPS/text/chapter.xhtml",
			reference: `..\..\..\..\etc\passwd`},
		// A consumer decodes the reference before resolving it.
		{name: "escapes percent-encoded", entry: "OPS/text/chapter.xhtml",
			reference: "%2e%2e/%2e%2e/%2e%2e/%2e%2e/etc/passwd"},
		{name: "escapes with encoded separators", entry: "OPS/text/chapter.xhtml",
			reference: "..%2f..%2f..%2f..%2fetc/passwd"},
		// A consumer discards a tab or newline anywhere in a reference, and
		// spaces at either end, before it resolves the rest.
		{name: "escapes across a tab", entry: "OPS/text/chapter.xhtml",
			reference: "..\t/../../../etc/passwd"},
		{name: "escapes across a newline", entry: "OPS/text/chapter.xhtml",
			reference: "../\n../../../etc/passwd"},
		// A stray percent is not an escape sequence and must not be read as one.
		{name: "unencodable text value", entry: "OPS/text/chapter.xhtml",
			reference: "100% cover", eligible: true},
		{name: "escapes from a shallower entry", entry: "OPS/chapter.xhtml",
			reference: "../../outside"},
		{name: "stays inside from a deeper entry", entry: "OPS/text/chapter.xhtml",
			reference: "../../outside", eligible: true},
		{name: "ordinary sibling reference", entry: "OPS/text/chapter.xhtml",
			reference: "../Images/cover.png", eligible: true},
		{name: "plain text attribute value", entry: "OPS/text/chapter.xhtml",
			reference: "Cover Image", eligible: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			href := strings.TrimPrefix(tt.entry, "OPS/")
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="c" href="` + href +
					`"/></manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: tt.entry, body: `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="` +
					tt.reference + `"/></body></html>`},
				zipEntry{name: "OPS/Images/cover.png", body: "png"},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
			if !tt.eligible {
				assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
			}
		})
	}

	// A base moves where later references resolve from, so containment follows
	// it. Measuring against the entry's own directory let a base spend the
	// distance and a reference cross the root after it.
	bases := []struct {
		name, base, reference string
		eligible              bool
	}{
		{name: "base spends the distance", base: "../", reference: "../../outside"},
		{name: "base spends more", base: "../../", reference: "../outside"},
		{name: "base stays inside", base: "../", reference: "../outside", eligible: true},
		// A base names a document, so only a trailing slash descends a level.
		{name: "file-like base does not descend", base: "a", reference: "../../../outside"},
		{name: "file-like base keeps its depth", base: "a", reference: "../outside", eligible: true},
		{name: "nested file-like base", base: "assets/base.xhtml", reference: "../../../../outside"},
		{name: "nested file-like base stays inside", base: "assets/base.xhtml",
			reference: "../../outside", eligible: true},
		{name: "parent file-like base", base: "../foo", reference: "../../outside"},
		// Descending by a base means more distance is needed to leave.
		{name: "base descends and stays inside", base: "images/",
			reference: "../../../outside", eligible: true},
		{name: "base descends and still leaves", base: "images/",
			reference: "../../../../outside"},
		// A consumer decodes the base too, so an encoded base moves the origin.
		{name: "encoded base spends the distance", base: "%2e%2e/",
			reference: "../../outside"},
		{name: "encoded base spends more", base: "%2e%2e/%2e%2e/",
			reference: "../outside"},
		{name: "encoded base stays inside", base: "%2e%2e/",
			reference: "../outside", eligible: true},
		// Landing on the root's own parent leaves the container too, and Clean
		// spells that one place without the trailing separator.
		{name: "reference lands on the root parent", base: "", reference: "../../.."},
		// A rooted base moves the origin to the container root. Reporting no
		// origin instead left later references measured against no container.
		{name: "rooted base measures from the root", base: "/", reference: "../secret"},
		{name: "rooted base descends", base: "/images/", reference: "../../secret"},
		{name: "rooted base stays inside", base: "/", reference: "images/cover.png",
			eligible: true},
		// A trailing dot segment is a step through the tree, not a document
		// name. Dropping it as one spent a level of the base that a reader
		// spends, so the origin stayed a directory deeper than the reader's.
		{name: "base ending in a dot segment", base: "../..", reference: "../outside"},
		{name: "base of one dot segment", base: "..", reference: "../../outside"},
		{name: "dot segment inside the base", base: "a/..", reference: "../../../outside"},
		{name: "base ending in a dot segment stays inside", base: "../..",
			reference: "outside", eligible: true},
		{name: "no base", base: "", reference: "../../outside", eligible: true},
	}
	for _, tt := range bases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			attribute := ""
			if tt.base != "" {
				attribute = ` xml:base="` + tt.base + `"`
			}
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="c" href="text/c.xhtml"/></manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: "OPS/text/c.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml"` + attribute +
					`><body><img src="` + tt.reference + `"/></body></html>`},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
		})
	}

	// A manifest href that leaves the container is caught where the package
	// document is scanned, rather than dropped as unresolvable.
	t.Run("manifest href", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="../../outside"/></manifest><spine/></package>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})
}

// A format is inspectable only if detection can confirm it. Macro-enabled
// Office formats and ODP have no detector candidate, so routing them into
// container inspection reported malformed input for a well-formed file. Say
// the family is unsupported instead of blaming the document.
func TestInspectReportsUndetectableFormatsAsUnsupported(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, validPPTXEntries())
	for _, spec := range [][2]string{
		{"deck.pptm", "application/vnd.ms-powerpoint.presentation.macroEnabled.12"},
		{"book.xlsm", "application/vnd.ms-excel.sheet.macroEnabled.12"},
		{"deck.odp", "application/vnd.oasis.opendocument.presentation"},
	} {
		t.Run(spec[0], func(t *testing.T) {
			t.Parallel()
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, spec[0], spec[1]))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonUnboundedFamily, record.Reason)
		})
	}

	// The same bytes under a detectable name stay inspectable.
	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "deck.pptx",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation"))
	require.NoError(t, err)
	assert.True(t, record.Eligible, record.Reason)
}

// A document type declaration and a stylesheet instruction are rejected for
// what they name, not for existing. Nearly every XHTML document carries a bare
// doctype, and a package-local stylesheet is an ordinary internal reference.
func TestInspectClassifiesPrologueByWhatItNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, prologue string
		eligible       bool
	}{
		{name: "bare doctype", prologue: `<!DOCTYPE html>`, eligible: true},
		{name: "system doctype", prologue: `<!DOCTYPE html SYSTEM "https://example.invalid/x.dtd">`},
		{name: "public doctype", prologue: `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.1//EN" "x.dtd">`},
		{name: "internal subset", prologue: `<!DOCTYPE html [<!ENTITY a "b">]>`},
		{name: "local stylesheet", prologue: `<?xml-stylesheet type="text/css" href="style.css"?>`, eligible: true},
		{name: "parent stylesheet", prologue: `<?xml-stylesheet href="../styles/main.css"?>`, eligible: true},
		{name: "external stylesheet", prologue: `<?xml-stylesheet href="https://example.invalid/book.css"?>`},
		{name: "escaping stylesheet", prologue: `<?xml-stylesheet href="../../../../etc/passwd"?>`},
		{name: "stylesheet without href", prologue: `<?xml-stylesheet type="text/css"?>`},
		// A consumer discards the whitespace before deciding what the value
		// names. Reading the unstripped text kept url.Parse from seeing a
		// scheme, so a leading space turned a scheme into an ordinary name.
		{name: "scheme behind a space", prologue: `<?xml-stylesheet href=" file:/etc/passwd"?>`},
		{name: "scheme behind a tab", prologue: `<?xml-stylesheet href="	https://example.invalid/x.css"?>`},
		{name: "space before a local href",
			prologue: `<?xml-stylesheet href=" style.css"?>`, eligible: true},
		// A decoy inside an earlier value must not steer the scan, and a second
		// href must not hide behind the first.
		{name: "decoy href in another value",
			prologue: `<?xml-stylesheet alt=" href='style.css'" href="https://example.invalid/x.css"?>`},
		{name: "repeated href",
			prologue: `<?xml-stylesheet href="style.css" href="https://example.invalid/x.css"?>`},
		{name: "href text inside a value is not an href",
			prologue: `<?xml-stylesheet type="href='https://example.invalid/x.css'" href="style.css"?>`,
			eligible: true},
		{name: "unparsable instruction", prologue: `<?xml-stylesheet href=style.css?>`},
		// A local href read before the syntax breaks proves nothing about what
		// follows it, so an instruction that cannot be parsed is unresolvable.
		{name: "syntax breaks after a local href",
			prologue: `<?xml-stylesheet href="style.css" alternate=yes?>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="c" href="text/c.xhtml"/></manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: "OPS/text/c.xhtml", body: tt.prologue +
					`<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`},
				zipEntry{name: "OPS/text/style.css", body: "body{}"},
				zipEntry{name: "OPS/styles/main.css", body: "body{}"},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
			if !tt.eligible {
				assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
			}
		})
	}
}

// A reader interprets an EPUB resource by its declared media type, so a
// neutral filename must not decide whether the resource is inspected.
func TestInspectFollowsEPUBDeclaredMediaTypes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, href, entry, mediaType, body string
	}{
		{
			name: "XHTML", href: "chapter.dat", entry: "chapter.dat", mediaType: "application/xhtml+xml",
			body: `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/t.png"/></body></html>`,
		},
		{
			name: "SVG", href: "art.bin", entry: "art.bin", mediaType: "image/svg+xml",
			body: `<svg xmlns="http://www.w3.org/2000/svg"><image href="https://example.invalid/i.png"/></svg>`,
		},
		{
			name: "CSS", href: "style.res", entry: "style.res", mediaType: "text/css",
			body: `body { background: url(https://example.invalid/p.png) }`,
		},
		{
			// A query or fragment names the same archive entry.
			name: "XHTML with query", href: "chapter.dat?v=1", entry: "chapter.dat",
			mediaType: "application/xhtml+xml",
			body:      `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/t.png"/></body></html>`,
		},
		{
			// A parameter does not change how a reader reads the resource.
			name: "XHTML with charset", href: "chapter.dat", entry: "chapter.dat",
			mediaType: "application/xhtml+xml; charset=utf-8",
			body:      `<html xmlns="http://www.w3.org/1999/xhtml"><body><img src="https://example.invalid/t.png"/></body></html>`,
		},
		{
			name: "CSS with charset", href: "style.res", entry: "style.res",
			mediaType: " Text/CSS ; charset=UTF-8 ",
			body:      `body { background: url(https://example.invalid/p.png) }`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="c" href="` +
					tt.href + `" media-type="` + tt.mediaType + `"/></manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: "OPS/" + tt.entry, body: tt.body},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}
}

// A consumer parses an OOXML part by the type [Content_Types].xml declares,
// and an EPUB container may declare more than one rendition.
func TestInspectFollowsDeclaredContainerParts(t *testing.T) {
	t.Parallel()

	t.Run("OOXML declared part", func(t *testing.T) {
		t.Parallel()
		contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/report.bin" ContentType="application/xml"/></Types>`
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: contentTypes},
			{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
			{name: "xl/report.bin", body: `<root><img src="https://example.invalid/t.png"/></root>`},
		})
		record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// A relationship target resolves from the part the relationship describes,
	// not from the _rels directory holding the file. Real packages point from
	// xl/_rels and xl/worksheets/_rels at siblings of their source part.
	relationships := []struct {
		name, entry, target string
		eligible            bool
	}{
		{name: "workbook rels sibling", entry: "xl/_rels/workbook.xml.rels",
			target: "worksheets/sheet1.xml", eligible: true},
		{name: "sheet rels parent", entry: "xl/worksheets/_rels/sheet1.xml.rels",
			target: "../media/image1.png", eligible: true},
		{name: "workbook rels escapes", entry: "xl/_rels/workbook.xml.rels",
			target: "../../outside"},
		{name: "sheet rels escapes", entry: "xl/worksheets/_rels/sheet1.xml.rels",
			target: "../../../outside"},
	}
	for _, tt := range relationships {
		t.Run("relationship origin "+tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validXLSXEntries(zipEntry{name: tt.entry,
				body: `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
					`<Relationship Id="r1" Target="` + tt.target + `" Type="http://example.test/t"/></Relationships>`}))
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
		})
	}

	// Readers disagree about whether xml:base applies to a manifest item, so a
	// resource is declared at every path one of them may resolve it to. The
	// declaration for an absent path is inert; a missing one would leave a
	// reachable resource unscanned.
	// A rooted base names its directory from the container root rather than
	// from the package directory, and is the spelling that used to leave the
	// item declared nowhere at all.
	manifestTargets := map[string]string{
		"../other/": "other/chapter.dat",
		"other/":    "OPS/other/chapter.dat",
		"/other/":   "other/chapter.dat",
		"/":         "chapter.dat",
	}
	for base, target := range manifestTargets {
		t.Run("manifest xml:base "+base, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package xml:base="` + base + `"><manifest>` +
					`<item id="c" href="chapter.dat" media-type="application/xhtml+xml"/>` +
					`</manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: target, body: `<html xmlns="http://www.w3.org/1999/xhtml"><body>` +
					`<img src="https://example.invalid/t.png"/></body></html>`},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// A manifest href is reduced the way a consumer reduces it before resolving.
	// Matching the unreduced text meant an item spelled with whitespace or a
	// backslash named no entry, so the resource it declares was left undeclared
	// and inspected by its file name.
	for _, href := range []string{" text/chapter.dat ", `text\chapter.dat`, "text/./chapter.dat"} {
		t.Run("manifest href "+href, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
					`<item id="c" href="` + href + `" media-type="application/xhtml+xml"/>` +
					`</manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: "OPS/text/chapter.dat", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
					`<body><img src="https://example.invalid/t.png"/></body></html>`},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// A reader may honour any prefix of the base chain, so every step it can
	// stop at is a place the item resolves to. Recording only the two ends left
	// the steps between them declared nowhere.
	for _, target := range []string{
		"OPS/chapter.dat", "OPS/a/chapter.dat", "OPS/a/b/chapter.dat", "OPS/a/b/c/chapter.dat",
	} {
		t.Run("manifest base chain "+target, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package xml:base="a/">` +
					`<manifest xml:base="b/"><item id="c" href="chapter.dat" ` +
					`media-type="application/xhtml+xml" xml:base="c/"/>` +
					`</manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: target, body: `<html xmlns="http://www.w3.org/1999/xhtml"><body>` +
					`<img src="https://example.invalid/t.png"/></body></html>`},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// The package directory stays in use, so an item without any base still
	// resolves the way it always did.
	t.Run("manifest without xml:base", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="chapter.dat" media-type="application/xhtml+xml"/>` +
				`</manifest><spine><itemref idref="c"/></spine></package>`},
			zipEntry{name: "OPS/chapter.dat", body: `<html xmlns="http://www.w3.org/1999/xhtml"><body>` +
				`<img src="https://example.invalid/t.png"/></body></html>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// An absolute manifest href names the container root. Joining it with the
	// package directory pointed the declaration at a different entry and left
	// the real one undeclared and unscanned.
	t.Run("absolute manifest href", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="/chapter.dat" media-type="application/xhtml+xml"/>` +
				`</manifest><spine><itemref idref="c"/></spine></package>`},
			zipEntry{name: "chapter.dat", body: `<html xmlns="http://www.w3.org/1999/xhtml"><body>` +
				`<img src="https://example.invalid/t.png"/></body></html>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// A part name is a URI path, so a consumer resolves its escapes and its dot
	// segments before matching. A spelling that matched no entry left the part
	// undeclared and inspected only by its filename.
	for _, partName := range []string{"/xl/re%70ort.bin", "/xl/./report.bin", "/xl/media/../report.bin",
		// The separator marking the package root may itself be encoded.
		"%2Fxl%2Freport.bin", "%2fxl/report.bin"} {
		t.Run("OOXML part name "+partName, func(t *testing.T) {
			t.Parallel()
			contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
				`<Override PartName="` + partName + `" ContentType="application/xml"/></Types>`
			data := zipBytes(t, []zipEntry{
				{name: "[Content_Types].xml", body: contentTypes},
				{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
				{name: "xl/report.bin", body: `<root><img src="https://example.invalid/t.png"/></root>`},
			})
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// A part named by two Overrides is declared twice. The package is invalid
	// either way and consumers need not agree on which one wins, so the part is
	// inspected under both: keeping only the last let a second declaration of
	// "opaque" hide a part that a consumer taking the first reads as markup.
	for _, order := range [][2]string{
		{"application/xml", "application/octet-stream"},
		{"application/octet-stream", "application/xml"},
	} {
		t.Run("conflicting OOXML declarations "+order[0], func(t *testing.T) {
			t.Parallel()
			contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
				`<Override PartName="/xl/report.bin" ContentType="` + order[0] + `"/>` +
				`<Override PartName="/xl/report.bin" ContentType="` + order[1] + `"/></Types>`
			data := zipBytes(t, []zipEntry{
				{name: "[Content_Types].xml", body: contentTypes},
				{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
				{name: "xl/report.bin", body: `<root><img src="https://example.invalid/t.png"/></root>`},
			})
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// Two Defaults may claim one extension, the same way two Overrides may claim
	// one part, and with the same consequence if only the last is kept.
	for _, order := range [][2]string{
		{"application/xml", "image/png"},
		{"image/png", "application/xml"},
	} {
		t.Run("conflicting OOXML defaults "+order[0], func(t *testing.T) {
			t.Parallel()
			contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Default Extension="bin" ContentType="` + order[0] + `"/>` +
				`<Default Extension="bin" ContentType="` + order[1] + `"/>` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`
			data := zipBytes(t, []zipEntry{
				{name: "[Content_Types].xml", body: contentTypes},
				{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
				{name: "xl/report.bin", body: `<root><img src="https://example.invalid/t.png"/></root>`},
			})
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// An Override still replaces the Default for its part, so a part its
	// extension declares as markup is not inspected when the Override says
	// otherwise and nothing else declares it.
	t.Run("Override replaces the Default", func(t *testing.T) {
		t.Parallel()
		contentTypes := `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
			`<Default Extension="bin" ContentType="application/xml"/>` +
			`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
			`<Override PartName="/xl/report.bin" ContentType="image/png"/></Types>`
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: contentTypes},
			{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
			{name: "xl/report.bin", body: `<root><img src="https://example.invalid/t.png"/></root>`},
		})
		record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
		require.NoError(t, err)
		assert.True(t, record.Eligible, record.Reason)
	})

	t.Run("second EPUB rootfile", func(t *testing.T) {
		t.Parallel()
		container := `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles>` +
			`<rootfile full-path="OPS/content.opf"/><rootfile full-path="OPS/alt.dat"/></rootfiles></container>`
		data := zipBytes(t, []zipEntry{
			{name: "mimetype", body: "application/epub+zip"},
			{name: "META-INF/container.xml", body: container},
			{name: "OPS/content.opf", body: `<package><manifest><item id="a" href="a.xhtml"/></manifest><spine><itemref idref="a"/></spine></package>`},
			{name: "OPS/a.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml"><body/></html>`},
			{name: "OPS/alt.dat", body: `<package><manifest><item id="b" href="https://example.invalid/b.xhtml"/></manifest><spine><itemref idref="b"/></spine></package>`},
		})
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// Renditions can declare the same entry differently, and each declaration
	// decides how some reader reads those bytes. Inspection has to satisfy every
	// one of them, whatever order the container lists them in.
	conflicts := [][2]string{
		{"application/xhtml+xml", "image/png"},
		{"image/png", "application/xhtml+xml"},
		{"text/css", "application/xhtml+xml"},
		{"application/xhtml+xml", "text/css"},
	}
	for _, declarations := range conflicts {
		t.Run("conflicting EPUB declarations "+declarations[0]+" then "+declarations[1], func(t *testing.T) {
			t.Parallel()
			container := `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles>` +
				`<rootfile full-path="OPS/content.opf"/><rootfile full-path="OPS/alt.opf"/></rootfiles></container>`
			manifest := func(mediaType string) string {
				return `<package><manifest><item id="c" href="chapter.dat" media-type="` + mediaType +
					`"/></manifest><spine><itemref idref="c"/></spine></package>`
			}
			data := zipBytes(t, []zipEntry{
				{name: "mimetype", body: "application/epub+zip"},
				{name: "META-INF/container.xml", body: container},
				{name: "OPS/content.opf", body: manifest(declarations[0])},
				{name: "OPS/alt.opf", body: manifest(declarations[1])},
				{name: "OPS/chapter.dat", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
					`<body><img src="https://example.invalid/t.png"/></body></html>`},
			})
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}
}

// ODF states each part's type in META-INF/manifest.xml rather than in
// [Content_Types].xml. Reading only the OOXML and EPUB declarations left an ODF
// part classified by its file name, so markup under a neutral name went unread.
func TestInspectFollowsODFManifestDeclarations(t *testing.T) {
	t.Parallel()
	const drawing = `<svg xmlns="http://www.w3.org/2000/svg">` +
		`<image href="https://example.invalid/t.png"/></svg>`
	odf := func(entryName, declaredPath, declaredType, body string) []zipEntry {
		declaration := ""
		if declaredPath != "" {
			declaration = `<manifest:file-entry manifest:full-path="` + declaredPath +
				`" manifest:media-type="` + declaredType + `"/>`
		}
		return []zipEntry{
			{name: "mimetype", body: "application/vnd.oasis.opendocument.spreadsheet"},
			{name: "META-INF/manifest.xml", body: `<manifest:manifest ` +
				`xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0">` +
				declaration + `</manifest:manifest>`},
			{name: "content.xml", body: `<office:document-content ` +
				`xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0">` +
				`<office:body><office:spreadsheet/></office:body></office:document-content>`},
			{name: entryName, body: body},
		}
	}
	cases := []struct {
		name, entry, declaredPath, declaredType, body string
		eligible                                      bool
	}{
		{name: "neutral name declared as markup", entry: "Pictures/image.dat",
			declaredPath: "Pictures/image.dat", declaredType: "image/svg+xml", body: drawing},
		{name: "encoded declaration names the part", entry: "Pictures/a b.dat",
			declaredPath: "Pictures/a%20b.dat", declaredType: "image/svg+xml", body: drawing},
		// A part name is a URI path, so a consumer resolves its dot segments
		// before matching. A spelling that matched no entry left the part
		// undeclared and inspected only by its filename.
		{name: "dot segment inside the declaration", entry: "Pictures/image.dat",
			declaredPath: "Pictures/./image.dat", declaredType: "image/svg+xml", body: drawing},
		{name: "declaration rooted by a dot segment", entry: "Pictures/image.dat",
			declaredPath: "./Pictures/image.dat", declaredType: "image/svg+xml", body: drawing},
		// Nothing declares it as markup, so its name decides, as it always did.
		{name: "undeclared neutral name", entry: "Pictures/image.dat",
			body: drawing, eligible: true},
		// A declaration that is not markup must not pull an entry into markup
		// inspection, or every stored image would be parsed as XML.
		{name: "declared as an image", entry: "Pictures/image.png",
			declaredPath: "Pictures/image.png", declaredType: "image/png",
			body: "\x89PNG\r\n\x1a\n", eligible: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, odf(tt.entry, tt.declaredPath, tt.declaredType, tt.body))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "sheet.ods",
					"application/vnd.oasis.opendocument.spreadsheet"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
		})
	}
}

// A part counts toward its semantic limit because of what it is, so a
// worksheet or slide parked outside the conventional path still counts.
func TestInspectCountsDeclaredOOXMLParts(t *testing.T) {
	t.Parallel()
	const worksheetType = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"
	const slideType = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"

	t.Run("worksheet", func(t *testing.T) {
		t.Parallel()
		sheet := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
			`<c/><c/><c/><c/><c/></worksheet>`
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
				`<Override PartName="/parts/data.bin" ContentType="` + worksheetType + `"/></Types>`},
			{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
			{name: "parts/data.bin", body: sheet},
		})
		policy := inspectionPolicy(data, "book.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		policy.MaxCells = 4
		record, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.NoError(t, err)
		assert.Equal(t, int64(1), record.Measurements.Sheets)
		assert.Equal(t, int64(5), record.Measurements.Cells)
		assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
	})

	// A declaration settles the type in both directions, so a part sitting under
	// a conventional path is not a worksheet when the package says otherwise.
	// Excel writes per-sheet relationship parts under that same prefix, and
	// counting those as sheets overstates a genuine workbook.
	t.Run("declaration outranks the path", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
				`<Override PartName="/xl/worksheets/sheet1.xml" ContentType="` + worksheetType + `"/>` +
				`<Override PartName="/xl/worksheets/notes.xml" ContentType="application/vnd.openxmlformats-officedocument.oleObject"/></Types>`},
			{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
			{name: "xl/worksheets/sheet1.xml", body: `<worksheet><c/></worksheet>`},
			{name: "xl/worksheets/notes.xml", body: `<notes/>`},
			{name: "xl/worksheets/_rels/sheet1.xml.rels", body: `<Relationships/>`},
		})
		record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(1), record.Measurements.Sheets)
	})

	// Unlike EPUB renditions, which each state a valid reading, an OOXML
	// Override states the single content type for that part and replaces the
	// Default its extension would otherwise carry.
	t.Run("override replaces default", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>` +
				`<Default Extension="bin" ContentType="` + worksheetType + `"/>` +
				`<Override PartName="/parts/data.bin" ContentType="application/vnd.openxmlformats-officedocument.oleObject"/></Types>`},
			{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
			{name: "parts/data.bin", body: "opaque"},
		})
		record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "book.xlsx",
			"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(0), record.Measurements.Sheets)
	})

	t.Run("slide", func(t *testing.T) {
		t.Parallel()
		slide := `<sld xmlns="http://schemas.openxmlformats.org/presentationml/2006/main"/>`
		data := zipBytes(t, []zipEntry{
			{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">` +
				`<Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/>` +
				`<Override PartName="/parts/one.bin" ContentType="` + slideType + `"/>` +
				`<Override PartName="/parts/two.bin" ContentType="` + slideType + `"/></Types>`},
			{name: "ppt/presentation.xml", body: `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`},
			{name: "parts/one.bin", body: slide},
			{name: "parts/two.bin", body: slide},
		})
		policy := inspectionPolicy(data, "deck.pptx",
			"application/vnd.openxmlformats-officedocument.presentationml.presentation")
		policy.MaxSlides = 1
		record, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.NoError(t, err)
		assert.Equal(t, int64(2), record.Measurements.Slides)
		assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
	})
}

// Treating every unlisted attribute as a locator must not reject the
// vocabulary URIs and colon-bearing values that ordinary documents carry.
func TestInspectAcceptsVocabularyAttributeValues(t *testing.T) {
	t.Parallel()
	sheet := `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">` +
		`<dimension ref="A1:C3"/><c/></worksheet>`
	// A one-letter prefix is only a drive when a path separator follows it, and
	// a relationship target may name an absolute part inside the package.
	relationships := `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">` +
		`<Relationship Id="rId1" Target="worksheets/sheet1.xml" ` +
		`Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"/>` +
		`<Relationship Id="rId2" Target="/xl/styles.xml" Type="http://example.test/styles" name="c:notes"/>` +
		`</Relationships>`
	data := zipBytes(t, validXLSXEntries(
		zipEntry{name: "xl/worksheets/sheet1.xml", body: sheet},
		zipEntry{name: "xl/_rels/workbook.xml.rels", body: relationships},
	))
	record, err := media.InspectCapability(bytes.NewReader(data),
		inspectionPolicy(data, "book.xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)

	chapter := `<html xmlns="http://www.w3.org/1999/xhtml" xml:base="../Images/"><head>` +
		`<meta property="dcterms:modified" content="2026-01-01T00:00:00Z"/>` +
		`<meta property="formula" content="\frac{1}{2}"/>` +
		`<meta property="coverage" content="100% cover"/>` +
		// MathML alt text is commonly TeX, which is full of backslashes.
		`<math xmlns="http://www.w3.org/1998/Math/MathML" alttext="\left(\frac{a}{b}\right)"><mi>x</mi></math>` +
		`<style>body { background: url(../Images/cover.png) }</style></head>` +
		`<body><p style="opacity: 0%">text</p></body></html>`
	data = zipBytes(t, validEPUBEntries(
		zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="chapter" href="chapter.xhtml"/></manifest><spine><itemref idref="chapter"/></spine></package>`},
		zipEntry{name: "OPS/chapter.xhtml", body: chapter},
		zipEntry{name: "OPS/Images/cover.png", body: "png"},
	))
	record, err = media.InspectCapability(bytes.NewReader(data),
		inspectionPolicy(data, "book.epub", "application/epub+zip"))
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
}

func TestInspectResolvesEPUBCSSReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, stylesheet string
		eligible         bool
	}{
		{name: "relative asset", stylesheet: `body { background: url(../Images/cover.png) }`, eligible: true},
		{name: "escaped relative asset", stylesheet: `body { background: u\72l( "../Images/cover.png" ) }`, eligible: true},
		{name: "relative import", stylesheet: `@import "theme.css";`, eligible: true},
		{name: "URL text in string", stylesheet: `a::after { content: "url" }`, eligible: true},
		{name: "escaped external URL", stylesheet: `body { background: u\72l(https://example.invalid/cover.png) }`},
		{name: "escaped external import", stylesheet: `@im\70ort "https://example.invalid/theme.css";`},
		{name: "missing package asset", stylesheet: `body { background: url(../Images/missing.png) }`},
		{name: "protocol-relative URL", stylesheet: `body { background: url("//host.invalid/x") }`},
		// A leading space hid the "//" and then became part of a directory
		// name, so with an entry crafted to carry that name the reference named
		// an archive entry here and a host to a renderer.
		{name: "protocol-relative URL behind a space",
			stylesheet: `body { background: url(" //host.invalid/x") }`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="css" href="Styles/book.css"/><item id="cover" href="Images/cover.png"/></manifest><spine/></package>`},
				zipEntry{name: "OPS/Styles/book.css", body: tt.stylesheet},
				zipEntry{name: "OPS/Styles/theme.css", body: `body { color: black }`},
				zipEntry{name: "OPS/Images/cover.png", body: "synthetic image"},
				// The entry the crafted reference resolves to when the space is
				// measured rather than discarded.
				zipEntry{name: "OPS/Styles/ /host.invalid/x", body: "decoy"},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible)
			if !tt.eligible {
				assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
			}
		})
	}
}

func TestInspectCountsODSRepeatedCells(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, []zipEntry{
		{name: "mimetype", body: "application/vnd.oasis.opendocument.spreadsheet"},
		{name: "META-INF/manifest.xml", body: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"/>`},
		{name: "content.xml", body: `<office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0"><office:body><office:spreadsheet><table:table><table:table-column table:number-columns-repeated="3"/><table:table-row table:number-rows-repeated="500"><table:table-cell table:number-columns-repeated="200"/></table:table-row></table:table></office:spreadsheet></office:body></office:document-content>`},
	})
	policy := inspectionPolicy(data, "book.ods", "application/vnd.oasis.opendocument.spreadsheet")
	policy.MaxCells = 100_000
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(100_000), record.Measurements.Cells)

	policy.MaxCells--
	record, err = media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
}

// An XML attribute name is case-sensitive and belongs to a namespace, so one
// that differs in either states nothing about the repeat. Matching the local
// name alone let a foreign attribute stand in for the real one and hide the
// cells it stands for from the limit.
func TestInspectCountsODSRepeatsByTheTableAttribute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, attributes string
		cells            int64
	}{
		{name: "table attribute", attributes: `table:number-rows-repeated="20000"`, cells: 4_000_000},
		{name: "unqualified attribute", attributes: `number-rows-repeated="20000"`, cells: 4_000_000},
		// A foreign attribute placed first used to be read instead of the real
		// one, which is the whole of the bypass.
		{name: "foreign attribute first",
			attributes: `x:number-rows-repeated="1" table:number-rows-repeated="20000"`,
			cells:      4_000_000},
		{name: "foreign attribute alone", attributes: `x:number-rows-repeated="20000"`, cells: 200},
		{name: "wrong case", attributes: `table:NUMBER-ROWS-REPEATED="20000"`, cells: 200},
		// The document is invalid and a consumer need not agree which repeat it
		// takes, so the largest is counted rather than whichever came last.
		{name: "repeated attribute, smaller last",
			attributes: `table:number-rows-repeated="20000" table:number-rows-repeated="1"`,
			cells:      4_000_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			content := `<office:document-content ` +
				`xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" ` +
				`xmlns:table="urn:oasis:names:tc:opendocument:xmlns:table:1.0" ` +
				`xmlns:x="http://example.test/x"><office:body><office:spreadsheet>` +
				`<table:table><table:table-row ` + tt.attributes + `>` +
				`<table:table-cell table:number-columns-repeated="200"/>` +
				`</table:table-row></table:table></office:spreadsheet></office:body>` +
				`</office:document-content>`
			data := zipBytes(t, []zipEntry{
				{name: "mimetype", body: "application/vnd.oasis.opendocument.spreadsheet"},
				{name: "META-INF/manifest.xml", body: `<manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0"/>`},
				{name: "content.xml", body: content},
			})
			policy := inspectionPolicy(data, "book.ods",
				"application/vnd.oasis.opendocument.spreadsheet")
			policy.MaxCells = 1_000_000
			record, err := media.InspectCapability(bytes.NewReader(data), policy)
			require.NoError(t, err)
			assert.Equal(t, tt.cells, record.Measurements.Cells)
			assert.Equal(t, tt.cells <= policy.MaxCells, record.Eligible, record.Reason)
		})
	}
}

func TestInspectUsesEPUBContainerPackagePath(t *testing.T) {
	t.Parallel()
	data := zipBytes(t, []zipEntry{
		{name: "mimetype", body: "application/epub+zip"},
		{name: "META-INF/container.xml", body: `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container" version="1.0"><rootfiles><rootfile full-path="OPS/package.bin" media-type="application/oebps-package+xml"/></rootfiles></container>`},
		{name: "OPS/package.bin", body: `<package><manifest><item id="a" href="a.xhtml"/><item id="b" href="b.xhtml"/></manifest><spine><itemref idref="a"/><itemref idref="b"/></spine></package>`},
	})
	policy := inspectionPolicy(data, "book.epub", "application/epub+zip")
	policy.MaxResources = 1
	policy.MaxSpineItems = 1
	record, err := media.InspectCapability(bytes.NewReader(data), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
	assert.Equal(t, int64(2), record.Measurements.Resources)
	assert.Equal(t, int64(2), record.Measurements.SpineItems)
}

// Unlike xml:base, an HTML <base> sets the base URI of the whole document, so a
// renderer resolves every reference against it. Containment measured from the
// entry's own directory let a climbing base buy distance for every reference
// after it.
func TestInspectMeasuresContainmentFromTheDocumentBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, base, reference string
		eligible              bool
	}{
		{name: "base climbs and reference spends it", base: "../../", reference: "../secret"},
		{name: "base climbs and reference spends more", base: "../../", reference: "../../secret"},
		// The base moves where the document resolves from, not how far a
		// reference may go once it is there.
		{name: "base climbs and reference stays inside", base: "../../",
			reference: "cover.png", eligible: true},
		{name: "base climbs one level", base: "../", reference: "cover.png", eligible: true},
		// A base descends, so a reference has further to climb before it leaves.
		{name: "base descends", base: "assets/", reference: "../../../secret", eligible: true},
		// The base is a locator itself, and resolves from where the document
		// sits rather than from its own answer.
		{name: "base leaves the container", base: "../../../../",
			reference: "cover.png"},
		{name: "remote base", base: "https://example.invalid/", reference: "cover.png"},
		// A consumer discards the whitespace before resolving, so a base
		// spelled with it climbs just as far as one spelled without.
		{name: "base wrapped in spaces", base: " ../../ ", reference: "../secret"},
		{name: "base split by a tab", base: "..\t/../", reference: "../secret"},
		{name: "base wrapped in spaces stays inside", base: " ../../ ",
			reference: "cover.png", eligible: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, validEPUBEntries(
				zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
					`<item id="c" href="text/c.xhtml" media-type="application/xhtml+xml"/>` +
					`</manifest><spine><itemref idref="c"/></spine></package>`},
				zipEntry{name: "OPS/text/c.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
					`<head><base href="` + tt.base + `"/></head>` +
					`<body><img src="` + tt.reference + `"/></body></html>`},
			))
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "book.epub", "application/epub+zip"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
		})
	}

	// The base's own href resolves from where the document sits. That exemption
	// covers the href alone: markup nested inside the element resolves from the
	// base like the rest of the document.
	t.Run("reference nested inside the base", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="text/c.xhtml" media-type="application/xhtml+xml"/>` +
				`</manifest><spine><itemref idref="c"/></spine></package>`},
			zipEntry{name: "OPS/text/c.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
				`<head><base href="../../"><img src="../secret"/></base></head>` +
				`<body/></html>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// Only the unqualified href states the document base. A renderer ignores a
	// namespaced one, so honouring it would move the origin somewhere the
	// renderer never resolves from, and a base that descends there would leave
	// a reference measured with more room than it has.
	t.Run("namespaced href does not state the base", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="text/c.xhtml" media-type="application/xhtml+xml"/>` +
				`</manifest><spine><itemref idref="c"/></spine></package>`},
			zipEntry{name: "OPS/text/c.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
				`<head><base xmlns:x="http://example.test/x" x:href="assets/"/></head>` +
				`<body><img src="../../../secret"/></body></html>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})

	// A <base> applies to the whole document, including references written
	// before it, so reading it only when it is reached would leave those
	// measured against the directory the renderer no longer uses.
	t.Run("reference before the base", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validEPUBEntries(
			zipEntry{name: "OPS/content.opf", body: `<package><manifest>` +
				`<item id="c" href="text/c.xhtml" media-type="application/xhtml+xml"/>` +
				`</manifest><spine><itemref idref="c"/></spine></package>`},
			zipEntry{name: "OPS/text/c.xhtml", body: `<html xmlns="http://www.w3.org/1999/xhtml">` +
				`<head><link href="../secret"/><base href="../../"/></head>` +
				`<body/></html>`},
		))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.epub", "application/epub+zip"))
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
	})
}

// A standalone document has no container, so a rooted path names no part of one
// and pins a location on the host instead, the way "C:\secret.txt" does. Inside
// a container the same spelling names the container root and stays local.
func TestInspectClassifiesRootedPathsByContainer(t *testing.T) {
	t.Parallel()
	standalone := []struct {
		reference string
		eligible  bool
	}{
		{reference: "/etc/passwd"},
		{reference: "/images/logo.png"},
		{reference: `\\host\share`},
		// A relative reference lands wherever the consumer put the document,
		// so it pins nothing and names no place.
		{reference: "cover.png", eligible: true},
		{reference: "assets/cover.png", eligible: true},
	}
	for _, tt := range standalone {
		t.Run("standalone "+tt.reference, func(t *testing.T) {
			t.Parallel()
			data := []byte(`<doc><image href="` + tt.reference + `"/></doc>`)
			record, err := media.InspectCapability(bytes.NewReader(data),
				inspectionPolicy(data, "doc.xml", "application/xml"))
			require.NoError(t, err)
			assert.Equal(t, tt.eligible, record.Eligible, record.Reason)
		})
	}

	// The container root is how a legitimate OPC part name is spelled, so the
	// same reference stays local when there is a container to name.
	t.Run("rooted part name inside a container", func(t *testing.T) {
		t.Parallel()
		data := zipBytes(t, validXLSXEntries(zipEntry{name: "xl/worksheets/sheet1.xml",
			body: `<worksheet><drawing r:href="/xl/styles.xml"/></worksheet>`}))
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "book.xlsx",
				"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"))
		require.NoError(t, err)
		assert.True(t, record.Eligible, record.Reason)
	})
}

func TestInspectCountsFinitePresentationSpreadsheetAndEPUBUnits(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		filename   string
		mediaType  string
		entries    []zipEntry
		family     string
		assertions func(*testing.T, media.CapabilityRecord)
	}{
		{
			name: "slides", filename: "deck.pptx", mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			entries: validPPTXEntries(
				zipEntry{name: "ppt/slides/slide1.xml", body: "<slide/>"},
				zipEntry{name: "ppt/slides/slide2.xml", body: "<slide/>"},
			), family: "presentation",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(2), r.Measurements.Slides)
			},
		},
		{
			name: "sheets and cells", filename: "book.xlsx", mediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
			entries: validXLSXEntries(zipEntry{name: "xl/worksheets/sheet1.xml", body: `<worksheet><c/><c/></worksheet>`}), family: "spreadsheet",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(1), r.Measurements.Sheets)
				assert.Equal(t, int64(2), r.Measurements.Cells)
			},
		},
		{
			name: "spine and resources", filename: "book.epub", mediaType: "application/epub+zip",
			entries: validEPUBEntries(zipEntry{name: "OPS/content.opf", body: `<package><manifest><item id="a" href="a.xhtml"/><item id="b" href="b.png"/></manifest><spine><itemref idref="a"/></spine></package>`}), family: "ebook",
			assertions: func(t *testing.T, r media.CapabilityRecord) {
				t.Helper()
				assert.Equal(t, int64(1), r.Measurements.SpineItems)
				assert.Equal(t, int64(2), r.Measurements.Resources)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := zipBytes(t, tt.entries)
			record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, tt.filename, tt.mediaType))
			require.NoError(t, err)
			require.True(t, record.Eligible, record.Reason)
			assert.Equal(t, tt.family, record.MediaFamily)
			tt.assertions(t, record)
		})
	}
}

func TestInspectRejectsDeceptiveDeclaredFormatAndFilename(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDF("deceptive")
	tests := []struct {
		name, filename, mediaType string
		data                      []byte
	}{
		{name: "PDF with text filename", filename: "report.txt", mediaType: "application/pdf", data: pdf},
		{name: "text declared as PDF", filename: "report.pdf", mediaType: "application/pdf", data: []byte("not a PDF")},
		{name: "OOXML declared generic ZIP", filename: "deck.pptx", mediaType: "application/zip", data: zipBytes(t, validPPTXEntries())},
		{name: "PPTX without main markers", filename: "deck.pptx", mediaType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", data: zipBytes(t, []zipEntry{{name: "ppt/slides/slide1.xml", body: "<slide/>"}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			record, err := media.InspectCapability(bytes.NewReader(tt.data), inspectionPolicy(tt.data, tt.filename, tt.mediaType))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectBindsVisualAndStandaloneXMLIdentity(t *testing.T) {
	t.Parallel()
	png := mediatest.PNG(4, 3, nil)
	for _, filename := range []string{"image.txt", "image.jpeg"} {
		policy := inspectionPolicy(png, filename, "image/png")
		policy.MaxPixels, policy.MaxFrames = 100, 2
		record, err := media.InspectCapability(bytes.NewReader(png), policy)
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
	}
	policy := inspectionPolicy(png, "image.png", "image/png")
	policy.MaxPixels, policy.MaxFrames = 100, 2
	record, err := media.InspectCapability(bytes.NewReader(png), policy)
	require.NoError(t, err)
	assert.True(t, record.Eligible)

	xml := []byte(`<!DOCTYPE root SYSTEM "file:///tmp/private.dtd"><root/>`)
	record, err = media.InspectCapability(bytes.NewReader(xml), inspectionPolicy(xml, "record.xml", "application/xml"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
}

func TestInspectRejectsUnregisteredFamily(t *testing.T) {
	t.Parallel()
	data := []byte("legacy binary document")
	record, err := media.InspectCapability(bytes.NewReader(data), inspectionPolicy(data, "report.doc", "application/msword"))
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonUnboundedFamily, record.Reason)
}

func TestInspectBoundsPDFPagesAndWAVDuration(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDFPages(2, "bounds")
	pdfPolicy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	pdfPolicy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), pdfPolicy)
	require.NoError(t, err)
	assert.Equal(t, int64(2), record.Measurements.Pages)
	assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)

	wav := wavBytes(8_000, 8_000)
	wavPolicy := inspectionPolicy(wav, "sample.wav", "audio/wav")
	wavPolicy.MaxDurationMS = 500
	record, err = media.InspectCapability(bytes.NewReader(wav), wavPolicy)
	require.NoError(t, err)
	assert.Equal(t, int64(1_000), record.Measurements.DurationMS)
	assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
}

func TestInspectResolvesAndBoundsAuthoritativePDFObjects(t *testing.T) {
	t.Parallel()
	t.Run("xref ignores unreferenced duplicate", func(t *testing.T) {
		pdf := syntheticPDFObjects("xref-authority", []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		}, "2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
		policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
		policy.MaxPages = 1
		record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
		require.NoError(t, err)
		assert.Equal(t, int64(2), record.Measurements.Pages)
		assert.Equal(t, media.CapabilityReasonSemanticUnits, record.Reason)
	})

	buildStreamPDF := func(t *testing.T, decoded []byte) []byte {
		t.Helper()
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		_, err := writer.Write(decoded)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		return syntheticPDFObjects("indirect-stream", []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R >>",
			fmt.Sprintf("<< /Length 5 0 R /Filter /FlateDecode >>\nstream\n%s\nendstream", compressed.Bytes()),
			strconv.Itoa(compressed.Len()),
		})
	}

	t.Run("indirect stream length", func(t *testing.T) {
		decoded := []byte("BT /F1 12 Tf 72 720 Td (bounded) Tj ET")
		pdf := buildStreamPDF(t, decoded)
		record, err := media.InspectCapability(bytes.NewReader(pdf),
			inspectionPolicy(pdf, "report.pdf", "application/pdf"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(len(decoded)), record.Measurements.ExpandedBytes)
	})

	t.Run("aggregate expansion", func(t *testing.T) {
		pdf := buildStreamPDF(t, bytes.Repeat([]byte("expanded "), 100_000))
		policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
		policy.MaxExpandedBytes = 128
		record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
		require.NoError(t, err)
		assert.False(t, record.Eligible)
		assert.Equal(t, media.CapabilityReasonExpandedBytes, record.Reason)
	})

	for _, testCase := range []struct {
		name, action string
	}{
		{name: "URI action", action: `/OpenAction << /S /URI /URI (https://example.invalid/probe) >>`},
		{name: "remote go-to action", action: `/OpenAction << /S /GoToR /F (remote.pdf) /D [0 /Fit] >>`},
		{name: "launch action", action: `/OpenAction << /S /Launch /F (program.exe) >>`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			pdf := syntheticPDFObjects("external-action", []string{
				fmt.Sprintf("<< /Type /Catalog /Pages 2 0 R %s >>", testCase.action),
				"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
			})
			record, err := media.InspectCapability(bytes.NewReader(pdf),
				inspectionPolicy(pdf, "report.pdf", "application/pdf"))
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
		})
	}

	// /Type is optional in a file specification, so a path entry decides, in
	// whichever of the five path keys carries it. An embedded file travels
	// inside the document and reaches nothing outside.
	fileSpecifications := []struct {
		name, object string
		eligible     bool
	}{
		{name: "typed", object: "<< /Type /Filespec /F (remote.txt) /AFRelationship /Data >>"},
		{name: "typeless", object: "<< /F (remote.txt) /AFRelationship /Data >>"},
		{name: "unicode path", object: "<< /UF (remote.txt) >>"},
		{name: "hex path", object: "<< /F <72656d6f74652e747874> >>"},
		{name: "dos path", object: `<< /Type /Filespec /DOS (C:\\share\\remote.txt) >>`},
		{name: "mac path", object: "<< /Type /Filespec /Mac (Disk:remote.txt) >>"},
		{name: "unix path", object: "<< /Type /Filespec /Unix (/etc/passwd) >>"},
		{name: "embedded", object: "<< /Type /Filespec /F (data.txt) /EF << /F 5 0 R >> >>", eligible: true},
		// An /EF that names no stream leaves a viewer nothing to open, and a
		// viewer with nothing to open falls back to the path. Reading the key's
		// presence alone let it stand in for a file that never travelled.
		{name: "empty embedded file", object: "<< /Type /Filespec /F (/etc/passwd) /EF << >> >>"},
		{name: "embedded names a non-stream",
			object: "<< /Type /Filespec /F (/etc/passwd) /EF << /F 6 0 R >> >>"},
		{name: "embedded names an integer",
			object: "<< /Type /Filespec /F (/etc/passwd) /EF << /F 42 >> >>"},
		{name: "embedded under a key that holds no path",
			object: "<< /Type /Filespec /F (/etc/passwd) /EF << /Junk 5 0 R >> >>"},
		// ISO 32000-1 §7.11.4 has /EF carry a subset of the path keys, and an
		// ordinary attachment is a /F and a /UF name over one stream. The file
		// is embedded, so requiring a stream per path key would reject it.
		{name: "one stream covers the specification", eligible: true,
			object: "<< /Type /Filespec /F (data.txt) /UF (data.txt) /EF << /F 5 0 R >> >>"},
	}
	for _, testCase := range fileSpecifications {
		t.Run("file specification "+testCase.name, func(t *testing.T) {
			pdf := syntheticPDFObjects("file-spec", []string{
				"<< /Type /Catalog /Pages 2 0 R /AF [4 0 R] >>",
				"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
				"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
				testCase.object,
				"<< /Type /EmbeddedFile /Length 4 >>\nstream\ndata\nendstream",
				"<< /Type /NotAStream /Value 1 >>",
			})
			record, err := media.InspectCapability(bytes.NewReader(pdf),
				inspectionPolicy(pdf, "report.pdf", "application/pdf"))
			require.NoError(t, err)
			assert.Equal(t, testCase.eligible, record.Eligible, record.Reason)
			if !testCase.eligible {
				assert.Equal(t, media.CapabilityReasonExternalReference, record.Reason)
			}
		})
	}

	// An annotation's /F is an integer flags field, not a path.
	t.Run("annotation flags", func(t *testing.T) {
		pdf := syntheticPDFObjects("annot-flags", []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Annots [4 0 R] >>",
			"<< /Type /Annot /Subtype /Text /Rect [0 0 1 1] /F 4 >>",
		})
		record, err := media.InspectCapability(bytes.NewReader(pdf),
			inspectionPolicy(pdf, "report.pdf", "application/pdf"))
		require.NoError(t, err)
		assert.True(t, record.Eligible, record.Reason)
	})

	t.Run("xref and object streams", func(t *testing.T) {
		bodies := []string{
			"<< /Type /Catalog /Pages 2 0 R >>",
			"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>",
		}
		second := len(bodies[0]) + 1
		third := second + len(bodies[1]) + 1
		prolog := fmt.Sprintf("1 0 2 %d 3 %d ", second, third)
		decoded := []byte(prolog + strings.Join(bodies, " "))
		var compressed bytes.Buffer
		writer := zlib.NewWriter(&compressed)
		_, err := writer.Write(decoded)
		require.NoError(t, err)
		require.NoError(t, writer.Close())

		var pdf bytes.Buffer
		_, _ = pdf.WriteString("%PDF-1.5\n")
		objectStreamOffset := pdf.Len()
		_, _ = fmt.Fprintf(&pdf, "4 0 obj\n<< /Type /ObjStm /N 3 /First %d /Length %d /Filter /FlateDecode >>\nstream\n", len(prolog), compressed.Len())
		_, _ = pdf.Write(compressed.Bytes())
		_, _ = pdf.WriteString("\nendstream\nendobj\n")
		xrefOffset := pdf.Len()
		entries := make([]byte, 0, 6*7)
		appendEntry := func(kind byte, field1 uint32, field2 uint16) {
			entries = append(entries, kind, byte(field1>>24), byte(field1>>16), byte(field1>>8), byte(field1), byte(field2>>8), byte(field2))
		}
		appendEntry(0, 0, math.MaxUint16)
		appendEntry(2, 4, 0)
		appendEntry(2, 4, 1)
		appendEntry(2, 4, 2)
		appendEntry(1, uint32(objectStreamOffset), 0) // #nosec G115 -- synthetic fixture is bounded
		appendEntry(1, uint32(xrefOffset), 0)         // #nosec G115 -- synthetic fixture is bounded
		_, _ = fmt.Fprintf(&pdf, "5 0 obj\n<< /Type /XRef /Size 6 /Root 1 0 R /W [1 4 2] /Length %d >>\nstream\n", len(entries))
		_, _ = pdf.Write(entries)
		_, _ = fmt.Fprintf(&pdf, "\nendstream\nendobj\nstartxref\n%d\n%%%%EOF\n", xrefOffset)

		data := pdf.Bytes()
		record, err := media.InspectCapability(bytes.NewReader(data),
			inspectionPolicy(data, "report.pdf", "application/pdf"))
		require.NoError(t, err)
		require.True(t, record.Eligible, record.Reason)
		assert.Equal(t, int64(1), record.Measurements.Pages)
		assert.Positive(t, record.Measurements.ExpandedBytes)
	})
}

func TestInspectRejectsForgedWAVRateAndTrailingBytes(t *testing.T) {
	t.Parallel()
	tests := map[string]func([]byte) []byte{
		"forged byte rate": func(data []byte) []byte {
			binary.LittleEndian.PutUint32(data[28:32], 1<<30)
			return data
		},
		"trailing bytes outside RIFF": func(data []byte) []byte {
			return append(data, []byte("trailing")...)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			wav := mutate(wavBytes(8_000, 8_000))
			policy := inspectionPolicy(wav, "sample.wav", "audio/wav")
			policy.MaxDurationMS = 2_000
			record, err := media.InspectCapability(bytes.NewReader(wav), policy)
			require.NoError(t, err)
			assert.False(t, record.Eligible)
			assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
		})
	}
}

func TestInspectRejectsPathLikePortableFilename(t *testing.T) {
	t.Parallel()
	data := []byte("alpha\n")
	for _, filename := range []string{".", "..", `folder\notes.txt`, "folder/notes.txt"} {
		policy := inspectionPolicy(data, filename, "text/plain")
		_, err := media.InspectCapability(bytes.NewReader(data), policy)
		require.ErrorContains(t, err, "filename", filename)
	}
}

func TestInspectCountsOnlyPDFPageTreeObjects(t *testing.T) {
	t.Parallel()
	pdf := syntheticPDF("/Type /Page in a comment")
	policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	policy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(1), record.Measurements.Pages)
}

func TestInspectRejectsExcessivelyDeepPDFPageTree(t *testing.T) {
	t.Parallel()
	pdf := syntheticDeepPDF(300)
	policy := inspectionPolicy(pdf, "report.pdf", "application/pdf")
	policy.MaxPages = 1
	record, err := media.InspectCapability(bytes.NewReader(pdf), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonMalformed, record.Reason)
}

func TestInspectBoundsVideoFramesFromValidatedSampleTables(t *testing.T) {
	t.Parallel()
	video := mediatest.MP4(64, 48, 1_000)
	decoy := make([]byte, 20)
	binary.BigEndian.PutUint32(decoy[:4], 20)
	copy(decoy[4:8], "stsz")
	binary.BigEndian.PutUint32(decoy[16:20], 100)
	video = append(video, mediatest.Box("free", decoy)...)
	policy := inspectionPolicy(video, "clip.mp4", "video/mp4")
	policy.MaxPixels = 64 * 48
	policy.MaxFrames = 1
	policy.MaxDurationMS = 1_000
	record, err := media.InspectCapability(bytes.NewReader(video), policy)
	require.NoError(t, err)
	require.True(t, record.Eligible, record.Reason)
	assert.Equal(t, int64(1), record.Measurements.Frames)

	policy.MaxFrames = 0
	record, err = media.InspectCapability(bytes.NewReader(video), policy)
	require.NoError(t, err)
	assert.False(t, record.Eligible)
	assert.Equal(t, media.CapabilityReasonVisualBounds, record.Reason)
}

func inspectionPolicy(data []byte, filename, mediaType string) media.InspectionPolicy {
	return media.InspectionPolicy{
		Filename: filename, DeclaredMediaType: mediaType,
		ExpectedBytes: int64(len(data)), ExpectedSHA256: sha256Hex(data),
		DescriptorFingerprint: strings.Repeat("a", 64), ProfileFingerprint: strings.Repeat("b", 64),
		DisclosureFingerprint: strings.Repeat("c", 64), InputKind: document.RenditionInputOriginalFile,
		MaxSourceBytes: 1 << 20, MaxExpandedBytes: 1 << 20, MaxEntryBytes: 1 << 20,
		MaxEntries: 100, MaxNestingDepth: 1, MaxTextLines: 1_000, MaxCharacters: 1 << 20,
		MaxPages: 100, MaxSlides: 100, MaxSheets: 100, MaxCells: 10_000, MaxSpineItems: 1_000, MaxResources: 10_000,
	}
}

type zipEntry struct{ name, body string }

func validPPTXEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/ppt/presentation.xml" ContentType="application/vnd.openxmlformats-officedocument.presentationml.presentation.main+xml"/></Types>`},
		{name: "ppt/presentation.xml", body: `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"/>`},
	}, extra...)
}

func validXLSXEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "[Content_Types].xml", body: `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/></Types>`},
		{name: "xl/workbook.xml", body: `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"/>`},
	}, extra...)
}

func validEPUBEntries(extra ...zipEntry) []zipEntry {
	return append([]zipEntry{
		{name: "mimetype", body: "application/epub+zip"},
		{name: "META-INF/container.xml", body: `<container xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OPS/content.opf"/></rootfiles></container>`},
	}, extra...)
}

func zipBytes(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for _, entry := range entries {
		w, err := zw.Create(entry.name)
		require.NoError(t, err)
		_, err = w.Write([]byte(entry.body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return out.Bytes()
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func wavBytes(byteRate, dataBytes uint32) []byte {
	data := make([]byte, 44+dataBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8)) // #nosec G115 -- synthetic fixture is bounded
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], byteRate)
	binary.LittleEndian.PutUint32(data[28:32], byteRate)
	binary.LittleEndian.PutUint16(data[32:34], 1)
	binary.LittleEndian.PutUint16(data[34:36], 8)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], dataBytes)
	return data
}

func syntheticPDF(label string) []byte {
	return syntheticPDFPages(1, label)
}

func syntheticPDFPages(pageCount int, label string) []byte {
	kids := make([]string, 0, pageCount)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R /Note (/Type /Page in a string) >>",
		"",
	}
	for index := range pageCount {
		kids = append(kids, fmt.Sprintf("%d 0 R", index+3))
		objects = append(objects, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] >>")
	}
	objects[1] = fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), pageCount)
	objects = append(objects,
		"<< /Type /Page /Parent 2 0 R /Note (orphan page object) >>",
		"<< /Length 11 >>\nstream\n/Type /Page\nendstream",
	)
	return syntheticPDFObjects(label, objects)
}

func syntheticDeepPDF(depth int) []byte {
	objects := make([]string, 0, depth+2)
	objects = append(objects, "<< /Type /Catalog /Pages 2 0 R >>")
	for index := range depth {
		number := index + 2
		child := number + 1
		parent := ""
		if index != 0 {
			parent = fmt.Sprintf(" /Parent %d 0 R", number-1)
		}
		objects = append(objects, fmt.Sprintf(
			"<< /Type /Pages /Kids [%d 0 R] /Count 1%s >>", child, parent))
	}
	objects = append(objects, fmt.Sprintf(
		"<< /Type /Page /Parent %d 0 R /MediaBox [0 0 612 792] >>", depth+1))
	return syntheticPDFObjects("deep", objects)
}

func syntheticPDFObjects(label string, objects []string, extraDefinitions ...string) []byte {
	var output bytes.Buffer
	_, _ = fmt.Fprintf(&output, "%%PDF-1.4\n%%%x\n", label)
	offsets := make([]int, len(objects))
	for index, object := range objects {
		offsets[index] = output.Len()
		_, _ = fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	for _, definition := range extraDefinitions {
		_, _ = output.WriteString(definition)
	}
	xref := output.Len()
	_, _ = fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, offset := range offsets {
		_, _ = fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	_, _ = fmt.Fprintf(&output,
		"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return output.Bytes()
}
