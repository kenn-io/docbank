package mistral

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const (
	xlsxWorkbookPath              = "xl/workbook.xml"
	xlsxWorkbookRelationshipsPath = "xl/_rels/workbook.xml.rels"
	xlsxWorkbookNamespace         = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"
	xlsxStrictWorkbookNamespace   = "http://purl.oclc.org/ooxml/spreadsheetml/main"
	xlsxWorksheetRelationshipType = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet"
	xlsxWorksheetContentType      = "application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"
	xlsxWorkbookContentType       = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"
)

var xlsxWorkbookNamespaces = pptxNamespacePair{
	transitional: xlsxWorkbookNamespace,
	strict:       xlsxStrictWorkbookNamespace,
}

var xlsxWorksheetRelationshipTypes = pptxNamespacePair{
	transitional: xlsxWorksheetRelationshipType,
	strict:       "http://purl.oclc.org/ooxml/officeDocument/relationships/worksheet",
}

type xlsxSheet struct {
	ID             string
	RelationshipID string
}

func countXLSXSheets(reader io.ReaderAt, size int64) (int, error) {
	if reader == nil || size <= 0 {
		return 0, errors.New("XLSX source must be non-empty")
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return 0, fmt.Errorf("open XLSX ZIP: %w", err)
	}
	entries, err := indexPPTXEntries(archive.File)
	if err != nil {
		return 0, fmt.Errorf("index XLSX ZIP: %w", err)
	}

	workbookXML, err := readPPTXPart(entries, xlsxWorkbookPath)
	if err != nil {
		return 0, fmt.Errorf("read XLSX workbook: %w", err)
	}
	workbookRelationshipsXML, err := readPPTXPart(entries, xlsxWorkbookRelationshipsPath)
	if err != nil {
		return 0, fmt.Errorf("read XLSX workbook relationships: %w", err)
	}
	rootRelationshipsXML, err := readPPTXPart(entries, pptxRootRelationshipsPath)
	if err != nil {
		return 0, fmt.Errorf("read XLSX root relationships: %w", err)
	}
	contentTypesXML, err := readPPTXPart(entries, ooxmlContentTypesName)
	if err != nil {
		return 0, fmt.Errorf("read XLSX content types: %w", err)
	}

	rootRelationships, rootFamily, err := parsePPTXRelationships(rootRelationshipsXML)
	if err != nil {
		return 0, fmt.Errorf("parse XLSX root relationships: %w", err)
	}
	workbookTarget, err := xlsxRootWorkbookTarget(rootRelationships)
	if err != nil {
		return 0, err
	}
	resolvedWorkbook, err := resolvePPTXTargetFrom("", workbookTarget)
	if err != nil {
		return 0, fmt.Errorf("resolve XLSX office document target: %w", err)
	}
	if !slices.Contains(resolvedWorkbook.keys, pptxPathKey(xlsxWorkbookPath)) {
		return 0, fmt.Errorf("XLSX office document relationship targets %q", workbookTarget)
	}

	declarations, err := parsePPTXContentTypes(contentTypesXML)
	if err != nil {
		return 0, fmt.Errorf("parse XLSX content types: %w", err)
	}
	workbookAliases, err := canonicalPPTXPath(xlsxWorkbookPath)
	if err != nil {
		return 0, fmt.Errorf("canonicalize XLSX workbook: %w", err)
	}
	declaredType, declared, err := declarations.forPartKeys(workbookAliases.keys(), xlsxWorkbookPath)
	if err != nil {
		return 0, fmt.Errorf("resolve XLSX workbook content type: %w", err)
	}
	if !declared || !strings.EqualFold(declaredType, xlsxWorkbookContentType) {
		return 0, errors.New("XLSX workbook has the wrong content type")
	}

	relationshipIDs, workbookFamily, err := parseXLSXSheetIDs(workbookXML)
	if err != nil {
		return 0, fmt.Errorf("parse XLSX workbook: %w", err)
	}
	if workbookFamily != pptxNamespaceFamilyTransitional || rootFamily != workbookFamily {
		return 0, errors.New("XLSX package uses an unsupported or mixed namespace family")
	}
	relationships, _, err := parsePPTXRelationships(workbookRelationshipsXML)
	if err != nil {
		return 0, fmt.Errorf("parse XLSX worksheet relationships: %w", err)
	}

	seenEntries := make(map[*zip.File]struct{}, len(relationshipIDs))
	for _, relationshipID := range relationshipIDs {
		relationship, ok := relationships[relationshipID]
		if !ok {
			return 0, fmt.Errorf("XLSX worksheet relationship %q is unresolved", relationshipID)
		}
		if relationship.Type != xlsxWorksheetRelationshipType {
			return 0, fmt.Errorf("XLSX relationship %q is not a worksheet", relationshipID)
		}
		if relationship.TargetMode != nil && !strings.EqualFold(*relationship.TargetMode, "Internal") {
			return 0, fmt.Errorf("XLSX worksheet relationship %q has an external target", relationshipID)
		}
		target, err := resolvePPTXTargetFrom(xlsxWorkbookPath, relationship.Target)
		if err != nil {
			return 0, fmt.Errorf("resolve XLSX worksheet relationship %q: %w", relationshipID, err)
		}
		entry, err := pptxEntryForKeys(entries, target.keys, relationship.Target)
		if err != nil {
			return 0, err
		}
		if entry.FileInfo().IsDir() {
			return 0, fmt.Errorf("XLSX worksheet target %q is missing", target.decoded)
		}
		if _, exists := seenEntries[entry]; exists {
			return 0, fmt.Errorf("XLSX worksheet relationship target %q is duplicated", target.decoded)
		}
		seenEntries[entry] = struct{}{}
		declaredType, declared, err := declarations.forPartKeys(target.keys, target.decoded)
		if err != nil {
			return 0, err
		}
		if !declared || !strings.EqualFold(declaredType, xlsxWorksheetContentType) {
			return 0, fmt.Errorf("XLSX worksheet target %q has the wrong content type", target.decoded)
		}
	}
	return len(relationshipIDs), nil
}

