package processing

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

const teststripXMPNamespace = "https://teststrip.app/xmp/1.0/"
const tiffXMPNamespace = "http://ns.adobe.com/tiff/1.0/"
const sourceMetadataSubjectField = "subject"
const sourceMetadataDescriptionField = "description"
const sourceMetadataRightsField = "rights"

const photoshopXMPNamespace = "http://ns.adobe.com/photoshop/1.0/"

// photoXMPFlag reads an XMP rating the same way for import and export. XMP
// types Rating as a real number, so "3.0" is a valid three-star rating; -1 is
// the reject marker and overrides pick.
func photoXMPFlag(rating, pick string) (flag string, n int, valid bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(rating), 64)
	if err != nil || value != math.Trunc(value) || value < -1 || value > 5 {
		return strings.TrimSpace(pick), 0, false
	}
	n = int(value)
	if n == -1 {
		return "reject", n, true
	}
	return strings.TrimSpace(pick), n, true
}

// photoXMPField names the authored XMP properties shared by sidecar import and export.
func photoXMPField(n xml.Name) string {
	if property := photoXMPPropertyFor(n); property.bit != 0 && property.name != sourceMetadataSubjectField && property.name != "Orientation" {
		return property.name
	}
	return ""
}

// boundedSidecarText buffers one property's text up to the authored text
// limit. An oversized value reads as absent instead of as a truncated prefix,
// so "3" followed by padding and other text cannot become a three-star rating.
// It tracks non-whitespace text separately, so an oversized prefix still counts
// as text beside a property's value.
type boundedSidecarText struct {
	builder    strings.Builder
	overflow   bool
	content    bool
	unreadable bool
}

type photoSidecarValue struct {
	text       string
	unreadable bool
}

func photoSidecarResourceAttribute(a xml.Attr) bool {
	return a.Name.Space == rdfNamespace && (a.Name.Local == "resource" || a.Name.Local == "nodeID")
}

func (t *boundedSidecarText) write(data []byte) {
	t.content = t.content || len(bytes.TrimSpace(data)) > 0
	if t.overflow || t.builder.Len()+len(data) > store.MaxPhotoAuthoredTextBytes {
		t.overflow = true
		return
	}
	t.builder.Write(data)
}

func (t *boundedSidecarText) reset() {
	*t = boundedSidecarText{}
}

func (t *boundedSidecarText) value() photoSidecarValue {
	if t.overflow {
		return photoSidecarValue{unreadable: t.unreadable}
	}
	return photoSidecarValue{t.builder.String(), t.unreadable}
}

type photoSidecarPacket struct {
	store.PhotoXMPPacket

	SidecarForExtension string
	extracted           store.PhotoAuthored
	refusal             error
	tokens              []xml.Token
	descriptions        map[int]bool
	properties          map[int]int
	firstDescription    int
}

// rewrite handles the property when it is an empty creator, which clears it, or
// an Alt or Seq that an active description holds. It edits the held alternatives
// or items and keeps the rest, at the point the caller writes, so the property
// moves into the first description.
func (p photoSidecarPacket) rewrite(emit func(xml.Token) error, prop photoXMPProperty, value string, keywords []string) (bool, error) {
	if prop.collection == "Seq" && value == "" {
		return true, nil
	}
	start, end, ok := 0, 0, false
	for s, e := range p.properties {
		if t, _ := p.tokens[s].(xml.StartElement); t.Name == (xml.Name{Space: prop.space, Local: prop.name}) && (!ok || s < start) {
			start, end, ok = s, e, true
		}
	}
	if !ok || prop.collection != photoXMPAlt && prop.collection != "Seq" {
		return false, nil
	}
	return true, p.rewriteCollection(emit, start, end, prop.collection, value, keywords)
}

// rewriteCollection keeps the language of a relocated property.
func (p photoSidecarPacket) rewriteCollection(emit func(xml.Token) error, start, end int, collection, value string, keywords []string) error {
	return editPhotoXMPCollection(emit, p.withLanguage(start), p.tokens[start:end], collection, value, keywords, false)
}

