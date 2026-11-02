package agentmcp

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"

	"github.com/tailscale/hujson"
)

func planJSON(agent Agent, original []byte, name string, entry map[string]any) ([]byte, bool, error) {
	input := original
	var bom []byte
	if agent == AgentGemini && len(input) >= 3 && bytes.HasPrefix(input, []byte{0xef, 0xbb, 0xbf}) {
		bom, input = input[:3], input[3:]
	}
	if len(input) == 0 {
		input = []byte("{}")
	}
	root, err := hujson.Parse(input)
	if err != nil {
		return nil, false, err
	}
	if agent != AgentGemini && agent != AgentQwen && agent != AgentCursor && !root.IsStandard() {
		return nil, false, errors.New("MCP config must use standard JSON")
	}
	standard := root.Clone()
	standard.Standardize()
	var doc map[string]jsontext.Value
	if err := json.Unmarshal(standard.Pack(), &doc); err != nil {
		return nil, false, err
	}
	if doc == nil {
		return nil, false, errors.New("MCP config must be an object")
	}
	servers := map[string]jsontext.Value{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, false, err
		}
		if servers == nil {
			return nil, false, errors.New("mcpServers must be an object")
		}
	}
	encoded, err := json.Marshal(entry, json.Deterministic(true))
	if err != nil {
		return nil, false, err
	}
	if previous, ok := servers[name]; ok {
		if err := previous.Canonicalize(); err != nil {
			return nil, false, err
		}
		canonical := jsontext.Value(encoded)
		if err := canonical.Canonicalize(); err != nil {
			return nil, false, err
		}
		if bytes.Equal(previous, canonical) {
			return original, false, nil
		}
	}
	if _, ok := doc["mcpServers"]; !ok {
		if err := root.Patch([]byte(`[{"op":"add","path":"/mcpServers","value":{}}]`)); err != nil {
			return nil, false, err
		}
	}
	pointer := "/mcpServers/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
	if target := root.Find(pointer); target != nil {
		replacement, err := hujson.Parse(encoded)
		if err != nil {
			return nil, false, err
		}
		inner := *target
		inner.BeforeExtra, inner.AfterExtra = nil, nil
		comments := jsonComments(inner)
		if !comments.IsStandard() {
			replacement.Value.(*hujson.Object).AfterExtra = comments
		}
		target.Value = replacement.Value
	} else {
		patch, err := json.Marshal([]map[string]any{{"op": "add", "path": pointer, "value": entry}}, json.Deterministic(true))
		if err != nil {
			return nil, false, err
		}
		if err := root.Patch(patch); err != nil {
			return nil, false, err
		}
	}
	// HuJSON formatting adds trailing commas to commented documents, while
	// some clients accept comments but reject trailing commas. Keep their layout.
	if root.IsStandard() {
		root.Format()
	}
	return append(bom, root.Pack()...), true, nil
}

// Extra contains only whitespace and comments already identified by HuJSON.
func jsonComments(value hujson.Value) hujson.Extra {
	comments := append(hujson.Extra(nil), value.BeforeExtra...)
	switch node := value.Value.(type) {
	case *hujson.Object:
		for _, member := range node.Members {
			comments = append(comments, jsonComments(member.Name)...)
			comments = append(comments, jsonComments(member.Value)...)
		}
		comments = append(comments, node.AfterExtra...)
	case *hujson.Array:
		for _, element := range node.Elements {
			comments = append(comments, jsonComments(element)...)
		}
		comments = append(comments, node.AfterExtra...)
	}
	return append(comments, value.AfterExtra...)
}