func xlsxRootWorkbookTarget(relationships map[string]pptxRelationship) (string, error) {
	var target string
	for _, relationship := range relationships {
		if relationship.Type == pptxStrictOfficeDocumentRel || relationship.Type == pptxOfficeDocumentRelType {
			if relationship.Type != pptxOfficeDocumentRelType {
				return "", errors.New("XLSX root uses the strict office document relationship")
			}
			if target != "" {
				return "", errors.New("XLSX root has duplicate office document relationships")
			}
			if relationship.TargetMode != nil && !strings.EqualFold(*relationship.TargetMode, "Internal") {
				return "", errors.New("XLSX office document relationship has an external target")
			}
			target = relationship.Target
		}
	}
	if target == "" {
		return "", errors.New("XLSX root has no office document relationship")
	}
	return target, nil
}

func parseXLSXSheetIDs(data []byte) ([]string, pptxNamespaceFamily, error) {
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	if err := validatePPTXXMLStructure(data); err != nil {
		return nil, pptxNamespaceFamilyUnknown, fmt.Errorf("decode XLSX XML: %w", err)
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var start xml.StartElement
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, pptxNamespaceFamilyUnknown, fmt.Errorf("read XLSX workbook root: %w", err)
		}
		var ok bool
		start, ok = token.(xml.StartElement)
		if ok {
			break
		}
		if data, ok := token.(xml.CharData); ok && strings.TrimSpace(string(data)) != "" {
			return nil, pptxNamespaceFamilyUnknown, errors.New("XLSX workbook has unexpected text")
		}
	}
	family, ok := xlsxWorkbookNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "workbook" {
		return nil, pptxNamespaceFamilyUnknown, errors.New("XLSX workbook has the wrong root element")
	}
	workbookNamespace := xlsxWorkbookNamespaces.value(family)
	var sheets []xlsxSheet
	listCount := 0
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, family, fmt.Errorf("read XLSX workbook markup: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Local == "sheet" || token.Name.Local == "sheets" {
				if token.Name.Space != workbookNamespace {
					return nil, family, errors.New("XLSX workbook has a foreign sheet element")
				}
			}
			if token.Name.Space != workbookNamespace {
				if err := skipXLSXElement(decoder, workbookNamespace); err != nil {
					return nil, family, fmt.Errorf("skip XLSX workbook element: %w", err)
				}
				continue
			}
			if token.Name.Local != "sheets" {
				if token.Name.Local == "sheet" {
					return nil, family, errors.New("XLSX sheet is outside the sheets list")
				}
				if err := skipXLSXElement(decoder, workbookNamespace); err != nil {
					return nil, family, fmt.Errorf("skip XLSX workbook element: %w", err)
				}
				continue
			}
			listCount++
			if listCount > 1 {
				return nil, family, errors.New("XLSX workbook has duplicate sheets lists")
			}
			list, err := parseXLSXSheetList(decoder, token)
			if err != nil {
				return nil, family, err
			}
			sheets = append(sheets, list...)
		case xml.EndElement:
			if token.Name != start.Name {
				return nil, family, errors.New("XLSX workbook has an unexpected closing element")
			}
			if listCount != 1 {
				return nil, family, errors.New("XLSX workbook has no sheets list")
			}
			return xlsxRelationshipIDs(sheets, family)
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return nil, family, errors.New("XLSX workbook has unexpected text")
			}
		}
	}
}

