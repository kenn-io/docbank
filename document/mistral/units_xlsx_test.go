package mistral

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document"
)

func TestCountXLSXSheets(t *testing.T) {
	tests := []struct {
		name      string
		archive   []byte
		wantUnits int
		wantError bool
	}{
		{
			name: "one worksheet",
			archive: xlsxArchive(t, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
			}}),
			wantUnits: 1,
		},
		{
			name: "listed custom names and hidden or empty sheets",
			archive: xlsxArchiveWithFamily(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{
				{sheetID: "9", relationshipID: "rId9", target: "worksheets/visible.xml"},
				{sheetID: "10", relationshipID: "rId10", target: "worksheets/hidden.xml", state: "hidden"},
				{sheetID: "11", relationshipID: "rId11", target: "worksheets/empty.xml", state: "veryHidden"},
			}, map[string]string{
				"xl/worksheets/sheet99.xml": "orphan",
				"xl/worksheets/visible.xml": `<worksheet xmlns="` + xlsxWorkbookNamespace + `"><hyperlinks><hyperlink ref="A1" r:id="rIdExternal"/></hyperlinks></worksheet>`,
				"xl/customXml/item1.xml":    "unrelated",
			}),
			wantUnits: 3,
		},
		{
			name: "package rooted and renamed targets",
			archive: xlsxArchive(t, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "/xl/worksheets/custom-name.xml",
			}}),
			wantUnits: 1,
		},
		{
			name: "unused relationship and custom property",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/custom.xml",
			}}, "", "", `<Relationships xmlns="`+pptxRelationshipNamespace+`"><Relationship Id="rId1" Type="`+xlsxWorksheetRelationshipType+`" Target="worksheets/custom.xml"/><Relationship Id="rIdUnused" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/customXml" Target="../customXml/item1.xml"/></Relationships>`, "", map[string]string{
				"docProps/custom.xml": "property",
			}),
			wantUnits: 1,
		},
		{
			name: "alternate prefixes and ordinary workbook metadata",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{target: "worksheets/sheet1.xml", relationshipID: "rId1", sheetID: "1"}},
				`<?xml version="1.0"?><x:workbook xmlns:x="`+xlsxWorkbookNamespace+`" xmlns:rel="`+pptxRelationshipIDNamespace+`"><x:fileVersion appName="Microsoft Office Excel"/><x:workbookPr/><x:bookViews/><x:sheets><x:sheet name="one" sheetId="1" rel:id="rId1"/></x:sheets><x:definedNames/><x:calcPr/></x:workbook>`, "", "", "", nil),
			wantUnits: 1,
		},
		{
			name: "UTF-8 BOM in workbook metadata",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
			}}, string([]byte{0xef, 0xbb, 0xbf})+`<workbook xmlns="`+xlsxWorkbookNamespace+`" xmlns:r="`+pptxRelationshipIDNamespace+`"><sheets><sheet sheetId="1" r:id="rId1"/></sheets></workbook>`, "", "", "", nil),
			wantUnits: 1,
		},
		{
			name:      "strict workbook",
			archive:   xlsxArchiveWithFamily(t, pptxNamespaceFamilyStrict, []xlsxTestSheet{{sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml"}}, nil),
			wantError: true,
		},
		{
			name: "mixed namespace families",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxStrictWorkbookNamespace+`" xmlns:r="`+pptxStrictRelationshipIDNS+`"><sheets><sheet sheetId="1" r:id="rId1"/></sheets></workbook>`,
				validPPTXRootRelationships(), "", "", nil),
			wantError: true,
		},
		{
			name: "chartsheet relationship",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "chartsheets/chart1.xml", relationshipType: pptxRelationshipTypeForTest("chartsheet"), contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.chartsheet+xml",
			}}, "", "", "", "", nil),
			wantError: true,
		},
		{
			name: "dialog sheet relationship",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "dialogs/dialog1.xml", relationshipType: pptxRelationshipTypeForTest("dialogsheet"), contentType: "application/vnd.openxmlformats-officedocument.spreadsheetml.dialogsheet+xml",
			}}, "", "", "", "", nil),
			wantError: true,
		},
		{
			name: "macro sheet relationship",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "macrosheets/macro1.xml", relationshipType: pptxRelationshipTypeForTest("macrosheet"), contentType: "application/vnd.ms-excel.macrosheet+xml",
			}}, "", "", "", "", nil),
			wantError: true,
		},
		{
			name: "empty sheets list",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`"><sheets/></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "duplicate sheets lists",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`"><sheets><sheet sheetId="1" r:id="rId1" xmlns:r="`+pptxRelationshipIDNamespace+`"/></sheets><sheets><sheet sheetId="2" r:id="rId2" xmlns:r="`+pptxRelationshipIDNamespace+`"/></sheets></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "duplicate sheet ID",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
			}, {
				sheetID: "1", relationshipID: "rId2", target: "worksheets/sheet2.xml",
			}}, "", "", "", "", nil),
			wantError: true,
		},
		{
			name: "duplicate relationship ID",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
			}, {
				sheetID: "2", relationshipID: "rId1", target: "worksheets/sheet2.xml",
			}}, "", "", "", "", nil),
			wantError: true,
		},
		{
			name: "missing relationship target",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rIdOther", target: "worksheets/sheet1.xml",
			}}, `<workbook xmlns="`+xlsxWorkbookNamespace+`" xmlns:r="`+pptxRelationshipIDNamespace+`"><sheets><sheet sheetId="1" r:id="rIdMissing"/></sheets></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "external relationship target",
			archive: xlsxArchive(t, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "https://example.invalid/sheet.xml",
			}}),
			wantError: true,
		},
		{
			name: "escaping relationship target",
			archive: xlsxArchive(t, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "../../worksheets/sheet1.xml",
			}}),
			wantError: true,
		},
		{
			name: "wrong content type",
			archive: xlsxArchive(t, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml", contentType: "application/xml",
			}}),
			wantError: true,
		},
		{
			name: "wrong workbook content type",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
				sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
			}}, "", "", "", `<Types xmlns="`+pptxContentTypesNamespace+`"><Override PartName="/xl/workbook.xml" ContentType="application/xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="`+xlsxWorksheetContentType+`"/></Types>`, nil),
			wantError: true,
		},
		{
			name: "foreign count-bearing element",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`" xmlns:x="urn:foreign"><x:sheets><x:sheet sheetId="1"/></x:sheets></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "missing sheet ID",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`" xmlns:r="`+pptxRelationshipIDNamespace+`"><sheets><sheet r:id="rId1"/></sheets></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "missing relationship ID",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`"><sheets><sheet sheetId="1"/></sheets></workbook>`, "", "", "", nil),
			wantError: true,
		},
		{
			name: "trailing root",
			archive: xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, nil,
				`<workbook xmlns="`+xlsxWorkbookNamespace+`" xmlns:r="`+pptxRelationshipIDNamespace+`"><sheets><sheet sheetId="1" r:id="rId1"/></sheets></workbook><workbook/>`, "", "", "", nil),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			units, err := countXLSXSheets(bytes.NewReader(test.archive), int64(len(test.archive)))
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.wantUnits, units)
		})
	}

	overLimit := xlsxArchiveWithParts(t, pptxNamespaceFamilyTransitional, []xlsxTestSheet{{
		sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
	}}, strings.Repeat("x", int(pptxMaxXMLBytes)+1), "", "", "", nil)
	_, err := countXLSXSheets(bytes.NewReader(overLimit), int64(len(overLimit)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read bound")
	t.Logf("metadata_limit=%d error=%v", pptxMaxXMLBytes, err)
}

func TestRenditionClientCountsXLSXSheetsForAuthorizedLocalExact(t *testing.T) {
	policy := testPolicy(t, 1<<20, 3)
	manifest := authorizedXLSXManifest(t, policy, 3)
	descriptor := renditionDescriptor(t, policy, manifest, "xlsx")
	source := xlsxArchive(t, []xlsxTestSheet{
		{sheetID: "9", relationshipID: "rId9", target: "worksheets/visible.xml"},
		{sheetID: "10", relationshipID: "rId10", target: "worksheets/hidden.xml", state: "hidden"},
		{sheetID: "11", relationshipID: "rId11", target: "worksheets/empty.xml", state: "veryHidden"},
	})
	fixture := xlsxRenditionFixture(t, descriptor, source)
	var uploaded []byte
	var requests atomic.Int64
	providerMismatch := false
	providerSizeMismatch := false
	client, err := NewRenditionProvider(Profile{
		Policy: policy, CapabilityManifest: manifest, Descriptor: descriptor,
		SecretBinding: "mistral-ocr", Timeout: time.Second, MaxRetries: 1,
		MaxRetryDelay: time.Millisecond,
	}, renditionSecrets{"mistral-ocr": "synthetic-key"}, &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var wire struct {
			Document struct {
				URL string `json:"document_url"`
			} `json:"document"`
		}
		require.NoError(t, json.Unmarshal(body, &wire))
		encoded := strings.TrimPrefix(wire.Document.URL,
			"data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64,")
		uploaded, err = base64.StdEncoding.DecodeString(encoded)
		require.NoError(t, err)
		processed := 3
		if providerMismatch {
			processed = 2
		}
		docSize := len(source)
		if providerSizeMismatch {
			docSize++
		}
		return &http.Response{
			StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(xlsxRenditionResponse(docSize, processed))), Request: request,
		}, nil
	})})
	require.NoError(t, err)

	result, err := client.Render(t.Context(), fixture.upload(), fixture.authorization)
	require.NoError(t, err)
	assert.Equal(t, source, uploaded)
	assert.Equal(t, int64(1), requests.Load())
	assert.Equal(t, document.EvidenceUnitSheet, result.Evidence.UnitKind)
	assert.Len(t, result.Evidence.Units, 3)
	assert.Equal(t, int64(3), result.Receipt.Usage.Units)
	t.Logf("at_limit max_units=%d local_units=3 provider_units=%d requests=%d uploaded_bytes=%d", policy.values.MaxUnits, result.Receipt.Usage.Units, requests.Load(), len(uploaded))

	overLimitSource := xlsxArchive(t, []xlsxTestSheet{
		{sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml"},
		{sheetID: "2", relationshipID: "rId2", target: "worksheets/sheet2.xml"},
		{sheetID: "3", relationshipID: "rId3", target: "worksheets/sheet3.xml"},
		{sheetID: "4", relationshipID: "rId4", target: "worksheets/sheet4.xml"},
	})
	overLimit := xlsxRenditionFixture(t, descriptor, overLimitSource)
	_, err = client.Render(t.Context(), overLimit.upload(), overLimit.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	assert.Equal(t, int64(1), requests.Load())
	t.Logf("over_limit max_units=%d requests=%d error=%v", policy.values.MaxUnits, requests.Load(), err)

	providerMismatch = true
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	require.ErrorIs(t, err, ErrCapabilityContract)
	assert.Equal(t, int64(2), requests.Load())
	t.Logf("provider_mismatch local_units=3 provider_units=2 requests=%d error=%v", requests.Load(), err)

	providerMismatch = false
	providerSizeMismatch = true
	_, err = client.Render(t.Context(), fixture.upload(), fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorMalformedEvidence)
	require.NotErrorIs(t, err, ErrCapabilityContract)
	assert.Equal(t, int64(3), requests.Load())
	t.Logf("provider_size_mismatch local_units=3 requests=%d error=%v", requests.Load(), err)

	mutated := bytes.Clone(source)
	mutated[len(mutated)-1] ^= 1
	_, err = client.Render(t.Context(), &renditionUpload{Reader: bytes.NewReader(mutated), metadata: fixture.metadata}, fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorPolicyRejected)
	assert.Equal(t, int64(3), requests.Load())

	wrongType := fixture.metadata
	wrongType.MediaFamily = "pdf"
	wrongType.MediaType = mediaTypePDF
	_, err = client.Render(t.Context(), &renditionUpload{Reader: bytes.NewReader(source), metadata: wrongType}, fixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorUnsupportedInput)
	assert.Equal(t, int64(3), requests.Load())

	strictSource := xlsxArchiveWithFamily(t, pptxNamespaceFamilyStrict, []xlsxTestSheet{{
		sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml",
	}}, nil)
	strictFixture := xlsxRenditionFixture(t, descriptor, strictSource)
	_, err = client.Render(t.Context(), strictFixture.upload(), strictFixture.authorization)
	assertRenditionCode(t, err, document.RenditionErrorUnsupportedInput)
	assert.Equal(t, int64(3), requests.Load())
}

func TestClientProcessesXLSXWithLocalExactUnits(t *testing.T) {
	policy := testPolicy(t, 1<<20, 3)
	manifest := authorizedXLSXManifest(t, policy, 3)
	authorization, err := policy.Authorize(manifest, "xlsx")
	require.NoError(t, err)
	source := xlsxArchive(t, []xlsxTestSheet{
		{sheetID: "1", relationshipID: "rId1", target: "worksheets/custom-a.xml"},
		{sheetID: "2", relationshipID: "rId2", target: "worksheets/custom-b.xml"},
		{sheetID: "3", relationshipID: "rId3", target: "worksheets/custom-c.xml"},
	})
	spoolDirectory := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, spoolDirectory)
	digest := sha256.Sum256(source)
	prepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(source)), policy, PrepareOptions{
		Directory: spoolDirectory, DeclaredMediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		ExpectedSize: int64(len(source)), ExpectedSHA256: hex.EncodeToString(digest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, prepared.Release()) })

	var uploaded []byte
	var requests atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		body, readErr := io.ReadAll(request.Body)
		if !assert.NoError(t, readErr) {
			return
		}
		var wire struct {
			Document struct {
				URL string `json:"document_url"`
			} `json:"document"`
		}
		if !assert.NoError(t, json.Unmarshal(body, &wire)) {
			return
		}
		encoded := strings.TrimPrefix(wire.Document.URL,
			"data:application/vnd.openxmlformats-officedocument.spreadsheetml.sheet;base64,")
		uploaded, err = base64.StdEncoding.DecodeString(encoded)
		if !assert.NoError(t, err) {
			return
		}
		_, _ = fmt.Fprintf(w, `{"model":"mistral-ocr-4-0","pages":[{"index":0},{"index":1},{"index":2}],"usage_info":{"pages_processed":3,"doc_size_bytes":%d}}`, len(source))
	}))
	defer server.Close()
	client := newServerClient(t, server, policy, ClientConfig{APIKey: "synthetic-key"})
	result, err := client.Process(t.Context(), prepared, authorization)
	require.NoError(t, err)
	assert.Equal(t, source, uploaded)
	assert.Equal(t, 3, result.UnitsProcessed)
	assert.Equal(t, "spreadsheet", result.Document.Family)
	assert.Equal(t, "sheet", result.Document.UnitKind)
	assert.Len(t, result.Document.Units, 3)

	overLimit := xlsxArchive(t, []xlsxTestSheet{
		{sheetID: "1", relationshipID: "rId1", target: "worksheets/sheet1.xml"},
		{sheetID: "2", relationshipID: "rId2", target: "worksheets/sheet2.xml"},
		{sheetID: "3", relationshipID: "rId3", target: "worksheets/sheet3.xml"},
		{sheetID: "4", relationshipID: "rId4", target: "worksheets/sheet4.xml"},
	})
	overDigest := sha256.Sum256(overLimit)
	overSpool := filepath.Join(t.TempDir(), "spool")
	makePrivateDirectory(t, overSpool)
	overPrepared, err := Prepare(t.Context(), io.NopCloser(bytes.NewReader(overLimit)), policy, PrepareOptions{
		Directory: overSpool, DeclaredMediaType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		ExpectedSize: int64(len(overLimit)), ExpectedSHA256: hex.EncodeToString(overDigest[:]),
		MaxSpoolBytes: policy.values.MaxDocumentBytes, MinFreeBytes: 1,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, overPrepared.Release()) })
	_, err = client.Process(t.Context(), overPrepared, authorization)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local unit count")
	assert.Equal(t, int64(1), requests.Load())
}

func authorizedXLSXManifest(t *testing.T, policy Policy, units int) CapabilityManifest {
	t.Helper()
	manifest := syntheticManifest(t, policy, true)
	for index := range manifest.Results {
		if manifest.Results[index].FormatID != "xlsx" {
			continue
		}
		manifest.Results[index].ReasonCode = ""
		manifest.Results[index].UnitBoundMethod = UnitBoundLocalExact
		manifest.Results[index].UnitCount = units
		manifest.Results[index].UnitsProcessed = units
		manifest.Results[index].LocalUnits = units
	}
	require.NoError(t, manifest.ValidateComplete())
	return manifest
}

type xlsxTestSheet struct {
	sheetID          string
	relationshipID   string
	target           string
	entryName        string
	contentTypeName  string
	contentType      string
	relationshipType string
	targetMode       string
	state            string
}

func xlsxArchive(t *testing.T, sheets []xlsxTestSheet) []byte {
	t.Helper()
	return xlsxArchiveWithFamily(t, pptxNamespaceFamilyTransitional, sheets, nil)
}

func xlsxArchiveWithFamily(t *testing.T, family pptxNamespaceFamily, sheets []xlsxTestSheet, extra map[string]string) []byte {
	t.Helper()
	return xlsxArchiveWithParts(t, family, sheets, "", "", "", "", extra)
}

func xlsxArchiveWithParts(
	t *testing.T, family pptxNamespaceFamily, sheets []xlsxTestSheet,
	workbook, rootRelationships, workbookRelationships, contentTypes string, extra map[string]string,
) []byte {
	t.Helper()
	workbookNamespace := xlsxWorkbookNamespaces.value(family)
	relationshipIDNamespace := pptxRelationshipIDNamespaces.value(family)
	worksheetRelationshipType := xlsxWorksheetRelationshipTypes.value(family)
	if workbook == "" {
		var markup strings.Builder
		fmt.Fprintf(&markup, `<workbook xmlns="%s" xmlns:r="%s"><sheets>`, workbookNamespace, relationshipIDNamespace)
		for _, sheet := range sheets {
			fmt.Fprintf(&markup, `<sheet name="Sheet%s" sheetId="%s" r:id="%s"`, sheet.sheetID, sheet.sheetID, sheet.relationshipID)
			if sheet.state != "" {
				fmt.Fprintf(&markup, ` state="%s"`, sheet.state)
			}
			markup.WriteString(`/>`)
		}
		markup.WriteString(`</sheets></workbook>`)
		workbook = markup.String()
	}
	if rootRelationships == "" {
		rootRelationships = `<Relationships xmlns="` + pptxRelationshipNamespace + `"><Relationship Id="rIdRoot" Type="` + pptxOfficeDocumentRelationshipTypes.value(family) + `" Target="xl/workbook.xml"/></Relationships>`
	}
	if workbookRelationships == "" {
		var markup strings.Builder
		fmt.Fprintf(&markup, `<Relationships xmlns="%s">`, pptxRelationshipNamespace)
		for _, sheet := range sheets {
			targetMode := ""
			if sheet.targetMode != "" {
				targetMode = ` TargetMode="` + sheet.targetMode + `"`
			}
			relationshipType := sheet.relationshipType
			if relationshipType == "" {
				relationshipType = worksheetRelationshipType
			}
			fmt.Fprintf(&markup, `<Relationship Id="%s" Type="%s" Target="%s"%s/>`, sheet.relationshipID, relationshipType, sheet.target, targetMode)
		}
		markup.WriteString(`</Relationships>`)
		workbookRelationships = markup.String()
	}
	if contentTypes == "" {
		var markup strings.Builder
		markup.WriteString(`<Types xmlns="` + pptxContentTypesNamespace + `"><Override PartName="/xl/workbook.xml" ContentType="` + xlsxWorkbookContentType + `"/>`)
		for _, sheet := range sheets {
			if sheet.target == "" {
				continue
			}
			targetName := xlsxArchiveTargetName(sheet.target)
			contentTypeName := "/" + targetName
			if sheet.contentTypeName != "" {
				contentTypeName = sheet.contentTypeName
			}
			contentType := sheet.contentType
			if contentType == "" {
				contentType = xlsxWorksheetContentType
			}
			fmt.Fprintf(&markup, `<Override PartName="%s" ContentType="%s"/>`, contentTypeName, contentType)
		}
		markup.WriteString(`</Types>`)
		contentTypes = markup.String()
	}
	entries := map[string]string{
		xlsxWorkbookPath:              workbook,
		xlsxWorkbookRelationshipsPath: workbookRelationships,
		pptxRootRelationshipsPath:     rootRelationships,
		ooxmlContentTypesName:         contentTypes,
	}
	for _, sheet := range sheets {
		if sheet.target == "" {
			continue
		}
		entryName := xlsxArchiveTargetName(sheet.target)
		if sheet.entryName != "" {
			entryName = sheet.entryName
		}
		entries[entryName] = `<worksheet xmlns="` + workbookNamespace + `"/>`
	}
	maps.Copy(entries, extra)
	return documentZIP(t, entries)
}

func xlsxArchiveTargetName(target string) string {
	if target, found := strings.CutPrefix(target, "/"); found {
		return target
	}
	return path.Clean(path.Join("xl", target))
}

func pptxRelationshipTypeForTest(kind string) string {
	return "http://schemas.openxmlformats.org/officeDocument/2006/relationships/" + kind
}

func xlsxRenditionFixture(
	t *testing.T, descriptor document.RenditionDescriptor, source []byte,
) renditionTestFixture {
	t.Helper()
	digest := sha256.Sum256(source)
	metadata := document.AuthorizedUploadMetadata{
		Filename: "document.xlsx", MediaFamily: "spreadsheet",
		MediaType:  "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		ByteLength: int64(len(source)), SHA256: hex.EncodeToString(digest[:]),
		CapabilityRecordChecksum: strings.Repeat("2", 64), ProviderMetadataChecksum: strings.Repeat("3", 64),
		InputKind: document.RenditionInputOriginalFile,
	}
	started := time.Now().UTC().Add(-time.Minute)
	return renditionTestFixture{metadata: metadata, source: source, authorization: document.RenditionAuthorization{
		ProviderID: descriptor.ID, DescriptorFingerprint: descriptor.Fingerprint,
		PolicyFingerprint:           descriptor.PolicyFingerprint,
		RenditionRequestFingerprint: strings.Repeat("4", 64), SourceSHA256: metadata.SHA256,
		SourceBytes: metadata.ByteLength, CapabilityRecordChecksum: metadata.CapabilityRecordChecksum,
		ProviderMetadataChecksum: metadata.ProviderMetadataChecksum, MediaFamily: metadata.MediaFamily,
		MediaType: metadata.MediaType, InputKind: metadata.InputKind,
		MaxProviderMarkdownBytes: 4096, MaxTotalResultBytes: 32768,
		AuthorizedAt: started.Format("2006-01-02T15:04:05.000000000Z"),
		ExpiresAt:    started.Add(10 * time.Minute).Format("2006-01-02T15:04:05.000000000Z"),
	}}
}

func xlsxRenditionResponse(sourceBytes, processed int) string {
	return fmt.Sprintf(
		`{"model":"mistral-ocr-4-0","pages":[{"index":0,"markdown":"first"},{"index":1,"markdown":"second"},{"index":2,"markdown":"third"}],"usage_info":{"pages_processed":%d,"doc_size_bytes":%d}}`,
		processed, sourceBytes,
	)
}
