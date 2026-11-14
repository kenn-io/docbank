package main

import (
	"github.com/spf13/cobra"
	"go.kenn.io/kit/clisearch"
)

func cliSearchOptions() clisearch.Options {
	return clisearch.Options{
		Synonyms: map[string][]string{
			"pdf":      {"document"},
			"folder":   {"directory"},
			"snapshot": {"backup"},
			"mail":     {"email", "mailbox"},
			"contact":  {"person", "people"},
			"label":    {"bates"},
			"extract":  {"rendition", "text"},
		},
		Skip:       skipCLISearchCommand,
		UsageError: usageError,
		Render: func(cmd *cobra.Command, response clisearch.Response) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), response)
			}
			return clisearch.RenderText(cmd, response)
		},
	}
}

func skipCLISearchCommand(cmd *cobra.Command) bool {
	if cmd.Annotations[helpOnlyAnnotation] != "" {
		return true
	}
	switch cmd.CommandPath() {
	case "docbank tui", "docbank web", "docbank mcp", "docbank daemon run":
		return true
	}
	return false
}

func init() {
	// These operations use shorter CLI verbs than task descriptions commonly do.
	clisearch.SetKeywords(addCmd, "import PDF")
	clisearch.SetKeywords(getCmd, "download verified copy")
	clisearch.SetKeywords(statCmd, "inspect metadata")
	// Discovery reads only Cobra metadata. Keep the root's validation hook;
	// config and daemon access belong to the data commands' handlers.
	cli := clisearch.NewCommand(rootCmd, cliSearchOptions())
	cli.GroupID = groupOperations
	cli.Long = "Find commands by describing a task, without opening a vault or daemon."
	search := cli.Commands()[0]
	// Use Docbank's canonical recovery hints for native flag-parse errors.
	nativeFlagError := func(cmd *cobra.Command, err error) error { return rootCmd.FlagErrorFunc()(cmd, err) }
	cli.SetFlagErrorFunc(nativeFlagError)
	search.SetFlagErrorFunc(nativeFlagError)
	search.Example = `  docbank cli search "import a folder of PDFs"
  docbank cli search pack loose blobs --json --limit 3`
	search.Flags().Bool("json", false, "print JSON to stdout")
	rootCmd.AddCommand(cli)
}
