package mistral

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"slices"
	"strings"
)

type localUnitCounter func(io.ReaderAt, int64) (int, error)

// localUnitCounters is the Mistral-owned source-unit counter registry.
var localUnitCounters = map[string]localUnitCounter{
	"pptx":       countPPTXSlides,
	"txt":        countTextLines,
	"markdown":   countTextLines,
	"csv":        countCSVRecords,
	"json":       countJSONValues,
	"jsonl":      countJSONLines,
	"yaml":       countYAMLDocuments,
	"go":         countTextLines,
	"python":     countTextLines,
	"javascript": countTextLines,
	"rst":        countTextLines,
	"latex":      countTextLines,
	"xml":        countXMLDocument,
	"eml":        countMailMessages,
	"msg":        countMSGMessages,
}

type pptxNamespaceFamily uint8

const (
	pptxNamespaceFamilyUnknown pptxNamespaceFamily = iota
	pptxNamespaceFamilyTransitional
	pptxNamespaceFamilyStrict
)

type pptxNamespacePair struct {
	transitional string
	strict       string
}

const (
	pptxPresentationPath        = "ppt/presentation.xml"
	pptxPresentationRelsPath    = "ppt/_rels/presentation.xml.rels"
	pptxRootRelationshipsPath   = "_rels/.rels"
	pptxMaxXMLBytes             = int64(1 << 20)
	pptxPresentationNamespace   = "http://schemas.openxmlformats.org/presentationml/2006/main"
	pptxStrictPresentationNS    = "http://purl.oclc.org/ooxml/presentationml/main"
	pptxRelationshipNamespace   = "http://schemas.openxmlformats.org/package/2006/relationships"
	pptxRelationshipIDNamespace = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	pptxStrictRelationshipIDNS  = "http://purl.oclc.org/ooxml/officeDocument/relationships"
	pptxContentTypesNamespace   = "http://schemas.openxmlformats.org/package/2006/content-types"
	pptxOfficeDocumentRelType   = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument"
	pptxStrictOfficeDocumentRel = "http://purl.oclc.org/ooxml/officeDocument/relationships/officeDocument"
	pptxRelationshipType        = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
	pptxStrictRelationshipType  = "http://purl.oclc.org/ooxml/officeDocument/relationships/slide"
	pptxSlideContentType        = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
)

var (
	pptxPresentationNamespaces = pptxNamespacePair{
		transitional: pptxPresentationNamespace,
		strict:       pptxStrictPresentationNS,
	}
	pptxRelationshipIDNamespaces = pptxNamespacePair{
		transitional: pptxRelationshipIDNamespace,
		strict:       pptxStrictRelationshipIDNS,
	}
	pptxOfficeDocumentRelationshipTypes = pptxNamespacePair{
		transitional: pptxOfficeDocumentRelType,
		strict:       pptxStrictOfficeDocumentRel,
	}
	pptxSlideRelationshipTypes = pptxNamespacePair{
		transitional: pptxRelationshipType,
		strict:       pptxStrictRelationshipType,
	}
)

func (pair pptxNamespacePair) family(uri string) (pptxNamespaceFamily, bool) {
	switch uri {
	case pair.transitional:
		return pptxNamespaceFamilyTransitional, true
	case pair.strict:
		return pptxNamespaceFamilyStrict, true
	default:
		return pptxNamespaceFamilyUnknown, false
	}
}

func (pair pptxNamespacePair) value(family pptxNamespaceFamily) string {
	switch family {
	case pptxNamespaceFamilyTransitional:
		return pair.transitional
	case pptxNamespaceFamilyStrict:
		return pair.strict
	default:
		return ""
	}
}

func pptxRelationshipTypeFamily(typeName string) (pptxNamespaceFamily, bool) {
	if family, ok := pptxOfficeDocumentRelationshipTypes.family(typeName); ok {
		return family, true
	}
	return pptxSlideRelationshipTypes.family(typeName)
}

type pptxRelationship struct {
	ID         string
	Type       string
	Target     string
	TargetMode *string
	family     pptxNamespaceFamily
}

