package renderpdf

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
)

const (
	FlatTextKind     = "fodt"
	maxXMLTokenBytes = 1 << 20
	maxXMLAttributes = 128
)

// Admission records the bounded normalized document accepted by the scanner.
type Admission struct {
	Kind     string
	Bytes    int64
	Elements int
	Depth    int
}

// Scan checks one exact flat ODF byte sequence against its expected profile.
func Scan(data []byte, expectedKind string, limits Limits) (Admission, error) {
	if int64(len(data)) <= 0 || int64(len(data)) > limits.MaxNormalizedBytes {
		return Admission{}, errors.New("normalized ODF exceeds byte limit")
	}
	if expectedKind != FlatTextKind {
		return Admission{}, errors.New("normalized ODF kind is unsupported")
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.Strict = true
	depth := 0
	maxDepth := 0
	elements := 0
	rootSeen := false
	rootClosed := false
	scriptDepth := 0
	stack := make([]xml.Name, 0, 16)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !rootSeen || !rootClosed || depth != 0 {
				return Admission{}, errors.New("normalized ODF document root is invalid")
			}
			return Admission{Kind: expectedKind, Bytes: int64(len(data)), Elements: elements, Depth: maxDepth}, nil
		}
		if err != nil {
			return Admission{}, errors.New("normalized ODF XML is malformed")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(value.Attr) > maxXMLAttributes {
				return Admission{}, errors.New("normalized ODF has too many attributes")
			}
			if len(value.Name.Local) > maxXMLTokenBytes {
				return Admission{}, errors.New("normalized ODF token exceeds limit")
			}
			if depth == 0 {
				if rootSeen || value.Name.Local != "document" || value.Name.Space != "urn:oasis:names:tc:opendocument:xmlns:office:1.0" {
					return Admission{}, errors.New("normalized ODF document root is invalid")
				}
				rootSeen = true
				if !hasExpectedMimeType(value.Attr) {
					return Admission{}, errors.New("normalized ODF document kind does not match profile")
				}
			}
			depth++
			local := strings.ToLower(value.Name.Local)
			if scriptDepth > 0 && depth > scriptDepth && local != "libraries" {
				return Admission{}, errors.New("normalized ODF contains a script or event handler")
			}
			if local == "script" && value.Name.Space == "urn:oasis:names:tc:opendocument:xmlns:office:1.0" {
				if !hasLibreOfficeScriptLanguage(value.Attr) {
					return Admission{}, errors.New("normalized ODF contains a script or event handler")
				}
				scriptDepth = depth
			}
			maxDepth = max(maxDepth, depth)
			if depth > limits.MaxXMLDepth {
				return Admission{}, errors.New("normalized ODF exceeds XML depth limit")
			}
			elements++
			if elements > limits.MaxXMLElements {
				return Admission{}, errors.New("normalized ODF exceeds XML element limit")
			}
			if err := inspectStartElement(value); err != nil {
				return Admission{}, err
			}
			for _, attr := range value.Attr {
				if len(attr.Value) > maxXMLTokenBytes {
					return Admission{}, errors.New("normalized ODF attribute exceeds limit")
				}
				if err := inspectAttribute(attr); err != nil {
					return Admission{}, err
				}
			}
			stack = append(stack, value.Name)
		case xml.EndElement:
			if depth <= 0 || len(stack) == 0 || stack[len(stack)-1] != value.Name {
				return Admission{}, errors.New("normalized ODF XML nesting is invalid")
			}
			stack = stack[:len(stack)-1]
			if depth == scriptDepth {
				scriptDepth = 0
			}
			depth--
			if depth == 0 {
				rootClosed = true
			}
		case xml.CharData:
			if len(value) > maxXMLTokenBytes {
				return Admission{}, errors.New("normalized ODF character data exceeds limit")
			}
			if len(stack) > 0 && formulaElement(stack[len(stack)-1]) && hasUnsafeFormula(string(value)) {
				return Admission{}, errors.New("normalized ODF contains an external or linked formula")
			}
			if scriptDepth > 0 && depth >= scriptDepth && len(bytes.TrimSpace(value)) != 0 {
				return Admission{}, errors.New("normalized ODF contains a script or event handler")
			}
		case xml.Directive:
			return Admission{}, errors.New("normalized ODF directives are not admitted")
		case xml.ProcInst:
			if value.Target != "xml" {
				return Admission{}, errors.New("normalized ODF processing instruction is not admitted")
			}
		}
	}
}

