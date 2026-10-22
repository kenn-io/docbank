// Package mcp exposes native MCP tool definitions for applications that supply
// their own handlers and transport.
package mcp

import (
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	internalmcp "go.kenn.io/docbank/internal/mcp"
)

// ReadTool returns an independent copy of a named read-only tool, including its
// input schema, output schema, annotations, and metadata. Schemas are JSON
// objects represented by map[string]any. Unknown names and write tools return
// an error.
//
// This is a definition only: it does not open a vault, start a daemon, register
// a handler, validate calls, or authorize access. Callers own those
// responsibilities and must implement the selected tool's documented behavior,
// including semantic validation and response bounds. Use the SDK's generic
// AddTool function to validate inputs and outputs against these schemas.
func ReadTool(name string) (*sdkmcp.Tool, error) {
	return internalmcp.ReadTool(name)
}
