package brainservice

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"unicode/utf16"
)

// Task Scheduler may change XML formatting/field order. Compare the whole
// definition after XML normalization, not a permissive subset of action fields.
// Unknown additions (including privilege/restart changes) remain mismatches.
type taskXMLNode struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Text     string
	Children []*taskXMLNode
}

func equalTaskXML(a, b []byte) (bool, error) {
	first, err := parseTaskXML(a)
	if err != nil {
		return false, err
	}
	second, err := parseTaskXML(b)
	if err != nil {
		return false, err
	}
	return reflect.DeepEqual(first, second), nil
}
func parseTaskXML(data []byte) (*taskXMLNode, error) {
	if len(data) > maxDefinition {
		return nil, ErrOutputLimit
	}
	if len(data) >= 2 && (bytes.Equal(data[:2], []byte{0xff, 0xfe}) || bytes.Equal(data[:2], []byte{0xfe, 0xff})) {
		if len(data)%2 != 0 {
			return nil, ErrManager
		}
		var order binary.ByteOrder = binary.LittleEndian
		if data[0] == 0xfe {
			order = binary.BigEndian
		}
		words := make([]uint16, (len(data)-2)/2)
		for i := range words {
			words[i] = order.Uint16(data[2+i*2:])
		}
		data = []byte(string(utf16.Decode(words)))
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "utf-16") {
			return input, nil
		}
		return nil, ErrManager
	}
	var root *taskXMLNode
	var stack []*taskXMLNode
	nodes := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrManager
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 512 || len(stack) > 32 {
				return nil, ErrManager
			}
			node := &taskXMLNode{Name: token.Name}
			for _, attr := range token.Attr {
				if attr.Name.Local == "xmlns" || attr.Name.Space == "xmlns" {
					continue
				}
				node.Attrs = append(node.Attrs, attr)
			}
			slices.SortFunc(node.Attrs, func(a, b xml.Attr) int {
				return strings.Compare(a.Name.Space+":"+a.Name.Local, b.Name.Space+":"+b.Name.Local)
			})
			if len(stack) == 0 {
				if root != nil {
					return nil, ErrManager
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, node)
			}
			stack = append(stack, node)
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(token)
			} else if strings.TrimSpace(string(token)) != "" {
				return nil, ErrManager
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, ErrManager
			}
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if len(node.Children) > 0 && strings.TrimSpace(node.Text) == "" {
				node.Text = ""
			}
			slices.SortStableFunc(node.Children, func(a, b *taskXMLNode) int {
				return strings.Compare(a.Name.Space+":"+a.Name.Local, b.Name.Space+":"+b.Name.Local)
			})
		}
	}
	if root == nil || len(stack) != 0 || root.Name.Local != "Task" || root.Name.Space != "http://schemas.microsoft.com/windows/2004/02/mit/task" {
		return nil, ErrManager
	}
	return root, nil
}