type pptxPathAliases struct {
	raw     string
	decoded string
}

func (aliases pptxPathAliases) keys() []string {
	if aliases.raw == aliases.decoded {
		return []string{aliases.raw}
	}
	return []string{aliases.raw, aliases.decoded}
}

type pptxResolvedTarget struct {
	keys    []string
	decoded string
}

func countPPTXSlides(reader io.ReaderAt, size int64) (int, error) {
	if reader == nil || size <= 0 {
		return 0, errors.New("PPTX source must be non-empty")
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return 0, fmt.Errorf("open PPTX ZIP: %w", err)
	}
	entries, err := indexPPTXEntries(archive.File)
	if err != nil {
		return 0, err
	}

	presentationXML, err := readPPTXPart(entries, pptxPresentationPath)
	if err != nil {
		return 0, err
	}
	relationshipsXML, err := readPPTXPart(entries, pptxPresentationRelsPath)
	if err != nil {
		return 0, err
	}
	contentTypesXML, err := readPPTXPart(entries, ooxmlContentTypesName)
	if err != nil {
		return 0, err
	}
	rootRelationshipsXML, err := readPPTXPart(entries, pptxRootRelationshipsPath)
	if err != nil {
		return 0, err
	}
	rootFamily, err := validatePPTXRootPresentation(rootRelationshipsXML)
	if err != nil {
		return 0, fmt.Errorf("validate PPTX root presentation relationship: %w", err)
	}

	relationshipIDs, presentationFamily, err := parsePPTXSlideIDs(presentationXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX presentation: %w", err)
	}
	if rootFamily != presentationFamily {
		return 0, errors.New("PPTX package mixes namespace families")
	}
	relationships, relationshipsFamily, err := parsePPTXRelationships(relationshipsXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX presentation relationships: %w", err)
	}
	if relationshipsFamily != presentationFamily {
		return 0, errors.New("PPTX package mixes namespace families")
	}
	contentDeclarations, err := parsePPTXContentTypes(contentTypesXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX content types: %w", err)
	}

	seenEntries := make(map[*zip.File]struct{}, len(relationshipIDs))
	for _, relationshipID := range relationshipIDs {
		relationship, ok := relationships[relationshipID]
		if !ok {
			return 0, fmt.Errorf("PPTX slide relationship %q is unresolved", relationshipID)
		}
		if relationship.Type != pptxSlideRelationshipTypes.value(presentationFamily) {
			return 0, fmt.Errorf("PPTX relationship %q is not a slide", relationshipID)
		}
		if relationship.TargetMode != nil && !strings.EqualFold(*relationship.TargetMode, "Internal") {
			return 0, fmt.Errorf("PPTX slide relationship %q has an external target", relationshipID)
		}
		target, err := resolvePPTXTargetFrom(pptxPresentationPath, relationship.Target)
		if err != nil {
			return 0, fmt.Errorf("resolve PPTX slide relationship %q: %w", relationshipID, err)
		}
		entry, err := pptxEntryForKeys(entries, target.keys, relationship.Target)
		if err != nil {
			return 0, err
		}
		if entry.FileInfo().IsDir() {
			return 0, fmt.Errorf("PPTX slide target %q is missing", target.decoded)
		}
		if _, exists := seenEntries[entry]; exists {
			return 0, fmt.Errorf("PPTX slide relationship target %q is duplicated", target.decoded)
		}
		seenEntries[entry] = struct{}{}
		declaredType, declared, err := contentDeclarations.forPartKeys(target.keys, target.decoded)
		if err != nil {
			return 0, err
		}
		if !declared || !strings.EqualFold(declaredType, pptxSlideContentType) {
			return 0, fmt.Errorf("PPTX slide target %q has the wrong content type", target.decoded)
		}
	}
	return len(relationshipIDs), nil
}

func readPPTXPart(entries map[string]*zip.File, name string) ([]byte, error) {
	aliases, err := canonicalPPTXPath(name)
	if err != nil {
		return nil, err
	}
	entry, err := pptxEntryForKeys(entries, aliases.keys(), name)
	if err != nil {
		return nil, err
	}
	return readPPTXXML(entry)
}