// withLanguage returns the property element at index start carrying the xml:lang it
// inherits there, when the first description would give its items another one.
func (p photoSidecarPacket) withLanguage(start int) xml.StartElement {
	held, _ := p.tokens[start].(xml.StartElement)
	lang := xml.Name{Space: xmlNamespace, Local: "lang"}
	if source := p.langAt(start); source != p.langAt(p.firstDescription) && !slices.ContainsFunc(held.Attr, func(a xml.Attr) bool { return a.Name == lang }) {
		held.Attr = append(slices.Clone(held.Attr), xml.Attr{Name: lang, Value: source})
	}
	return held
}

// langAt is the xml:lang in scope for the token at index at.
func (p photoSidecarPacket) langAt(at int) string {
	lang, scopes := "", []string{}
	for _, token := range p.tokens[:at] {
		switch t := token.(type) {
		case xml.StartElement:
			scopes = append(scopes, lang)
			for _, a := range t.Attr {
				if a.Name == (xml.Name{Space: xmlNamespace, Local: "lang"}) {
					lang = a.Value
				}
			}
		case xml.EndElement:
			lang, scopes = scopes[len(scopes)-1], scopes[:len(scopes)-1]
		}
	}
	return lang
}

type photoSidecarReadMode uint8

const (
	photoSidecarExtraction photoSidecarReadMode = iota
	photoSidecarEditing
)

