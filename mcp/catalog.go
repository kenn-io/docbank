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
// a handler, validate calls, or authorize access. The host owns admission and
// transport; docbank.Vault.CallReadTool executes its supported subset with native
// validation and result bounds. Other tools need caller-supplied execution.

func ReadTool(name string) (*sdkmcp.Tool, error) {
	return internalmcp.ReadTool(name)
}
