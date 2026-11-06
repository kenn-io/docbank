// Package agentguide owns the authored guidance shared by CLI help and MCP.
package agentguide

import (
	_ "embed"

	"go.kenn.io/kit/mcpskills"
)

var (
	// Workflow is the root CLI help, preserved verbatim.
	//go:embed workflow.txt
	Workflow string
	// Search is the CLI search help, preserved verbatim.
	//go:embed search.txt
	Search string
	// Processing is the CLI processing help, preserved verbatim.
	//go:embed processing.txt
	Processing string

	//go:embed workflow.md
	workflowMCP string
	//go:embed search.md
	searchMCP string
	//go:embed processing.md
	processingMCP string
)

// Guides returns only the shipped allowlist. MCP adaptations precede the shared
// CLI reference so command examples cannot be mistaken for tool arguments.
func Guides() []mcpskills.Guide {
	return []mcpskills.Guide{
		{Entry: mcpskills.Entry{Name: "workflow", Title: "Docbank agent workflow", Summary: "Select exact document identities, find evidence and read verified content."}, Markdown: withCLI(workflowMCP, Workflow)},
		{Entry: mcpskills.Entry{Name: "search", Title: "Find document evidence", Summary: "Search an explicit scope and interpret coverage before drawing conclusions."}, Markdown: withCLI(searchMCP, Search)},
		{Entry: mcpskills.Entry{Name: "processing", Title: "Review document processing", Summary: "Inspect provider disclosure and use existing consent for an exact reviewed plan."}, Markdown: withCLI(processingMCP, Processing)},
	}
}

func withCLI(adaptation, source string) string {
	return adaptation + "\n## CLI guidance\n\nThe following is the CLI's authored help. Use the MCP adaptation above for tool calls.\n\n```text\n" + source + "\n```\n"
}
