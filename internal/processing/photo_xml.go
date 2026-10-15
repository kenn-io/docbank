package processing

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const xmlNamespacePrefix = "xmlns"

type photoXMLNamespaceUndo struct {
	prefix, value string
	existed       bool
}

type photoXMLTokenReader struct {
	decoder    *xml.Decoder
	namespaces map[string]string
	scopes     [][]photoXMLNamespaceUndo
}

func newPhotoXMLDecoder(data []byte) *xml.Decoder {
	reader := &photoXMLTokenReader{
		decoder:    xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))),
		namespaces: map[string]string{"xml": xmlNamespace},
	}
	return xml.NewTokenDecoder(reader)
}

func (r *photoXMLTokenReader) Token() (xml.Token, error) {
	token, err := r.decoder.RawToken()
	if err != nil {
		return nil, err //nolint:wrapcheck // Preserve token-stream EOF for the standard decoder.
	}
	check := func(name xml.Name) error {
		if strings.Contains(name.Local, ":") {
			return errors.New("invalid XML qualified name")
		}
		if name.Space != "" && r.namespaces[name.Space] == "" {
			return fmt.Errorf("undeclared XML namespace prefix %q", name.Space)
		}
		return nil
	}
	switch t := token.(type) {
	case xml.StartElement:
		var undo []photoXMLNamespaceUndo
		for _, a := range t.Attr {
			prefix := a.Name.Local
			if a.Name.Space != xmlNamespacePrefix {
				if a.Name.Space != "" || prefix != xmlNamespacePrefix {
					continue
				}
				prefix = ""
			}
			if prefix == xmlNamespacePrefix || a.Value == "http://www.w3.org/2000/xmlns/" ||
				(prefix == "xml") != (a.Value == xmlNamespace) || prefix != "" && a.Value == "" {
				return nil, errors.New("invalid XML namespace binding")
			}
			value, existed := r.namespaces[prefix]
			undo = append(undo, photoXMLNamespaceUndo{prefix, value, existed})
			r.namespaces[prefix] = a.Value
		}
		r.scopes = append(r.scopes, undo)
		if err := check(t.Name); err != nil {
			return nil, err
		}
		seen := map[xml.Name]bool{}
		for _, a := range t.Attr {
			if a.Name.Space != xmlNamespacePrefix {
				if err := check(a.Name); err != nil {
					return nil, err
				}
				if a.Name.Space != "" {
					a.Name.Space = r.namespaces[a.Name.Space]
				}
			}
			if seen[a.Name] {
				return nil, errors.New("duplicate XML attribute")
			}
			seen[a.Name] = true
		}
	case xml.EndElement:
		if err := check(t.Name); err != nil {
			return nil, err
		}
		if len(r.scopes) > 0 {
			undoScope := r.scopes[len(r.scopes)-1]
			for _, undo := range slices.Backward(undoScope) {
				if undo.existed {
					r.namespaces[undo.prefix] = undo.value
				} else {
					delete(r.namespaces, undo.prefix)
				}
			}
			r.scopes = r.scopes[:len(r.scopes)-1]
		}
	}
	return token, nil
}
