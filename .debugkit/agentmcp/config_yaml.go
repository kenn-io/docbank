package agentmcp

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

func editYAML(original []byte, name string, entry, existing map[string]any) ([]byte, error) {
	var doc yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(original))
	if err := decoder.Decode(&doc); errors.Is(err, io.EOF) {
		// A comments-only document has no YAML node to carry its comments.
		// Preserve those original bytes and append the new mapping.
		data, err := yaml.Marshal(map[string]any{"mcp_servers": map[string]any{name: entry}})
		if err != nil {
			return nil, err
		}
		if len(original) > 0 && original[len(original)-1] != '\n' {
			original = append(original, '\n')
		}
		return append(original, data...), nil
	} else if err != nil {
		return nil, err
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("MCP config must be a mapping")
	}
	servers := yamlMappingValue(root, "mcp_servers")
	if servers.Kind == 0 {
		// A new local mapping must retain servers inherited through a YAML
		// merge key. The original merge source and its comments stay intact.
		if err := servers.Encode(existing); err != nil {
			return nil, err
		}
	}
	if servers.Kind != yaml.MappingNode {
		return nil, errors.New("mcp_servers must be a direct YAML mapping")
	}
	target := yamlMappingValue(servers, name)
	var replacement yaml.Node
	if err := replacement.Encode(entry); err != nil {
		return nil, err
	}
	// Comments on removed fields still belong to the user's config. Keep them
	// above the replacement rather than dropping them with the old values.
	replacement.HeadComment = strings.Join(yamlComments(target), "\n")
	replacement.Anchor = target.Anchor
	*target = replacement
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func yamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	value := new(yaml.Node)
	mapping.Content = append(mapping.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
	return value
}

func yamlComments(node *yaml.Node) []string {
	var comments []string
	for _, comment := range []string{node.HeadComment, node.LineComment} {
		if comment != "" {
			comments = append(comments, comment)
		}
	}
	for _, child := range node.Content {
		comments = append(comments, yamlComments(child)...)
	}
	if node.FootComment != "" {
		comments = append(comments, node.FootComment)
	}
	return comments
}