func pptxEntryForKeys(entries map[string]*zip.File, keys []string, name string) (*zip.File, error) {
	var found *zip.File
	for _, key := range keys {
		entry, ok := entries[key]
		if !ok {
			continue
		}
		if found != nil && found != entry {
			return nil, fmt.Errorf("PPTX target %q has ambiguous ZIP entries", name)
		}
		found = entry
	}
	if found == nil {
		return nil, fmt.Errorf("PPTX ZIP is missing %q", name)
	}
	return found, nil
}

func indexPPTXEntries(files []*zip.File) (map[string]*zip.File, error) {
	entries := make(map[string]*zip.File, len(files))
	for _, entry := range files {
		if hasPPTXEncodedSeparator(entry.Name) {
			return nil, fmt.Errorf("PPTX ZIP entry %q contains an encoded path separator", entry.Name)
		}
		aliases, err := canonicalPPTXPath(entry.Name)
		if err != nil {
			return nil, fmt.Errorf("normalize PPTX ZIP entry %q: %w", entry.Name, err)
		}
		for _, key := range aliases.keys() {
			if existing, exists := entries[key]; exists && existing != entry {
				return nil, fmt.Errorf("PPTX ZIP contains ambiguous equivalent entries %q and %q", existing.Name, entry.Name)
			}
			entries[key] = entry
		}
	}
	return entries, nil
}

func readPPTXXML(entry *zip.File) ([]byte, error) {
	if entry.UncompressedSize64 > uint64(pptxMaxXMLBytes) {
		return nil, fmt.Errorf("PPTX XML part %q exceeds the read bound", entry.Name)
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, fmt.Errorf("open PPTX XML part %q: %w", entry.Name, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, pptxMaxXMLBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read PPTX XML part %q: %w", entry.Name, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close PPTX XML part %q: %w", entry.Name, closeErr)
	}
	if int64(len(data)) > pptxMaxXMLBytes || uint64(len(data)) != entry.UncompressedSize64 {
		return nil, fmt.Errorf("PPTX XML part %q exceeded the read bound", entry.Name)
	}
	data = bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})
	return data, nil
}

type pptxPresentation struct {
	SlideList []pptxSlideList
	family    pptxNamespaceFamily
}

type pptxSlideList struct {
	Slides []pptxSlideID
}

type pptxSlideID struct {
	ID             string
	RelationshipID string
}

type pptxRelationshipDocument struct {
	Relationships []pptxRelationship
	family        pptxNamespaceFamily
}

type pptxContentTypesDocument struct {
	XMLName   xml.Name          `xml:"Types"`
	Defaults  []pptxDefaultType `xml:"http://schemas.openxmlformats.org/package/2006/content-types Default"`
	Overrides []pptxContentType `xml:"http://schemas.openxmlformats.org/package/2006/content-types Override"`
}

type pptxContentDeclarations struct {
	defaults  map[string]string
	overrides map[string]pptxContentDeclaration
}

func (declarations pptxContentDeclarations) forPartKeys(keys []string, decodedName string) (string, bool, error) {
	var declaration pptxContentDeclaration
	for _, key := range keys {
		value, ok := declarations.overrides[key]
		if !ok {
			continue
		}
		if declaration.partName != "" && declaration.partName != value.partName {
			return "", false, fmt.Errorf("PPTX content type for %q has ambiguous declarations", decodedName)
		}
		declaration = value
	}
	if declaration.partName != "" {
		return declaration.contentType, true, nil
	}
	contentType, ok := declarations.defaults[strings.ToLower(path.Ext(decodedName))]
	return contentType, ok, nil
}

type pptxContentDeclaration struct {
	partName    string
	contentType string
}

func pptxPathKey(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}

func hasPPTXEncodedSeparator(value string) bool {
	value = strings.ToLower(value)
	return strings.Contains(value, "%2f") || strings.Contains(value, "%5c")
}