func xlsxRelationshipIDs(sheets []xlsxSheet, family pptxNamespaceFamily) ([]string, pptxNamespaceFamily, error) {
	if len(sheets) == 0 {
		return nil, family, errors.New("XLSX workbook has no sheets")
	}
	relationshipIDs := make([]string, 0, len(sheets))
	seenSheetIDs := make(map[string]struct{}, len(sheets))
	seenRelationshipIDs := make(map[string]struct{}, len(sheets))
	for _, sheet := range sheets {
		if sheet.ID == "" {
			return nil, family, errors.New("XLSX sheet ID is missing")
		}
		if _, exists := seenSheetIDs[sheet.ID]; exists {
			return nil, family, fmt.Errorf("XLSX sheet ID %q is duplicated", sheet.ID)
		}
		if sheet.RelationshipID == "" {
			return nil, family, errors.New("XLSX sheet relationship ID is missing")
		}
		if _, exists := seenRelationshipIDs[sheet.RelationshipID]; exists {
			return nil, family, fmt.Errorf("XLSX sheet relationship %q is duplicated", sheet.RelationshipID)
		}
		seenSheetIDs[sheet.ID] = struct{}{}
		seenRelationshipIDs[sheet.RelationshipID] = struct{}{}
		relationshipIDs = append(relationshipIDs, sheet.RelationshipID)
	}
	return relationshipIDs, family, nil
}

func skipXLSXElement(decoder *xml.Decoder, workbookNamespace string) error {
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read XLSX skipped element: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if (token.Name.Local == "sheet" || token.Name.Local == "sheets") && token.Name.Space != workbookNamespace {
				return errors.New("XLSX workbook has a foreign sheet element")
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}
	return nil
}

func parseXLSXSheetList(decoder *xml.Decoder, start xml.StartElement) ([]xlsxSheet, error) {
	family, ok := xlsxWorkbookNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "sheets" {
		return nil, errors.New("XLSX sheets list has an unexpected element")
	}
	workbookNamespace := xlsxWorkbookNamespaces.value(family)
	var sheets []xlsxSheet
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("read XLSX sheets list markup: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != workbookNamespace || token.Name.Local != "sheet" {
				return nil, errors.New("XLSX sheets list has an unexpected element")
			}
			sheet, err := parseXLSXSheet(token)
			if err != nil {
				return nil, fmt.Errorf("decode XLSX sheet: %w", err)
			}
			if err := skipXLSXElement(decoder, workbookNamespace); err != nil {
				return nil, fmt.Errorf("skip XLSX sheet: %w", err)
			}
			sheets = append(sheets, sheet)
		case xml.EndElement:
			if token.Name != start.Name {
				return nil, errors.New("XLSX sheets list has an unexpected closing element")
			}
			if len(sheets) == 0 {
				return nil, errors.New("XLSX workbook has an empty sheets list")
			}
			return sheets, nil
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return nil, errors.New("XLSX sheets list has unexpected text")
			}
		}
	}
}

func parseXLSXSheet(start xml.StartElement) (xlsxSheet, error) {
	family, ok := xlsxWorkbookNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "sheet" {
		return xlsxSheet{}, errors.New("XLSX sheet has an unexpected element")
	}
	relationshipIDNamespace := pptxRelationshipIDNamespaces.value(family)
	for _, attribute := range start.Attr {
		if attribute.Name.Space == "xmlns" {
			continue
		}
		switch attribute.Name.Local {
		case "sheetId", "name", "state":
			if attribute.Name.Space != "" {
				return xlsxSheet{}, errors.New("XLSX sheet has an unexpected attribute namespace")
			}
		case "id":
			if attribute.Name.Space == "" || attribute.Name.Space == relationshipIDNamespace {
				continue
			}
			if otherFamily, known := pptxRelationshipIDNamespaces.family(attribute.Name.Space); known && otherFamily != family {
				return xlsxSheet{}, errors.New("XLSX sheet relationship ID mixes namespace families")
			}
			return xlsxSheet{}, errors.New("XLSX sheet relationship ID has an unknown namespace")
		}
	}
	id, ok, err := pptxAttribute(start.Attr, "", "sheetId")
	if err != nil {
		return xlsxSheet{}, err
	}
	if !ok || id == "" {
		return xlsxSheet{}, errors.New("XLSX sheet ID is missing")
	}
	relationshipID, ok, err := pptxAttribute(start.Attr, relationshipIDNamespace, "id")
	if err != nil {
		return xlsxSheet{}, err
	}
	if !ok || relationshipID == "" {
		return xlsxSheet{}, errors.New("XLSX sheet relationship ID is missing")
	}
	return xlsxSheet{ID: id, RelationshipID: relationshipID}, nil
}