func hasExpectedMimeType(attributes []xml.Attr) bool {
	want := "application/vnd.oasis.opendocument.text"
	for _, attr := range attributes {
		if attr.Name.Local == "mimetype" && attr.Name.Space == "urn:oasis:names:tc:opendocument:xmlns:office:1.0" {
			return attr.Value == want
		}
	}
	return false
}

func inspectStartElement(element xml.StartElement) error {
	local := strings.ToLower(element.Name.Local)
	switch {
	case local == "scripts" || local == "script" && element.Name.Space == "urn:oasis:names:tc:opendocument:xmlns:office:1.0":
		return nil
	case local == "script" || local == "event-listener" || local == "event-listeners":
		return errors.New("normalized ODF contains a script or event handler")
	case local == "dde" || strings.Contains(local, "dde-") || strings.Contains(local, "database") ||
		local == "datasource" || local == "data-source" || local == "external-data":
		return errors.New("normalized ODF contains a DDE or database source")
	case local == "section-source" || local == "linked-section" || local == "linked-image" || local == "linked-object":
		return errors.New("normalized ODF contains a linked section or object")
	case local == "object" || local == "object-ole" || local == "ole" || local == "plugin" ||
		local == "applet" || local == "nested-document" || local == "embedded-document" || local == "subdocument":
		return errors.New("normalized ODF contains an opaque active object")
	}
	return nil
}

func hasLibreOfficeScriptLanguage(attributes []xml.Attr) bool {
	for _, attribute := range attributes {
		if attribute.Name.Local == "language" && attribute.Name.Space == "urn:oasis:names:tc:opendocument:xmlns:script:1.0" {
			return attribute.Value == "ooo:Basic"
		}
	}
	return false
}

func inspectAttribute(attribute xml.Attr) error {
	if attribute.Name.Space == "xmlns" || attribute.Name.Local == "xmlns" {
		return nil
	}
	if attribute.Name.Space == "http://www.w3.org/XML/1998/namespace" && attribute.Name.Local == "base" {
		return errors.New("normalized ODF XML Base is not admitted")
	}
	local := strings.ToLower(attribute.Name.Local)
	if (local == "value" && strings.Contains(attribute.Name.Space, "field")) ||
		local == "formula" || local == "f" || local == "instr" || local == "command" {
		if hasUnsafeFormula(attribute.Value) {
			return errors.New("normalized ODF contains an external or linked formula")
		}
	}
	if attribute.Name.Space == "http://www.w3.org/1999/xlink" && local == "href" {
		if allowedInternalLink(attribute.Value) {
			return nil
		}
		return errors.New("normalized ODF contains an external or relative link")
	}
	if local == "formula" || local == "f" {
		if hasUnsafeFormula(attribute.Value) {
			return errors.New("normalized ODF contains an external or linked formula")
		}
	}
	if local == "event-name" || local == "event-handler" || local == "script" || local == "script-name" ||
		local == "listener" || local == "macro" {
		return errors.New("normalized ODF contains an event handler")
	}
	if local == "object" || local == "ole" || local == "plugin" || local == "applet" || local == "embedded-document" {
		return errors.New("normalized ODF contains an opaque active object")
	}
	return nil
}

func allowedInternalLink(value string) bool {
	if strings.HasPrefix(value, "#") && len(value) > 1 {
		return true
	}
	lower := strings.ToLower(value)
	if !strings.HasPrefix(lower, "data:image/") {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "data" {
		return false
	}
	header, _, ok := strings.Cut(lower, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return false
	}
	mediaType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	return slices.Contains([]string{"image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp"}, mediaType)
}

func formulaElement(element xml.Name) bool {
	local := strings.ToLower(element.Local)
	return local == "f" || local == "formula" || local == "expression"
}

func hasUnsafeFormula(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false
	}
	for _, marker := range []string{
		"webservice", "dde", "http:", "https:", "file:", "ftp:",
		"vnd.sun.star.script:",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