func readPhotoSidecar(ctx context.Context, data []byte, mode photoSidecarReadMode) (photoSidecarPacket, error) {
	result := photoSidecarPacket{Keywords: []string{}}
	if mode == photoSidecarEditing {
		if len(data) > maxPhotoSidecarBytes {
			result.refusal = errors.New("photo sidecar exceeds sync limit")
			return result, nil
		}
		result.descriptions, result.properties = map[int]bool{}, map[int]int{}
	}
	if len(data) > maxSourceMetadataOriginalBytes || !utf8.Valid(data) {
		return result, errors.New("invalid photo sidecar bytes")
	}
	decoder := newPhotoXMLDecoder(data)
	stack := []xml.Name{}
	rootCount := 0
	rdfCount := 0
	description := false
	var subject photoXMPSubject
	activeDescription := false
	values := map[string]string{}
	rawValues := map[string]string{}
	var propertyStart int
	var syncDefault photoSidecarValue
	var hasSyncDefault bool
	var field string
	var fieldDepth int
	var child xml.Name
	var structured bool
	var text boundedSidecarText
	var items []photoSidecarValue
	var itemText boundedSidecarText
	var itemDepth int
	var itemDefault bool
	var itemSyncDefault bool
	var defaultText photoSidecarValue
	var hasDefault bool
	fieldFor := func(n xml.Name) string {
		if n == (xml.Name{Space: photoshopXMPNamespace, Local: "SidecarForExtension"}) {
			return n.Local
		}
		return photoXMPField(n)
	}
	save := func(key, value string) error {
		if _, exists := values[key]; exists {
			return fmt.Errorf("duplicate XMP property %s", key)
		}
		rawValues[key] = value
		if strings.TrimSpace(value) == "" {
			value = ""
		} else if key != sourceMetadataDescriptionField && key != sourceMetadataCreatorField && key != sourceMetadataRightsField {
			value = strings.TrimSpace(value)
		}
		values[key] = value
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, fmt.Errorf("invalid photo sidecar XML: %w", err)
		}
		i := len(result.tokens)
		if mode == photoSidecarEditing {
			result.tokens = append(result.tokens, xml.CopyToken(token))
			// Adobe and Teststrip never write xml:base; refusing it keeps every subject and resource target unresolved text.
			if t, ok := token.(xml.StartElement); ok && slices.ContainsFunc(t.Attr, func(a xml.Attr) bool { return a.Name == (xml.Name{Space: xmlNamespace, Local: "base"}) }) {
				result.refusal = errors.New("XMP xml:base is unsupported; remove it to sync")
			}
		}
		switch t := token.(type) {
		case xml.StartElement:
			if len(stack) >= maxSourceMetadataXMLDepth {
				return result, errors.New("photo sidecar exceeds XML depth")
			}
			if len(stack) == 0 {
				rootCount++
				if !isXMPRoot(t.Name) {
					return result, errors.New("photo sidecar needs an XMP root")
				}
				if rootCount > 1 {
					return result, errors.New("photo sidecar has multiple roots")
				}
			}
			if t.Name == (xml.Name{Space: rdfNamespace, Local: "RDF"}) {
				rdfCount++
				if rdfCount > 1 {
					result.refusal = errors.New("multiple XMP RDF roots")
				}
			}
			parent := xml.Name{}
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, t.Name)
			if field != "" && len(stack) > fieldDepth {
				valid := t.Name.Space == rdfNamespace
				if len(stack) == fieldDepth+1 {
					if child != (xml.Name{}) || text.content {
						return result, errors.New("multiple authored RDF values")
					}
					child = t.Name
					text.reset()
					valid = valid && (t.Name.Local == "value" || field == sourceMetadataCreatorField && t.Name.Local == "Seq" || (field == sourceMetadataDescriptionField || field == sourceMetadataRightsField) && t.Name.Local == "Alt")
				} else {
					valid = valid && len(stack) == fieldDepth+2 && t.Name.Local == "li" && parent.Space == rdfNamespace && (parent.Local == "Alt" || parent.Local == "Seq")
				}
				if !valid {
					return result, errors.New("unsupported authored RDF value")
				}
			}
			if len(stack) == 3 && t.Name.Space == rdfNamespace && t.Name.Local == "Description" && parent.Space == rdfNamespace && parent.Local == "RDF" {
				current := readPhotoXMPSubject(t.Attr)
				if !description {
					description = true
					subject = current
				}
				activeDescription = current == subject
				if activeDescription {
					if mode == photoSidecarEditing {
						result.descriptions[i] = true
					}
					if current.kind == "ID" {
						result.refusal = errors.New("XMP rdf:ID subjects cannot be patched")
					}
				}
				for _, attr := range t.Attr {
					if activeDescription && (attr.Name == (xml.Name{Space: tiffXMPNamespace, Local: "Orientation"}) || attr.Name == (xml.Name{Space: xmpDublinCoreNamespace, Local: "subject"})) {
						if _, exists := rawValues[attr.Name.Local]; exists {
							result.refusal = errors.New("duplicate XMP property")
						}
						rawValues[attr.Name.Local] = attr.Value
					}
					if key := fieldFor(attr.Name); activeDescription && key != "" {
						if err := save(key, attr.Value); err != nil {
							return result, err
						}
					}
				}
			}
			if activeDescription && len(stack) == 4 && parent.Space == rdfNamespace && parent.Local == "Description" {
				propertyStart = i
				if key := fieldFor(t.Name); key != "" {
					if field != "" {
						return result, errors.New("nested authored XMP property")
					}
					field = key
					fieldDepth = len(stack)
					child = xml.Name{}
					structured = false
					text.reset()
					items = nil
					hasDefault = false
					defaultText = photoSidecarValue{}
					hasSyncDefault = false
				}
			}
			if field != "" {
				for _, a := range t.Attr {
					if a.Name == (xml.Name{Space: rdfNamespace, Local: "parseType"}) {
						structured = true
					}
					if photoSidecarResourceAttribute(a) && t.Name != (xml.Name{Space: rdfNamespace, Local: "li"}) {
						text.unreadable = true
					}
				}
			}
			if field != "" && t.Name.Space == rdfNamespace && t.Name.Local == "li" {
				if itemDepth != 0 {
					return result, errors.New("nested RDF item")
				}
				itemDepth = len(stack)
				itemText.reset()
				itemText.unreadable = slices.ContainsFunc(t.Attr, photoSidecarResourceAttribute)
				itemDefault = false
				itemSyncDefault = false
				for _, a := range t.Attr {
					if a.Name.Space == xmlNamespace && a.Name.Local == "lang" {
						itemDefault = a.Value == "x-default"
						itemSyncDefault = strings.EqualFold(a.Value, "x-default")
					}
				}
			}
		case xml.CharData:
			if len(stack) == 0 && strings.TrimSpace(string(t)) != "" {
				return result, errors.New("text outside photo sidecar root")
			}
			if field != "" {
				target := &text
				if itemDepth != 0 {
					target = &itemText
				}
				beside := itemDepth == 0 && child != (xml.Name{}) && (len(stack) != fieldDepth+1 || child.Local != "value")
				if beside && strings.TrimSpace(string(t)) != "" {
					return result, errors.New("text beside authored RDF value")
				}
				if !beside {
					target.write(t)
				}
			}
		case xml.EndElement:
			if mode == photoSidecarEditing && activeDescription && len(stack) == 4 {
				result.properties[propertyStart] = i + 1
			}
			if mode == photoSidecarEditing && activeDescription && len(stack) == 3 && result.firstDescription == 0 {
				result.firstDescription = i
			}
			if text.overflow || itemText.overflow {
				result.refusal = errors.New("XMP property exceeds text limit")
			}
			if len(stack) == 3 {
				activeDescription = false
			}
			if itemDepth == len(stack) && itemDepth != 0 {
				value := itemText.value()
				if mode == photoSidecarEditing || len(items) == 0 {
					items = append(items, value)
				}
				if itemSyncDefault {
					if hasSyncDefault {
						result.refusal = errors.New("duplicate default RDF text")
					}
					hasSyncDefault = true
					syncDefault = value
				}
				if itemDefault {
					if hasDefault {
						return result, errors.New("duplicate default RDF text")
					}
					hasDefault = true
					defaultText = value
				}
				itemDepth = 0
			}
			if field != "" && fieldDepth == len(stack) {
				if structured && child == (xml.Name{}) {
					result.refusal = errors.New("structured authored RDF property has no supported literal")
				}
				value := text.value()
				if len(items) > 0 {
					if field == sourceMetadataCreatorField {
						value = items[0]
					} else if hasDefault {
						value = defaultText
					} else {
						value = items[0]
					}
				}
				if err := save(field, value.text); err != nil {
					return result, err
				}
				if hasSyncDefault && field != sourceMetadataCreatorField {
					rawValues[field] = syncDefault.text
					value = syncDefault
				}
				if text.unreadable || value.unreadable {
					result.Ignored |= photoXMPPropertyFor(stack[fieldDepth-1]).bit
				}
				field = ""
			}
			stack = stack[:len(stack)-1]
		case xml.Directive:
			return result, errors.New("XML directives are unsupported in photo sidecars")
		}
	}
	if rootCount != 1 || len(stack) != 0 || !description {
		return result, errors.New("photo sidecar needs an RDF description")
	}
	result.SidecarForExtension = strings.ToLower(values["SidecarForExtension"])
	result.Flag = values["Pick"]
	if !query.ValidPhotoFlag(result.Flag) {
		result.Flag = ""
		result.Ignored |= store.PhotoConfirmedFlag
	}
	result.Label = strings.ToLower(values["Label"])
	if !query.ValidPhotoColorLabel(result.Label) {
		result.Label = ""
		result.Ignored |= store.PhotoConfirmedLabel
	}
	result.Caption = values[sourceMetadataDescriptionField]
	result.Creator = values[sourceMetadataCreatorField]
	result.Copyright = values[sourceMetadataRightsField]
	for _, text := range []*string{&result.Caption, &result.Creator, &result.Copyright} {
		if len(*text) > store.MaxPhotoAuthoredTextBytes {
			*text = ""
		}
	}
	if flag, n, valid := photoXMPFlag(values["Rating"], result.Flag); valid {
		if n == -1 {
			result.LegacyReject = true
			result.Ignored &^= store.PhotoConfirmedFlag
			result.Flag = flag
		} else {
			result.Rating = n
		}
	} else if rating, exists := values["Rating"]; exists && rating != "" {
		result.Ignored |= store.PhotoConfirmedRating
	}
	normalizePhotoXMPRotation(values)
	readPhotoXMPRotation(&result, values)
	result.extracted = result.PhotoAuthored
	if mode == photoSidecarExtraction {
		return result, store.ValidatePhotoAuthoredValues(result.extracted)
	}
	result.Caption, result.Creator, result.Copyright = rawValues[sourceMetadataDescriptionField], rawValues[sourceMetadataCreatorField], rawValues[sourceMetadataRightsField]
	if result.LegacyReject {
		result.Ignored |= store.PhotoConfirmedRating
		result.Present |= store.PhotoConfirmedFlag
	}
	for start, end := range result.properties {
		t, _ := result.tokens[start].(xml.StartElement)
		if t.Name == (xml.Name{Space: xmpDublinCoreNamespace, Local: "subject"}) || t.Name == (xml.Name{Space: tiffXMPNamespace, Local: "Orientation"}) {
			if _, exists := rawValues[t.Name.Local]; exists {
				result.refusal = errors.New("duplicate XMP property")
			}
			items, err := photoXMPExtraValues(result.tokens[start:end])
			if err != nil {
				result.refusal = err
			} else if t.Name.Local == "subject" {
				result.Keywords = items
				rawValues["subject"] = ""
			} else {
				rawValues[t.Name.Local] = items[0]
			}
		}
	}
	normalizePhotoXMPRotation(rawValues)
	for _, property := range photoXMPProperties {
		if _, exists := rawValues[property.name]; exists {
			result.Present |= property.bit
		}
	}
	if value, exists := rawValues["Orientation"]; exists {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || n < 1 || n > 8 {
			result.Ignored |= store.PhotoConfirmedRotation
		} else {
			result.Orientation = n
		}
	}
	if value := rawValues["subject"]; value != "" {
		result.Keywords = append(result.Keywords, value)
	}
	result.Keywords = slices.DeleteFunc(result.Keywords, func(s string) bool { return s == "" })
	for i, value := range result.Keywords {
		name, err := store.NormalizeTagName(value)
		if err != nil {
			result.refusal = err
		}
		result.Keywords[i] = name
	}
	result.Keywords = store.PhotoXMPKeywords(result.Keywords)
	result.refusal = errors.Join(result.refusal, store.ValidatePhotoXMPKeywords(result.Keywords))
	if err := store.ValidatePhotoAuthoredValues(result.PhotoAuthored); err != nil {
		result.refusal = err
		result.PhotoAuthored = result.extracted
	}
	return result, nil
}