func canonicalPPTXPath(value string) (pptxPathAliases, error) {
	if value == "" {
		return pptxPathAliases{}, errors.New("PPTX part name is not an internal path")
	}
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return pptxPathAliases{}, errors.New("PPTX part name is not a valid path")
	}
	if strings.ContainsAny(value, "\\\x00") || strings.ContainsAny(decoded, "\\\x00") {
		return pptxPathAliases{}, errors.New("PPTX part name is not an internal path")
	}
	raw, err := cleanPPTXPath(value)
	if err != nil {
		return pptxPathAliases{}, err
	}
	decodedClean, err := cleanPPTXPath(decoded)
	if err != nil {
		return pptxPathAliases{}, err
	}
	return pptxPathAliases{raw: pptxPathKey(raw), decoded: pptxPathKey(decodedClean)}, nil
}

func cleanPPTXPath(value string) (string, error) {
	value = strings.TrimPrefix(value, "/")
	cleaned := path.Clean(value)
	if cleaned == "." || path.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("PPTX part name escapes the package root")
	}
	return cleaned, nil
}

type pptxDefaultType struct {
	Extension   string
	ContentType string
}

type pptxContentType struct {
	PartName    string
	ContentType string
}

func (presentation *pptxPresentation) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	family, ok := pptxPresentationNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "presentation" {
		return errors.New("PPTX presentation has the wrong root element")
	}
	presentation.family = family
	presentation.SlideList = nil
	for {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read PPTX presentation markup: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != pptxPresentationNamespaces.value(family) {
				return errors.New("PPTX presentation has an unexpected element")
			}
			if token.Name.Local != "sldIdLst" {
				if err := decoder.Skip(); err != nil {
					return fmt.Errorf("skip PPTX presentation element: %w", err)
				}
				continue
			}
			slideList := pptxSlideList{}
			if err := decoder.DecodeElement(&slideList, &token); err != nil {
				return fmt.Errorf("decode PPTX slide list: %w", err)
			}
			presentation.SlideList = append(presentation.SlideList, slideList)
		case xml.EndElement:
			if token.Name != start.Name {
				return errors.New("PPTX presentation has an unexpected closing element")
			}
			return nil
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return errors.New("PPTX presentation has unexpected text")
			}
		}
	}
}

func (slideList *pptxSlideList) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	family, ok := pptxPresentationNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "sldIdLst" {
		return errors.New("PPTX slide list has an unexpected element")
	}
	slideList.Slides = nil
	for {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read PPTX slide list markup: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != pptxPresentationNamespaces.value(family) || token.Name.Local != "sldId" {
				return errors.New("PPTX slide list has an unexpected element")
			}
			slide := pptxSlideID{}
			if err := decoder.DecodeElement(&slide, &token); err != nil {
				return fmt.Errorf("decode PPTX slide ID: %w", err)
			}
			slideList.Slides = append(slideList.Slides, slide)
		case xml.EndElement:
			if token.Name != start.Name {
				return errors.New("PPTX slide list has an unexpected closing element")
			}
			return nil
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return errors.New("PPTX slide list has unexpected text")
			}
		}
	}
}

func (slide *pptxSlideID) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	family, ok := pptxPresentationNamespaces.family(start.Name.Space)
	if !ok || start.Name.Local != "sldId" {
		return errors.New("PPTX slide list has an unexpected element")
	}
	relationshipIDNamespace := pptxRelationshipIDNamespaces.value(family)
	for _, attribute := range start.Attr {
		if attribute.Name.Space == "xmlns" {
			continue
		}
		if attribute.Name.Local != "id" || attribute.Name.Space == "" || attribute.Name.Space == relationshipIDNamespace {
			continue
		}
		if otherFamily, known := pptxRelationshipIDNamespaces.family(attribute.Name.Space); known && otherFamily != family {
			return errors.New("PPTX slide relationship ID mixes namespace families")
		}
		return errors.New("PPTX slide relationship ID has an unknown namespace")
	}
	id, ok, err := pptxAttribute(start.Attr, "", "id")
	if err != nil {
		return err
	}
	if !ok || id == "" {
		return errors.New("PPTX slide ID is missing")
	}
	relationshipID, ok, err := pptxAttribute(start.Attr, relationshipIDNamespace, "id")
	if err != nil {
		return err
	}
	if !ok || relationshipID == "" {
		return errors.New("PPTX slide relationship ID is missing")
	}
	if err := decoder.Skip(); err != nil {
		return fmt.Errorf("skip PPTX slide: %w", err)
	}
	slide.ID, slide.RelationshipID = id, relationshipID
	return nil
}

