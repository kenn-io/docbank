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
	"strings"
)

type localUnitCounter func(io.ReaderAt, int64) (int, error)

// localUnitCounters is the Mistral-owned authority registry. Only formats
// with provider-authentic unit evidence may be added here.
var localUnitCounters = map[string]localUnitCounter{
	"pptx": countPPTXSlides,
}

const (
	pptxPresentationPath        = "ppt/presentation.xml"
	pptxPresentationRelsPath    = "ppt/_rels/presentation.xml.rels"
	pptxMaxXMLBytes             = int64(1 << 20)
	pptxPresentationNamespace   = "http://schemas.openxmlformats.org/presentationml/2006/main"
	pptxRelationshipNamespace   = "http://schemas.openxmlformats.org/package/2006/relationships"
	pptxRelationshipIDNamespace = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"
	pptxContentTypesNamespace   = "http://schemas.openxmlformats.org/package/2006/content-types"
	pptxMarkupCompatibilityNS   = "http://schemas.openxmlformats.org/markup-compatibility/2006"
	pptxRelationshipType        = "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide"
	pptxSlideContentType        = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml"
)

type pptxRelationship struct {
	ID         string  `xml:"Id,attr"`
	Type       string  `xml:"Type,attr"`
	Target     string  `xml:"Target,attr"`
	TargetMode *string `xml:"TargetMode,attr"`
}

func countPPTXSlides(reader io.ReaderAt, size int64) (int, error) {
	if reader == nil || size <= 0 {
		return 0, errors.New("PPTX source must be non-empty")
	}
	archive, err := zip.NewReader(reader, size)
	if err != nil {
		return 0, fmt.Errorf("open PPTX ZIP: %w", err)
	}
	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		key := pptxPathKey(entry.Name)
		if _, exists := entries[key]; exists {
			return 0, fmt.Errorf("PPTX ZIP contains duplicate entry %q", entry.Name)
		}
		entries[key] = entry
	}

	presentation, err := pptxEntry(entries, pptxPresentationPath)
	if err != nil {
		return 0, err
	}
	presentationRels, err := pptxEntry(entries, pptxPresentationRelsPath)
	if err != nil {
		return 0, err
	}
	contentTypes, err := pptxEntry(entries, ooxmlContentTypesName)
	if err != nil {
		return 0, err
	}
	presentationXML, err := readPPTXXML(presentation)
	if err != nil {
		return 0, err
	}
	relationshipsXML, err := readPPTXXML(presentationRels)
	if err != nil {
		return 0, err
	}
	contentTypesXML, err := readPPTXXML(contentTypes)
	if err != nil {
		return 0, err
	}

	relationshipIDs, err := parsePPTXSlideIDs(presentationXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX presentation: %w", err)
	}
	relationships, err := parsePPTXRelationships(relationshipsXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX presentation relationships: %w", err)
	}
	contentDeclarations, err := parsePPTXContentTypes(contentTypesXML)
	if err != nil {
		return 0, fmt.Errorf("parse PPTX content types: %w", err)
	}

	seenTargets := make(map[string]struct{}, len(relationshipIDs))
	for _, relationshipID := range relationshipIDs {
		relationship, ok := relationships[relationshipID]
		if !ok {
			return 0, fmt.Errorf("PPTX slide relationship %q is unresolved", relationshipID)
		}
		if relationship.Type != pptxRelationshipType {
			return 0, fmt.Errorf("PPTX relationship %q is not a slide", relationshipID)
		}
		if relationship.TargetMode != nil && !strings.EqualFold(*relationship.TargetMode, "Internal") {
			return 0, fmt.Errorf("PPTX slide relationship %q is external", relationshipID)
		}
		target, err := resolvePPTXTarget(relationship.Target)
		if err != nil {
			return 0, fmt.Errorf("resolve PPTX slide relationship %q: %w", relationshipID, err)
		}
		targetKey := pptxPathKey(target)
		if _, exists := seenTargets[targetKey]; exists {
			return 0, fmt.Errorf("PPTX slide relationship target %q is duplicated", target)
		}
		seenTargets[targetKey] = struct{}{}
		entry, ok := entries[targetKey]
		if !ok || entry.FileInfo().IsDir() {
			return 0, fmt.Errorf("PPTX slide target %q is missing", target)
		}
		declaredType, declared := contentDeclarations.forPart(target)
		if !declared || !strings.EqualFold(declaredType, pptxSlideContentType) {
			return 0, fmt.Errorf("PPTX slide target %q has the wrong content type", target)
		}
	}
	return len(relationshipIDs), nil
}

