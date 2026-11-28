package mcp

import (
	_ "embed"

	"go.kenn.io/kit/mcpskills"
)

var (
	// CLIWorkflowHelp is the root CLI help, preserved verbatim.
	//go:embed agentguide/workflow.txt
	CLIWorkflowHelp string
	// CLISearchHelp is the CLI search help, preserved verbatim.
	//go:embed agentguide/search.txt
	CLISearchHelp string
	// CLIProcessingHelp is the CLI processing help, preserved verbatim.
	//go:embed agentguide/processing.txt
	CLIProcessingHelp string

	//go:embed agentguide/workflow.md
	workflowMCP string
	//go:embed agentguide/search.md
	searchMCP string
	//go:embed agentguide/processing.md
	processingMCP string
)

// bundledGuides returns only the shipped allowlist. MCP adaptations precede the shared
// CLI reference so command examples cannot be mistaken for tool arguments.
func bundledGuides() []mcpskills.Guide {
	return []mcpskills.Guide{
		{Name: "workflow", Title: "Docbank agent workflow", Summary: "Select exact document identities, find evidence and read verified content.", Markdown: withCLI(workflowMCP, CLIWorkflowHelp)},
		{Name: "search", Title: "Find document evidence", Summary: "CLISearchHelp an explicit scope and interpret coverage before drawing conclusions.", Markdown: withCLI(searchMCP, CLISearchHelp)},
		{Name: "processing", Title: "Review document processing", Summary: "Inspect provider disclosure and use existing consent for an exact reviewed plan.", Markdown: withCLI(processingMCP, CLIProcessingHelp)},
	}
}

func withCLI(adaptation, source string) string {
	return adaptation + "\n## CLI guidance\n\nThe following is the CLI's authored help. Use the MCP adaptation above for tool calls.\n\n```text\n" + source + "\n```\n"
}