func (relationship *pptxRelationship) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != pptxRelationshipNamespace || start.Name.Local != "Relationship" {
		return errors.New("PPTX relationships have an unexpected element")
	}
	for _, attribute := range start.Attr {
		if attribute.Name.Space == "xmlns" {
			continue
		}
		switch attribute.Name.Local {
		case "Id", "Type", "Target", "TargetMode":
			if attribute.Name.Space != "" {
				return errors.New("PPTX relationship has an unexpected attribute namespace")
			}
		}
	}
	id, ok, err := pptxAttribute(start.Attr, "", "Id")
	if err != nil {
		return err
	}
	if !ok || id == "" {
		return errors.New("PPTX relationship ID is missing")
	}
	typeName, ok, err := pptxAttribute(start.Attr, "", "Type")
	if err != nil {
		return err
	}
	if !ok || typeName == "" {
		return fmt.Errorf("PPTX relationship %q has no type", id)
	}
	target, ok, err := pptxAttribute(start.Attr, "", "Target")
	if err != nil {
		return err
	}
	if !ok || target == "" {
		return fmt.Errorf("PPTX relationship %q has no target", id)
	}
	targetMode, targetModeSet, err := pptxAttribute(start.Attr, "", "TargetMode")
	if err != nil {
		return err
	}
	if err := decoder.Skip(); err != nil {
		return fmt.Errorf("skip PPTX relationship: %w", err)
	}
	relationship.ID, relationship.Type, relationship.Target = id, typeName, target
	relationship.family, _ = pptxRelationshipTypeFamily(typeName)
	if targetModeSet {
		relationship.TargetMode = &targetMode
	}
	return nil
}

func (document *pptxRelationshipDocument) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != pptxRelationshipNamespace || start.Name.Local != "Relationships" {
		return errors.New("PPTX relationships have the wrong root element")
	}
	document.family = pptxNamespaceFamilyUnknown
	document.Relationships = nil
	for {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read PPTX relationship markup: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if token.Name.Space != pptxRelationshipNamespace || token.Name.Local != "Relationship" {
				return errors.New("PPTX relationships have an unexpected element")
			}
			relationship := pptxRelationship{}
			if err := decoder.DecodeElement(&relationship, &token); err != nil {
				return fmt.Errorf("decode PPTX relationship: %w", err)
			}
			if relationship.family != pptxNamespaceFamilyUnknown {
				if document.family != pptxNamespaceFamilyUnknown && document.family != relationship.family {
					return errors.New("PPTX relationships mix namespace families")
				}
				document.family = relationship.family
			}
			document.Relationships = append(document.Relationships, relationship)
		case xml.EndElement:
			if token.Name != start.Name {
				return errors.New("PPTX relationships have an unexpected closing element")
			}
			return nil
		case xml.CharData:
			if strings.TrimSpace(string(token)) != "" {
				return errors.New("PPTX relationships have unexpected text")
			}
		}
	}
}

func (contentType *pptxContentType) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != pptxContentTypesNamespace || start.Name.Local != "Override" {
		return errors.New("PPTX content types have an unexpected element")
	}
	partName, ok, err := pptxAttribute(start.Attr, "", "PartName")
	if err != nil {
		return err
	}
	if !ok || partName == "" {
		return errors.New("PPTX content type part name is missing")
	}
	value, ok, err := pptxAttribute(start.Attr, "", "ContentType")
	if err != nil {
		return err
	}
	if !ok || value == "" {
		return fmt.Errorf("PPTX content type for %q is missing", partName)
	}
	if err := decoder.Skip(); err != nil {
		return fmt.Errorf("skip PPTX content type: %w", err)
	}
	contentType.PartName, contentType.ContentType = partName, value
	return nil
}