func pptxEntry(entries map[string]*zip.File, name string) (*zip.File, error) {
	entry, ok := entries[pptxPathKey(name)]
	if !ok {
		return nil, fmt.Errorf("PPTX ZIP is missing %q", name)
	}
	return entry, nil
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
	return data, nil
}

type pptxPresentation struct {
	XMLName   xml.Name        `xml:"presentation"`
	SlideList []pptxSlideList `xml:"http://schemas.openxmlformats.org/presentationml/2006/main sldIdLst"`
}

type pptxSlideList struct {
	Slides []pptxSlideID `xml:"http://schemas.openxmlformats.org/presentationml/2006/main sldId"`
}

type pptxSlideID struct {
	ID             string `xml:"id,attr"`
	RelationshipID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
}

type pptxRelationshipDocument struct {
	XMLName       xml.Name           `xml:"Relationships"`
	Relationships []pptxRelationship `xml:"http://schemas.openxmlformats.org/package/2006/relationships Relationship"`
}

type pptxContentTypesDocument struct {
	XMLName   xml.Name          `xml:"Types"`
	Defaults  []pptxDefaultType `xml:"http://schemas.openxmlformats.org/package/2006/content-types Default"`
	Overrides []pptxContentType `xml:"http://schemas.openxmlformats.org/package/2006/content-types Override"`
}

type pptxContentDeclarations struct {
	defaults  map[string]string
	overrides map[string]string
}

func (declarations pptxContentDeclarations) forPart(name string) (string, bool) {
	key := pptxPathKey(name)
	if contentType, ok := declarations.overrides[key]; ok {
		return contentType, true
	}
	contentType, ok := declarations.defaults[strings.ToLower(path.Ext(name))]
	return contentType, ok
}

func pptxPathKey(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, value)
}

type pptxDefaultType struct {
	Extension   string `xml:"Extension,attr"`
	ContentType string `xml:"ContentType,attr"`
}

type pptxContentType struct {
	PartName    string `xml:"PartName,attr"`
	ContentType string `xml:"ContentType,attr"`
}

func (slide *pptxSlideID) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != pptxPresentationNamespace || start.Name.Local != "sldId" {
		return errors.New("PPTX slide list has an unexpected element")
	}
	id, ok, err := pptxAttribute(start.Attr, "", "id")
	if err != nil {
		return err
	}
	if !ok || id == "" {
		return errors.New("PPTX slide ID is missing")
	}
	relationshipID, ok, err := pptxAttribute(start.Attr, pptxRelationshipIDNamespace, "id")
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
	if targetModeSet {
		relationship.TargetMode = &targetMode
	}
	return nil
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

func parsePPTXSlideIDs(data []byte) ([]string, error) {
	if err := rejectPPTXAlternateContent(data); err != nil {
		return nil, err
	}
	var document pptxPresentation
	if err := decodePPTXXML(data, &document); err != nil {
		return nil, err
	}
	if document.XMLName.Space != pptxPresentationNamespace || document.XMLName.Local != "presentation" {
		return nil, errors.New("PPTX presentation has the wrong root element")
	}
	if len(document.SlideList) != 1 || len(document.SlideList[0].Slides) == 0 {
		return nil, errors.New("PPTX presentation has no slides")
	}
	relationshipIDs := make([]string, 0, len(document.SlideList[0].Slides))
	seenRelationshipIDs := make(map[string]struct{}, len(document.SlideList[0].Slides))
	seenSlideIDs := make(map[string]struct{}, len(document.SlideList[0].Slides))
	for _, slide := range document.SlideList[0].Slides {
		if slide.ID == "" {
			return nil, errors.New("PPTX slide ID is missing")
		}
		if slide.RelationshipID == "" {
			return nil, errors.New("PPTX slide relationship ID is missing")
		}
		if _, exists := seenRelationshipIDs[slide.RelationshipID]; exists {
			return nil, fmt.Errorf("PPTX slide relationship %q is duplicated", slide.RelationshipID)
		}
		if _, exists := seenSlideIDs[slide.ID]; exists {
			return nil, fmt.Errorf("PPTX slide ID %q is duplicated", slide.ID)
		}
		seenRelationshipIDs[slide.RelationshipID] = struct{}{}
		seenSlideIDs[slide.ID] = struct{}{}
		relationshipIDs = append(relationshipIDs, slide.RelationshipID)
	}
	return relationshipIDs, nil
}

