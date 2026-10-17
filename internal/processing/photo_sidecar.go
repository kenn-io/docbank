package processing

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

const sourceMetadataRightsField = "rights"

const sourceMetadataDescriptionField = "description"
const sourceMetadataSubjectField = "subject"

const teststripXMPNamespace = "https://teststrip.app/xmp/1.0/"
const photoshopXMPNamespace = "http://ns.adobe.com/photoshop/1.0/"

// photoXMPFlag reads an XMP rating the same way for import and export. XMP
// types Rating as a real number, so "3.0" is a valid three-star rating; -1 is
// the legacy reject marker and overrides pick.
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

// boundedSidecarText records overflow so the reader refuses the whole packet.
type boundedSidecarText struct {
	builder  strings.Builder
	overflow bool
	content  bool
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
	t.builder.Reset()
	t.overflow = false
	t.content = false
}

func (t *boundedSidecarText) value() string {
	if t.overflow {
		return ""
	}
	return t.builder.String()
}

type photoSidecarPacket struct {
	store.PhotoAuthored

	Refusal             error
	SidecarForExtension string
	Keywords            []string
	KeywordsIgnored     bool
	LegacyReject        bool
	RotationPresent     bool
	Ignored             store.PhotoAuthoredFields
	Orientation         int
}

var errPhotoSidecarPropertyLimit = errors.New("XMP property exceeds its text limit")