func (contentType *pptxDefaultType) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != pptxContentTypesNamespace || start.Name.Local != "Default" {
		return errors.New("PPTX content types have an unexpected element")
	}
	extension, ok, err := pptxAttribute(start.Attr, "", "Extension")
	if err != nil {
		return err
	}
	if !ok || extension == "" {
		return errors.New("PPTX content type extension is missing")
	}
	value, ok, err := pptxAttribute(start.Attr, "", "ContentType")
	if err != nil {
		return err
	}
	if !ok || value == "" {
		return fmt.Errorf("PPTX default content type for %q is missing", extension)
	}
	if err := decoder.Skip(); err != nil {
		return fmt.Errorf("skip PPTX default content type: %w", err)
	}
	contentType.Extension, contentType.ContentType = extension, value
	return nil
}

func pptxAttribute(attributes []xml.Attr, space, local string) (string, bool, error) {
	var value string
	found := false
	for _, attribute := range attributes {
		if attribute.Name.Space != space || attribute.Name.Local != local {
			continue
		}
		if found {
			return "", false, fmt.Errorf("PPTX XML attribute %q is duplicated", local)
		}
		found, value = true, attribute.Value
	}
	return value, found, nil
}

func parsePPTXSlideIDs(data []byte) ([]string, pptxNamespaceFamily, error) {
	var document pptxPresentation
	if err := decodePPTXXML(data, &document); err != nil {
		return nil, pptxNamespaceFamilyUnknown, err
	}
	if len(document.SlideList) != 1 || len(document.SlideList[0].Slides) == 0 {
		return nil, document.family, errors.New("PPTX presentation has no slides")
	}
	relationshipIDs := make([]string, 0, len(document.SlideList[0].Slides))
	seenRelationshipIDs := make(map[string]struct{}, len(document.SlideList[0].Slides))
	seenSlideIDs := make(map[string]struct{}, len(document.SlideList[0].Slides))
	for _, slide := range document.SlideList[0].Slides {
		if slide.ID == "" {
			return nil, document.family, errors.New("PPTX slide ID is missing")
		}
		if slide.RelationshipID == "" {
			return nil, document.family, errors.New("PPTX slide relationship ID is missing")
		}
		if _, exists := seenRelationshipIDs[slide.RelationshipID]; exists {
			return nil, document.family, fmt.Errorf("PPTX slide relationship %q is duplicated", slide.RelationshipID)
		}
		if _, exists := seenSlideIDs[slide.ID]; exists {
			return nil, document.family, fmt.Errorf("PPTX slide ID %q is duplicated", slide.ID)
		}
		seenRelationshipIDs[slide.RelationshipID] = struct{}{}
		seenSlideIDs[slide.ID] = struct{}{}
		relationshipIDs = append(relationshipIDs, slide.RelationshipID)
	}
	return relationshipIDs, document.family, nil
}

func parsePPTXRelationships(data []byte) (map[string]pptxRelationship, pptxNamespaceFamily, error) {
	var document pptxRelationshipDocument
	if err := decodePPTXXML(data, &document); err != nil {
		return nil, pptxNamespaceFamilyUnknown, err
	}
	relationships := make(map[string]pptxRelationship, len(document.Relationships))
	for _, relationship := range document.Relationships {
		if relationship.ID == "" || relationship.Type == "" || relationship.Target == "" {
			return nil, document.family, errors.New("PPTX relationship is incomplete")
		}
		if family, known := pptxRelationshipTypeFamily(relationship.Type); known && family != document.family {
			return nil, document.family, fmt.Errorf("PPTX relationship %q mixes namespace families", relationship.ID)
		}
		if _, exists := relationships[relationship.ID]; exists {
			return nil, document.family, fmt.Errorf("PPTX relationship %q is duplicated", relationship.ID)
		}
		relationships[relationship.ID] = relationship
	}
	return relationships, document.family, nil
}

