// Package yamlnode provides shared YAML node operations for lint tools.
package yamlnode

import "gopkg.in/yaml.v3"

// MappingValue returns the node stored under key, or nil if it is absent.
func MappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