func readPhotoSidecar(ctx context.Context, data []byte) (photoSidecarPacket, error) {
	var result photoSidecarPacket
	if len(data) > maxSourceMetadataOriginalBytes || !utf8.Valid(data) {
		return result, errors.New("invalid photo sidecar bytes")
	}
	if len(data) > maxPhotoSidecarBytes {
		result.Refusal = errors.New("photo sidecar exceeds sync limit")
	}
	decoder := newPhotoXMLDecoder(data)
	stack := []xml.Name{}
	rootCount := 0
	rdfCount := 0
	description := false
	var subjectKind, subjectValue string
	activeDescription := false
	values := map[string]string{}
	var field string
	var fieldDepth int
	var unsupported bool
	ignored := map[string]bool{}
	var child xml.Name
	var text boundedSidecarText
	var items []string
	var itemText boundedSidecarText
	var itemDepth int
	var itemDefault bool
	var defaultText string
	var hasDefault bool
	fieldFor := func(n xml.Name) string {
		if n == (xml.Name{Space: photoshopXMPNamespace, Local: "SidecarForExtension"}) {
			return n.Local
		}
		field := photoXMPPropertyFor(n).name
		return field
	}
	save := func(key, value string) error {
		if len(value) > store.MaxPhotoAuthoredTextBytes {
			result.Refusal = errPhotoSidecarPropertyLimit
			value = ""
		}
		if _, exists := values[key]; exists {
			return fmt.Errorf("duplicate XMP property %s", key)
		}
		if strings.TrimSpace(value) == "" {
			value = ""
		} else if key != sourceMetadataDescriptionField && key != sourceMetadataCreatorField && key != sourceMetadataRightsField && key != sourceMetadataSubjectField {
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
			parent := xml.Name{}
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			if t.Name == (xml.Name{Space: rdfNamespace, Local: "RDF"}) {
				rdfCount++
				if rdfCount > 1 {
					result.Refusal = errors.New("multiple XMP RDF roots")
				}
			}
			subject := photoXMPSubject{}
			if t.Name == (xml.Name{Space: rdfNamespace, Local: photoXMPRDFDescription}) {
				var err error
				subject, err = readPhotoXMPSubject(t.Attr)
				if err != nil {
					result.Refusal = err
				}
			}
			if t.Name == (xml.Name{Space: "http://ns.adobe.com/xmp/note/", Local: "HasExtendedXMP"}) {
				result.Refusal = errPhotoExtendedXMP
			}
			for _, attr := range t.Attr {
				if attr.Name == (xml.Name{Space: "http://ns.adobe.com/xmp/note/", Local: "HasExtendedXMP"}) {
					result.Refusal = errPhotoExtendedXMP
				}
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
					valid = valid && (t.Name.Local == "value" || field == sourceMetadataSubjectField && t.Name.Local == "Bag" || field == sourceMetadataCreatorField && t.Name.Local == "Seq" || (field == sourceMetadataDescriptionField || field == sourceMetadataRightsField) && t.Name.Local == "Alt")
				} else {
					valid = valid && len(stack) == fieldDepth+2 && t.Name.Local == "li" && parent.Space == rdfNamespace && (parent.Local == "Alt" || parent.Local == "Seq" || parent.Local == "Bag")
				}
				if !valid {
					if field != sourceMetadataSubjectField {
						return result, errors.New("unsupported authored RDF value")
					}
					unsupported = true
				}
			}
			if len(stack) == 3 && t.Name.Space == rdfNamespace && t.Name.Local == photoXMPRDFDescription && parent.Space == rdfNamespace && parent.Local == "RDF" {
				kind, value := subject.kind, subject.value
				if !description {
					description = true
					subjectKind, subjectValue = kind, value
				}
				activeDescription = kind == subjectKind && value == subjectValue
				for _, attr := range t.Attr {
					if key := fieldFor(attr.Name); activeDescription && key != "" {
						if err := save(key, attr.Value); err != nil {
							return result, err
						}
					}
				}
			}
			if activeDescription && len(stack) == 4 && parent.Space == rdfNamespace && parent.Local == photoXMPRDFDescription {
				if key := fieldFor(t.Name); key != "" {
					if field != "" {
						return result, errors.New("nested authored XMP property")
					}
					field = key
					unsupported = false
					fieldDepth = len(stack)
					child = xml.Name{}
					text.reset()
					items = nil
					hasDefault = false
					defaultText = ""
				}
			}
			if field != "" {
				for _, attr := range t.Attr {
					if attr.Name == (xml.Name{Space: rdfNamespace, Local: "resource"}) {
						unsupported = true
					}
				}
			}
			if field != "" && t.Name.Space == rdfNamespace && t.Name.Local == "li" {
				if itemDepth != 0 {
					return result, errors.New("nested RDF item")
				}
				itemDepth = len(stack)
				itemText.reset()
				itemDefault = false
				for _, a := range t.Attr {
					if a.Name.Space == xmlNamespace && a.Name.Local == "lang" && a.Value == "x-default" {
						itemDefault = true
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
			if text.overflow || itemText.overflow {
				result.Refusal = errPhotoSidecarPropertyLimit
			}
			if len(stack) == 3 {
				activeDescription = false
			}
			if itemDepth == len(stack) && itemDepth != 0 {
				value := itemText.value()
				items = append(items, value)
				if itemDefault {
					if hasDefault {
						return result, errors.New("duplicate default RDF text")
					}
					hasDefault = true
					defaultText = value
				}
				itemDepth = 0
			}
			if field != "" && fieldDepth == len(stack) && unsupported {
				ignored[field] = true
			}
			if field != "" && fieldDepth == len(stack) {
				value := text.value()
				if field == sourceMetadataSubjectField {
					result.Keywords = append(result.Keywords, items...)
					if len(items) == 0 && value != "" {
						result.Keywords = append(result.Keywords, value)
					}
				}
				if len(items) > 0 {
					if field == sourceMetadataCreatorField {
						value = items[0]
					} else if hasDefault {
						value = defaultText
					} else {
						value = items[0]
					}
				}
				if err := save(field, value); err != nil {
					return result, err
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
	if result.Keywords == nil && values[sourceMetadataSubjectField] != "" {
		result.Keywords = []string{values[sourceMetadataSubjectField]}
	}
	if ignored[sourceMetadataSubjectField] {
		result.Keywords = nil
		delete(values, sourceMetadataSubjectField)
	}
	keywords := result.Keywords[:0]
	for _, keyword := range result.Keywords {
		if keyword == "" {
			continue
		}
		normalized, err := store.NormalizeTagName(keyword)
		if err != nil {
			result.Refusal = err
			continue
		}
		keywords = append(keywords, normalized)
	}
	result.Keywords = store.PhotoXMPKeywords(keywords)
	if err := store.ValidatePhotoXMPKeywords(result.Keywords); err != nil {
		result.Refusal = errors.New("XMP keywords exceed the metadata value limit")
		result.Keywords = []string{}
	}
	result.KeywordsIgnored = ignored[sourceMetadataSubjectField]
	if value := values["Orientation"]; value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 8 {
			result.Ignored |= store.PhotoConfirmedRotation
			delete(values, "Orientation")
		} else {
			result.Orientation = n
		}
	}
	result.Flag = values["Pick"]
	if !query.ValidPhotoFlag(result.Flag) {
		result.Flag = ""
		result.Ignored |= store.PhotoConfirmedFlag
		delete(values, "Pick")
	}
	result.Label = strings.ToLower(values["Label"])
	if !query.ValidPhotoColorLabel(result.Label) {
		result.Label = ""
		result.Ignored |= store.PhotoConfirmedLabel
		delete(values, "Label")
	}
	result.Caption = values[sourceMetadataDescriptionField]
	result.Creator = values[sourceMetadataCreatorField]
	result.Copyright = values[sourceMetadataRightsField]
	if value, present := values["Rating"]; present {
		flag, n, valid := photoXMPFlag(value, result.Flag)
		if !valid {
			result.Ignored |= store.PhotoConfirmedRating
			delete(values, "Rating")
		} else if n == -1 {
			result.LegacyReject = true
			result.Flag = flag
			result.Confirmed |= store.PhotoConfirmedFlag
			result.Ignored &^= store.PhotoConfirmedFlag
			delete(ignored, "Pick")
			delete(values, "Rating")
		} else {
			result.Rating = n
		}
	}
	if value, ok := values["Rotation"]; ok {
		n, err := strconv.Atoi(value)
		if err != nil || (n != 0 && n != 90 && n != 180 && n != 270) {
			result.Ignored |= store.PhotoConfirmedRotation
			delete(values, "Rotation")
		} else {
			result.Rotation = n
		}
	}
	_, result.RotationPresent = values["Rotation"]
	for _, property := range &photoXMPProperties {
		key, bit := property.name, property.bit
		if bit == store.PhotoXMPKeywordsBit {
			continue
		}
		if ignored[key] {
			result.Ignored |= bit
		}
		if _, present := values[key]; present && !ignored[key] {
			result.Confirmed |= bit
		}
	}
	if result.RotationPresent {
		result.Confirmed |= store.PhotoConfirmedRotation
	}
	result.Confirmed &= store.PhotoAuthoredNonDefault(result.PhotoAuthored)
	if result.Orientation != 0 || result.RotationPresent {
		result.Confirmed |= store.PhotoConfirmedRotation
	}
	if err := store.ValidatePhotoAuthoredValues(result.PhotoAuthored); err != nil {
		return result, err
	}
	return result, nil
}