func validatePPTXRootPresentation(data []byte) (pptxNamespaceFamily, error) {
	relationships, family, err := parsePPTXRelationships(data)
	if err != nil {
		return pptxNamespaceFamilyUnknown, err
	}
	var target string
	for _, relationship := range relationships {
		if relationship.Type != pptxOfficeDocumentRelationshipTypes.value(family) {
			continue
		}
		if target != "" {
			return pptxNamespaceFamilyUnknown, errors.New("PPTX root has duplicate office document relationships")
		}
		if relationship.TargetMode != nil && !strings.EqualFold(*relationship.TargetMode, "Internal") {
			return pptxNamespaceFamilyUnknown, errors.New("PPTX office document relationship has an external target")
		}
		target = relationship.Target
	}
	if target == "" {
		return pptxNamespaceFamilyUnknown, errors.New("PPTX root has no office document relationship")
	}
	resolved, err := resolvePPTXTargetFrom("", target)
	if err != nil {
		return pptxNamespaceFamilyUnknown, fmt.Errorf("resolve PPTX office document target: %w", err)
	}
	if !slices.Contains(resolved.keys, pptxPathKey(pptxPresentationPath)) {
		return pptxNamespaceFamilyUnknown, fmt.Errorf("PPTX office document relationship targets %q", target)
	}
	return family, nil
}

func parsePPTXContentTypes(data []byte) (pptxContentDeclarations, error) {
	var document pptxContentTypesDocument
	if err := decodePPTXXML(data, &document); err != nil {
		return pptxContentDeclarations{}, err
	}
	if document.XMLName.Space != pptxContentTypesNamespace || document.XMLName.Local != "Types" {
		return pptxContentDeclarations{}, errors.New("PPTX content types have the wrong root element")
	}
	defaults := make(map[string]string, len(document.Defaults))
	for _, contentType := range document.Defaults {
		extension := "." + strings.ToLower(strings.TrimSpace(contentType.Extension))
		if extension == "." || contentType.ContentType == "" {
			return pptxContentDeclarations{}, errors.New("PPTX default content type is incomplete")
		}
		if _, exists := defaults[extension]; exists {
			return pptxContentDeclarations{}, fmt.Errorf("PPTX default content type for %q is duplicated", extension)
		}
		defaults[extension] = contentType.ContentType
	}
	overrides := make(map[string]pptxContentDeclaration, len(document.Overrides))
	for _, contentType := range document.Overrides {
		if contentType.PartName == "" || contentType.ContentType == "" {
			return pptxContentDeclarations{}, errors.New("PPTX content type is incomplete")
		}
		partName, err := canonicalPPTXPartName(contentType.PartName)
		if err != nil {
			return pptxContentDeclarations{}, err
		}
		declaration := pptxContentDeclaration{partName: partName.decoded, contentType: contentType.ContentType}
		for _, key := range partName.keys() {
			if _, exists := overrides[key]; exists {
				return pptxContentDeclarations{}, fmt.Errorf("PPTX content type for %q is duplicated", contentType.PartName)
			}
			overrides[key] = declaration
		}
	}
	return pptxContentDeclarations{defaults: defaults, overrides: overrides}, nil
}

func decodePPTXXML(data []byte, value any) error {
	if err := validatePPTXXMLStructure(data); err != nil {
		return fmt.Errorf("decode PPTX XML: %w", err)
	}
	if err := xml.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode PPTX XML: %w", err)
	}
	return nil
}

func validatePPTXXMLStructure(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth, roots := 0, 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if roots != 1 || depth != 0 {
				return errors.New("PPTX XML must contain one root element")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("read PPTX XML token: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return errors.New("PPTX XML has trailing data")
				}
			}
			depth++
		case xml.EndElement:
			depth--
			if depth < 0 {
				return errors.New("PPTX XML has an unexpected closing element")
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(token)) != "" {
				return errors.New("PPTX XML has unexpected text")
			}
		}
	}
}

