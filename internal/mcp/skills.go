package mcp

import (
	"sync"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.kenn.io/docbank/internal/agentguide"
	"go.kenn.io/kit/mcpskills"
)

var appSkills = sync.OnceValue(func() *mcpskills.Catalog {
	catalog, err := mcpskills.New(mcpskills.Config{ToolName: "docbank.skills", Guides: agentguide.Guides()})
	if err != nil {
		panic(err) // The bundled catalog is a build-time application invariant.
	}
	return catalog
})

func skillsTool() *sdkmcp.Tool {
	tool := appSkills().Tool()
	// Kit supplies independent JSON schema objects. Keep Docbank's dialect and
	// response bounds on this application's copy without changing shared policy.
	tool.InputSchema.(map[string]any)["$schema"] = jsonSchemaDraft
	tool.OutputSchema.(map[string]any)["$schema"] = jsonSchemaDraft
	tool.Annotations.DestructiveHint = new(false)
	tool.Meta = sdkmcp.Meta{"io.docbank/bounds": map[string]any{"maxResponseBytes": maxToolResponseBytes}}
	return tool
}
