package processing

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.kenn.io/docbank/internal/query"
	"go.kenn.io/docbank/internal/store"
)

const maxPhotoSidecarBytes = 1 << 20
const teststripXMPNamespace = "https://teststrip.app/xmp/1.0/"

var photoXMPProperties = map[string]string{
	"rating": "Rating", "flag": "Pick", "label": "Label", "caption": "description", "creator": "creator", "copyright": "rights", "rotation": "Rotation",
}

// ReadPhotoSidecar validates the complete packet before returning supported decisions.
func ReadPhotoSidecar(ctx context.Context, data []byte) (store.PhotoAuthored, error) {
	var result store.PhotoAuthored
	if len(data) > maxPhotoSidecarBytes || !utf8.Valid(data) {
		return result, errors.New("invalid photo sidecar bytes")
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	stack := []xml.Name{}
	rootCount := 0
	description := false
	values := map[string]string{}
	var field string
	var fieldDepth int
	var child xml.Name
	var text strings.Builder
	var items []string
	var itemText strings.Builder
	var itemDepth int
	var itemDefault bool
	var defaultText string
	var hasDefault bool
	fieldFor := func(n xml.Name) string {
		switch n.Space {
		case xmpBasicNamespace:
			if n.Local == "Rating" || n.Local == "Label" {
				return n.Local
			}
		case teststripXMPNamespace:
			if n.Local == "Pick" || n.Local == "Rotation" {
				return n.Local
			}
		case xmpDublinCoreNamespace:
			if n.Local == "description" || n.Local == "creator" || n.Local == "rights" {
				return n.Local
			}
		}
		return ""
	}
	save := func(key, value string) error {
		if _, exists := values[key]; exists {
			return fmt.Errorf("duplicate XMP property %s", key)
		}
		if strings.TrimSpace(value) == "" {
			value = ""
		} else if key != "description" && key != "creator" && key != "rights" {
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
			stack = append(stack, t.Name)
			attributes := map[xml.Name]bool{}
			for _, attr := range t.Attr {
				if attributes[attr.Name] {
					return result, errors.New("duplicate XML attribute")
				}
				attributes[attr.Name] = true
			}
			if field != "" && len(stack) > fieldDepth {
				valid := t.Name.Space == rdfNamespace
				if len(stack) == fieldDepth+1 {
					if child != (xml.Name{}) || strings.TrimSpace(text.String()) != "" {
						return result, errors.New("multiple authored RDF values")
					}
					child = t.Name
					text.Reset()
					valid = valid && (t.Name.Local == "value" || field == "creator" && t.Name.Local == "Seq" || (field == "description" || field == "rights") && t.Name.Local == "Alt")
				} else {
					valid = valid && len(stack) == fieldDepth+2 && t.Name.Local == "li" && parent.Space == rdfNamespace && (parent.Local == "Alt" || parent.Local == "Seq")
				}
				if !valid {
					return result, errors.New("unsupported authored RDF value")
				}
			}
			if t.Name.Space == rdfNamespace && t.Name.Local == "Description" && parent.Space == rdfNamespace && parent.Local == "RDF" {
				description = true
				for _, attr := range t.Attr {
					if key := fieldFor(attr.Name); key != "" {
						if err := save(key, attr.Value); err != nil {
							return result, err
						}
					}
				}
			}
			if parent.Space == rdfNamespace && parent.Local == "Description" && len(stack) >= 3 && stack[len(stack)-3] == (xml.Name{Space: rdfNamespace, Local: "RDF"}) {
				if key := fieldFor(t.Name); key != "" {
					if field != "" {
						return result, errors.New("nested authored XMP property")
					}
					field = key
					fieldDepth = len(stack)
					child = xml.Name{}
					text.Reset()
					items = nil
					hasDefault = false
					defaultText = ""
				}
			}
			if field != "" {
				for _, attr := range t.Attr {
					if attr.Name.Space == rdfNamespace {
						return result, errors.New("unsupported authored RDF value")
					}
				}
			}
			if field != "" && t.Name.Space == rdfNamespace && t.Name.Local == "li" {
				if itemDepth != 0 {
					return result, errors.New("nested RDF item")
				}
				itemDepth = len(stack)
				itemText.Reset()
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
				if itemDepth != 0 {
					itemText.Write(t)
				} else if child == (xml.Name{}) || len(stack) == fieldDepth+1 && child.Local == "value" {
					text.Write(t)
				} else if strings.TrimSpace(string(t)) != "" {
					return result, errors.New("text beside authored RDF value")
				}
			}
		case xml.EndElement:
			if itemDepth == len(stack) && itemDepth != 0 {
				value := itemText.String()
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
			if field != "" && fieldDepth == len(stack) {
				value := text.String()
				if len(items) > 0 {
					if field == "creator" {
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
	for _, field := range store.PhotoAuthoredFieldTable {
		if field.Name == "rating" || field.Name == "rotation" {
			continue
		}
		if value, present := values[photoXMPProperties[field.Name]]; present {
			if !field.Set(&result, value) {
				return result, errors.New("invalid authored XMP field")
			}
			result.Confirmed |= field.Bit
		}
	}
	if !query.ValidPhotoFlag(result.Flag) {
		return result, errors.New("invalid XMP pick")
	}
	result.Label = strings.ToLower(result.Label)
	if !query.ValidPhotoColorLabel(result.Label) {
		result.Label = ""
		result.Confirmed &^= store.PhotoConfirmedLabel
	}
	if value, ok := values["Rating"]; ok {
		n, err := strconv.Atoi(value)
		if err != nil || n < -1 || n > 5 {
			return result, errors.New("invalid XMP rating")
		}
		if n == -1 {
			result.Flag = "reject"
			result.Confirmed |= store.PhotoConfirmedFlag
		} else if n != 0 {
			result.Rating = n
			result.Confirmed |= store.PhotoConfirmedRating
		}
	}
	if value, ok := values["Rotation"]; ok {
		n, err := strconv.Atoi(value)
		if err == nil && (n == 0 || n == 90 || n == 180 || n == 270) {
			result.Rotation = n
			result.Confirmed |= store.PhotoConfirmedRotation
		}
	}
	if err := store.ValidatePhotoAuthored(result); err != nil {
		return result, err
	}
	return result, nil
}
