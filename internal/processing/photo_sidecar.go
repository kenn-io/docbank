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

const teststripXMPNamespace = "https://teststrip.app/xmp/1.0/"

// ReadPhotoSidecar validates the complete packet before returning supported decisions.
func ReadPhotoSidecar(ctx context.Context, data []byte) (store.PhotoAuthored, error) {
	packet, err := readPhotoSidecar(ctx, data)
	return packet.PhotoAuthored, err
}

// boundedSidecarText buffers one property's text up to the authored text
// limit. An oversized value reads as absent instead of as a truncated prefix,
// so "3" followed by padding and other text cannot become a three-star rating.
// It tracks non-whitespace text separately, so an oversized prefix still counts
// as text beside a property's value.
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

	SidecarForExtension string
}

func readPhotoSidecar(ctx context.Context, data []byte) (photoSidecarPacket, error) {
	var result photoSidecarPacket
	if len(data) > maxSourceMetadataOriginalBytes || !utf8.Valid(data) {
		return result, errors.New("invalid photo sidecar bytes")
	}
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	stack := []xml.Name{}
	rootCount := 0
	description := false
	var subjectKind, subjectValue string
	activeDescription := false
	values := map[string]string{}
	var field string
	var fieldDepth int
	var child xml.Name
	var text boundedSidecarText
	var items []string
	var itemText boundedSidecarText
	var itemDepth int
	var itemDefault bool
	var defaultText string
	var hasDefault bool
	fieldFor := func(n xml.Name) string {
		switch n.Space {
		case "http://ns.adobe.com/photoshop/1.0/":
			if n.Local == "SidecarForExtension" {
				return n.Local
			}
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
					if child != (xml.Name{}) || text.content {
						return result, errors.New("multiple authored RDF values")
					}
					child = t.Name
					text.reset()
					valid = valid && (t.Name.Local == "value" || field == "creator" && t.Name.Local == "Seq" || (field == "description" || field == "rights") && t.Name.Local == "Alt")
				} else {
					valid = valid && len(stack) == fieldDepth+2 && t.Name.Local == "li" && parent.Space == rdfNamespace && (parent.Local == "Alt" || parent.Local == "Seq")
				}
				if !valid {
					return result, errors.New("unsupported authored RDF value")
				}
			}
			if len(stack) == 3 && t.Name.Space == rdfNamespace && t.Name.Local == "Description" && parent.Space == rdfNamespace && parent.Local == "RDF" {
				kind, value := "about", ""
				for _, attr := range t.Attr {
					if attr.Name.Space == rdfNamespace {
						switch attr.Name.Local {
						case "about":
							if kind == "about" {
								value = attr.Value
							}
						case "ID", "nodeID":
							kind, value = attr.Name.Local, attr.Value
						}
					}
				}
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
			if activeDescription && len(stack) == 4 && parent.Space == rdfNamespace && parent.Local == "Description" {
				if key := fieldFor(t.Name); key != "" {
					if field != "" {
						return result, errors.New("nested authored XMP property")
					}
					field = key
					fieldDepth = len(stack)
					child = xml.Name{}
					text.reset()
					items = nil
					hasDefault = false
					defaultText = ""
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
			if field != "" && fieldDepth == len(stack) {
				value := text.value()
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
	result.SidecarForExtension = strings.ToLower(values["SidecarForExtension"])
	result.Flag = values["Pick"]
	if !query.ValidPhotoFlag(result.Flag) {
		result.Flag = ""
	}
	result.Label = strings.ToLower(values["Label"])
	if !query.ValidPhotoColorLabel(result.Label) {
		result.Label = ""
	}
	result.Caption = values["description"]
	result.Creator = values["creator"]
	result.Copyright = values["rights"]
	for _, text := range []*string{&result.Caption, &result.Creator, &result.Copyright} {
		if len(*text) > store.MaxPhotoAuthoredTextBytes {
			*text = ""
		}
	}
	// XMP types Rating as a real number, so "3.0" is a valid three-star rating.
	if n, err := strconv.ParseFloat(values["Rating"], 64); err == nil && n == math.Trunc(n) && n >= -1 && n <= 5 {
		if n == -1 {
			result.Flag = "reject"
		} else {
			result.Rating = int(n)
		}
	}
	if value, ok := values["Rotation"]; ok {
		n, err := strconv.Atoi(value)
		if err == nil && (n == 0 || n == 90 || n == 180 || n == 270) {
			result.Rotation = n
		}
	}
	if err := store.ValidatePhotoAuthoredValues(result.PhotoAuthored); err != nil {
		return result, err
	}
	return result, nil
}