// Empty rotation properties carry the same removal evidence as absent ones.
func normalizePhotoXMPRotation(values map[string]string) {
	for _, key := range []string{"Rotation", "Orientation"} {
		if strings.TrimSpace(values[key]) == "" {
			delete(values, key)
		}
	}
}

func readPhotoXMPRotation(p *photoSidecarPacket, values map[string]string) {
	if value, ok := values["Rotation"]; ok {
		n, err := strconv.Atoi(value)
		if err == nil && (n == 0 || n == 90 || n == 180 || n == 270) {
			p.PhotoAuthored.Rotation, p.Rotation = n, new(n)
		} else {
			p.Ignored |= store.PhotoConfirmedRotation
		}
	}
}

// photoXMPExtraValues reads sync-only properties without changing v20 extraction.
func photoXMPExtraValues(tokens []xml.Token) ([]string, error) {
	refusal := errors.New("unsupported XMP RDF value; replace rdf:resource with literal text, or use rdf:Bag with rdf:li items for keywords")
	values := []string{}
	depth := 0
	var text strings.Builder
	collection := false
	for _, token := range tokens {
		switch t := token.(type) {
		case xml.StartElement:
			depth++
			for _, a := range t.Attr {
				if a.Name.Space == rdfNamespace {
					return nil, refusal
				}
			}
			if depth == 2 {
				if collection {
					return nil, refusal
				}
				root, _ := tokens[0].(xml.StartElement)
				collection = root.Name == (xml.Name{Space: xmpDublinCoreNamespace, Local: sourceMetadataSubjectField}) && t.Name == (xml.Name{Space: rdfNamespace, Local: "Bag"})
				if !collection || strings.TrimSpace(text.String()) != "" {
					return nil, refusal
				}
				text.Reset()
			}
			if depth > 3 || depth == 3 && t.Name != (xml.Name{Space: rdfNamespace, Local: "li"}) {
				return nil, refusal
			}
		case xml.CharData:
			if depth == 1 && !collection || depth == 3 {
				text.Write(t)
			} else if len(bytes.TrimSpace(t)) > 0 {
				return nil, refusal
			}
		case xml.EndElement:
			if depth == 3 || depth == 1 && !collection {
				values = append(values, text.String())
				text.Reset()
			}
			depth--
		}
	}
	if len(values) == 0 {
		values = []string{""}
	}
	return values, nil
}
