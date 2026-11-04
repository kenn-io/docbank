//go:build !darwin || arm64

package main

import (
	"strings"
	"testing"

	"go.kenn.io/kit/clisearch"
	"hegel.dev/go/hegel"
)

// The real tree, including work-performing parents, is the eligibility oracle.
func TestCLISearchEligibleResultsProperty(t *testing.T) {
	configureCommandGroups(rootCmd)
	hegel.Test(t, func(ht *hegel.T) {
		query := hegel.Draw(ht, hegel.Text())
		if hegel.Draw(ht, hegel.Booleans()) {
			query = hegel.Draw(ht, hegel.SampledFrom(cliSearchTasks)).query + " " + query
		}
		limit := hegel.Draw(ht, hegel.Integers(1, 20))
		results := clisearch.Search(rootCmd, query, limit, cliSearchOptions())
		if results == nil || len(results) > limit {
			ht.Fatalf("unbounded or null results")
		}
		seen := map[string]bool{}
		for _, result := range results {
			cmd, remaining, err := rootCmd.Find(strings.Fields(strings.TrimPrefix(result.Command, "docbank ")))
			if err != nil || len(remaining) != 0 || !cmd.Runnable() || seen[result.Command] {
				ht.Fatalf("invalid result %+v", result)
			}
			seen[result.Command] = true
			if cmd.Annotations[helpOnlyAnnotation] != "" || (cmd.HasSubCommands() && !workParents[result.Command]) {
				ht.Fatalf("help-only result %s", result.Command)
			}
			for ancestor := cmd; ancestor != nil; ancestor = ancestor.Parent() {
				if ancestor.Hidden || ancestor.Deprecated != "" || ancestor.Name() == "cli" || ancestor.Name() == "completion" || ancestor.Name() == "help" {
					ht.Fatalf("excluded ancestor %s", result.Command)
				}
			}
			switch result.Command {
			case "docbank tui", "docbank web", "docbank mcp", "docbank daemon run":
				ht.Fatalf("long-lived result %s", result.Command)
			}
		}
	}, hegel.WithTestCases(1000), hegel.WithDatabase(t.TempDir()))
}
