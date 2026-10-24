package processing

import (
	"encoding/xml"
	"slices"
	"strings"

	"go.kenn.io/docbank/internal/store"
)

const photoXMPAlt = "Alt"

// Lightroom stores hierarchical paths and flat leaf copies beside dc:subject. A
// hierarchy keeps while its leaf is a keyword; a flat keyword is the whole string.
func photoXMPLightroomRetained(name xml.Name, value string, keywords []string) bool {
	if name.Local == "hierarchicalSubject" {
		value = value[strings.LastIndex(value, "|")+1:]
	}
	leaf, err := store.NormalizeTagName(value)
	if err != nil {
		return false
	}
	return slices.Contains(keywords, leaf)
}

// editPhotoXMPCollection preserves complete token ranges for later creators,
// translations, and qualifiers. Every retained token uses the caller's emitter.
func editPhotoXMPCollection(emit func(xml.Token) error, property xml.StartElement, tokens []xml.Token, collection, value string, keywords []string, alias bool) error {
	literal := func(start xml.StartElement) xml.StartElement {
		start.Attr = slices.DeleteFunc(slices.Clone(start.Attr), func(a xml.Attr) bool { return a.Name.Space == rdfNamespace })
		return start
	}
	if alias {
		collection = "Bag"
	}
	wrapper := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: collection}}
	begin, end, selectedStart, selectedEnd := 0, 0, 0, 0
	li := xml.StartElement{Name: xml.Name{Space: rdfNamespace, Local: "li"}}
	for i, depth := 1, 1; i < len(tokens)-1; i++ {
		switch t := tokens[i].(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && t.Name == wrapper.Name {
				wrapper, begin = t, i
			}
			if depth == 3 && begin > 0 && t.Name == li.Name && selectedStart == 0 && (collection == "Seq" || slices.ContainsFunc(t.Attr, func(a xml.Attr) bool {
				return a.Name == (xml.Name{Space: xmlNamespace, Local: "lang"}) && strings.EqualFold(a.Value, "x-default")
			})) {
				selectedStart = i
				li = literal(t)
			}
		case xml.EndElement:
			if depth == 3 && selectedStart > 0 && selectedEnd == 0 {
				selectedEnd = i + 1
			}
			if depth == 2 && begin > 0 {
				end = i
			}
			depth--
		}
	}
	if alias {
		// Lightroom paths remain only while their leaf keyword is in the final set.
		for _, token := range tokens {
			if start, ok := token.(xml.StartElement); ok && len(literal(start).Attr) != len(start.Attr) {
				return errPhotoXMPKeywords
			}
		}
		if begin == 0 {
			if photoXMPLightroomRetained(property.Name, photoXMPTokenText(tokens), keywords) {
				return emitPhotoXMPTokens(emit, tokens)
			}
			return nil
		}
		kept := []xml.Token{property, wrapper}
		for i := begin + 1; i < end; {
			j := i + 1
			if start, ok := tokens[i].(xml.StartElement); ok {
				if start.Name != li.Name {
					return errPhotoXMPKeywords
				}
				for j < end {
					if stop, ok := tokens[j].(xml.EndElement); ok && stop.Name == li.Name {
						j++
						break
					}
					j++
				}
				if photoXMPLightroomRetained(property.Name, photoXMPTokenText(tokens[i:j]), keywords) {
					kept = append(kept, tokens[i:j]...)
				}
			} else {
				kept = append(kept, tokens[i])
			}
			i = j
		}
		kept = append(kept, wrapper.End(), property.End())
		return emitPhotoXMPTokens(emit, kept)
	}
	if collection == photoXMPAlt {
		li.Attr = slices.DeleteFunc(li.Attr, func(a xml.Attr) bool { return a.Name == (xml.Name{Space: xmlNamespace, Local: "lang"}) })
		li.Attr = append(li.Attr, xml.Attr{Name: xml.Name{Space: xmlNamespace, Local: "lang"}, Value: "x-default"})
	}
	replacement := []xml.Token{li, xml.CharData(value), li.End()}
	if begin == 0 {
		return emitPhotoXMPTokens(emit, append(append([]xml.Token{literal(property), literal(wrapper)}, replacement...), wrapper.End(), property.End()))
	}
	tokens = slices.Clone(tokens)
	tokens[0], tokens[begin] = literal(property), literal(wrapper)
	if collection == photoXMPAlt {
		// Adobe's alternatives list x-default first, and Teststrip reads the first item.
		if selectedStart > 0 {
			tokens = slices.Delete(tokens, selectedStart, selectedEnd)
		}
		tokens = slices.Insert(tokens, begin+1, replacement...)
		return emitPhotoXMPTokens(emit, tokens)
	}
	if selectedStart == 0 {
		selectedStart, selectedEnd = end, end
	}
	tokens = slices.Replace(tokens, selectedStart, selectedEnd, replacement...)
	return emitPhotoXMPTokens(emit, tokens)
}

func emitPhotoXMPTokens(emit func(xml.Token) error, tokens []xml.Token) error {
	for _, token := range tokens {
		if err := emit(token); err != nil {
			return err
		}
	}
	return nil
}

func photoXMPTokenText(tokens []xml.Token) string {
	var text strings.Builder
	for _, token := range tokens {
		if data, ok := token.(xml.CharData); ok {
			text.Write(data)
		}
	}
	return text.String()
}