func resolvePPTXTargetFrom(sourcePath, target string) (pptxResolvedTarget, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return pptxResolvedTarget{}, errors.New("PPTX relationship target is not an internal path")
	}
	decoded, err := url.PathUnescape(target)
	if err != nil {
		return pptxResolvedTarget{}, errors.New("PPTX relationship target is not a valid URI")
	}
	decoded = strings.TrimSpace(decoded)
	if err := validatePPTXTargetURI(target, decoded); err != nil {
		return pptxResolvedTarget{}, err
	}
	rooted := strings.HasPrefix(target, "/") || strings.HasPrefix(decoded, "/")
	resolveSpelling := func(spelling string) string {
		if rooted {
			spelling = strings.TrimPrefix(spelling, "/")
			if strings.HasPrefix(strings.ToLower(spelling), "%2f") {
				spelling = spelling[3:]
			}
			return spelling
		}
		if sourcePath == "" {
			return spelling
		}
		base := path.Dir(sourcePath)
		if base == "." {
			return spelling
		}
		base = strings.ReplaceAll(base, "%", "%25")
		return base + "/" + spelling
	}
	rawTarget, err := canonicalPPTXPath(resolveSpelling(target))
	if err != nil {
		return pptxResolvedTarget{}, fmt.Errorf("normalize PPTX relationship target: %w", err)
	}
	decodedTarget, err := canonicalPPTXDecodedPath(resolveSpelling(decoded))
	if err != nil {
		return pptxResolvedTarget{}, fmt.Errorf("normalize decoded PPTX relationship target: %w", err)
	}
	keys := rawTarget.keys()
	if !slices.Contains(keys, decodedTarget) {
		keys = append(keys, decodedTarget)
	}
	return pptxResolvedTarget{keys: keys, decoded: decodedTarget}, nil
}

func canonicalPPTXDecodedPath(value string) (string, error) {
	cleaned, err := cleanPPTXPath(value)
	if err != nil {
		return "", err
	}
	return pptxPathKey(cleaned), nil
}

func canonicalPPTXPartName(partName string) (pptxPathAliases, error) {
	if partName == "" {
		return pptxPathAliases{}, errors.New("PPTX part name is not an internal path")
	}
	parsed, err := url.Parse(partName)
	if err != nil {
		return pptxPathAliases{}, errors.New("PPTX part name is not a valid URI")
	}
	if parsed.Scheme != "" || parsed.Host != "" || strings.HasPrefix(partName, "//") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return pptxPathAliases{}, errors.New("PPTX part name is external")
	}
	decoded, err := url.PathUnescape(partName)
	if err != nil {
		return pptxPathAliases{}, errors.New("PPTX part name is not a valid URI")
	}
	if hasPPTXURIPathScheme(decoded) || strings.HasPrefix(decoded, "//") {
		return pptxPathAliases{}, errors.New("PPTX part name is external")
	}
	aliases, err := canonicalPPTXPath(partName)
	if err != nil {
		return pptxPathAliases{}, err
	}
	return aliases, nil
}

func validatePPTXTargetURI(target, decoded string) error {
	target = strings.TrimSpace(target)
	decoded = strings.TrimSpace(decoded)
	if hasPPTXURIPathScheme(decoded) || strings.HasPrefix(decoded, "//") {
		return errors.New("PPTX relationship target is external")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return errors.New("PPTX relationship target is not a valid URI")
	}
	if parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" ||
		strings.HasPrefix(target, "//") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("PPTX relationship target is external")
	}
	return nil
}

func hasPPTXURIPathScheme(value string) bool {
	colon := strings.IndexByte(value, ':')
	if colon <= 0 {
		return false
	}
	prefixEnd := strings.IndexAny(value, "/?#")
	if prefixEnd >= 0 && prefixEnd < colon {
		return false
	}
	if (value[0] < 'A' || value[0] > 'Z') && (value[0] < 'a' || value[0] > 'z') {
		return false
	}
	for _, character := range value[1:colon] {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '+' && character != '-' && character != '.' {
			return false
		}
	}
	return true
}

func countLocalUnits(format CandidateFormat, reader io.ReaderAt, size int64) (int, error) {
	counter := localUnitCounters[format.ID]
	if counter == nil {
		return 0, nil
	}
	return counter(reader, size)
}