func rejectPPTXAlternateContent(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read PPTX presentation markup: %w", err)
		}
		start, ok := token.(xml.StartElement)
		if ok && start.Name.Space == pptxMarkupCompatibilityNS && start.Name.Local == "AlternateContent" {
			return errors.New("PPTX presentation uses unsupported alternate slide-list content")
		}
	}
}

func parsePPTXRelationships(data []byte) (map[string]pptxRelationship, error) {
	var document pptxRelationshipDocument
	if err := decodePPTXXML(data, &document); err != nil {
		return nil, err
	}
	if document.XMLName.Space != pptxRelationshipNamespace || document.XMLName.Local != "Relationships" {
		return nil, errors.New("PPTX relationships have the wrong root element")
	}
	relationships := make(map[string]pptxRelationship, len(document.Relationships))
	for _, relationship := range document.Relationships {
		if relationship.ID == "" || relationship.Type == "" || relationship.Target == "" {
			return nil, errors.New("PPTX relationship is incomplete")
		}
		if _, exists := relationships[relationship.ID]; exists {
			return nil, fmt.Errorf("PPTX relationship %q is duplicated", relationship.ID)
		}
		relationships[relationship.ID] = relationship
	}
	return relationships, nil
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
	overrides := make(map[string]string, len(document.Overrides))
	for _, contentType := range document.Overrides {
		if contentType.PartName == "" || contentType.ContentType == "" {
			return pptxContentDeclarations{}, errors.New("PPTX content type is incomplete")
		}
		partName, err := normalizePPTXPartName(contentType.PartName)
		if err != nil {
			return pptxContentDeclarations{}, err
		}
		key := pptxPathKey(partName)
		if _, exists := overrides[key]; exists {
			return pptxContentDeclarations{}, fmt.Errorf("PPTX content type for %q is duplicated", partName)
		}
		overrides[key] = contentType.ContentType
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

func resolvePPTXTarget(target string) (string, error) {
	if target == "" {
		return "", errors.New("PPTX relationship target is not an internal path")
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", errors.New("PPTX relationship target is not a valid URI")
	}
	if parsed.Scheme != "" || parsed.Host != "" || strings.HasPrefix(target, "//") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("PPTX relationship target is external")
	}
	decoded, err := url.PathUnescape(target)
	if err != nil {
		return "", errors.New("PPTX relationship target is not a valid path")
	}
	if strings.ContainsAny(decoded, "\\\x00") {
		return "", errors.New("PPTX relationship target is not an internal path")
	}
	if strings.HasPrefix(target, "/") {
		target, _ = strings.CutPrefix(target, "/")
	} else {
		target = path.Join(path.Dir(pptxPresentationPath), target)
	}
	return normalizePPTXPartName(target)
}

func normalizePPTXPartName(partName string) (string, error) {
	if partName == "" {
		return "", errors.New("PPTX part name is not an internal path")
	}
	parsed, err := url.Parse(partName)
	if err != nil {
		return "", errors.New("PPTX part name is not a valid URI")
	}
	if parsed.Scheme != "" || parsed.Host != "" || strings.HasPrefix(partName, "//") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("PPTX part name is external")
	}
	decoded, err := url.PathUnescape(partName)
	if err != nil {
		return "", errors.New("PPTX part name is not a valid path")
	}
	if strings.ContainsAny(decoded, "\\\x00") {
		return "", errors.New("PPTX part name is not an internal path")
	}
	partName = strings.TrimPrefix(partName, "/")
	cleaned := path.Clean(partName)
	decodedCleaned := path.Clean(strings.TrimPrefix(decoded, "/"))
	if cleaned == "." || path.IsAbs(cleaned) || decodedCleaned == ".." || strings.HasPrefix(decodedCleaned, "../") {
		return "", errors.New("PPTX part name escapes the package root")
	}
	return cleaned, nil
}

func countLocalUnits(format CandidateFormat, reader io.ReaderAt, size int64) (int, error) {
	counter := localUnitCounters[format.ID]
	if counter == nil {
		return 0, nil
	}
	return counter(reader, size)
}
