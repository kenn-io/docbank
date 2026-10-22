package mcp

import (
	"encoding/json/v2"
	"fmt"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReadTool supplies the public catalog API without exposing daemon handlers or
// sharing mutable schema and metadata values with callers.
func ReadTool(name string) (*sdkmcp.Tool, error) {
	for _, definition := range toolCatalog(ServerOptions{}) {
		if definition.Name != name {
			continue
		}
		encoded, err := json.Marshal(definition)
		if err != nil {
			return nil, fmt.Errorf("encode read tool %q: %w", name, err)
		}
		var tool sdkmcp.Tool
		if err := json.Unmarshal(encoded, &tool); err != nil {
			return nil, fmt.Errorf("copy read tool %q: %w", name, err)
		}
		return &tool, nil
	}
	return nil, fmt.Errorf("read tool %q is unavailable", name)
}
