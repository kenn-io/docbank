package main

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Cobra's Deprecated field prints to stdout, which would corrupt JSON and cat
// output. Keep replacement metadata here and print the notice on stderr from
// the process boundary before Cobra parses arguments instead.
const legacyCommandReplacement = "docbank.legacy-replacement"

func warnLegacyCommand(cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	if replacement := cmd.Annotations[legacyCommandReplacement]; replacement != "" {
		cmd.PrintErrf("Command %q is deprecated, use '%s' instead\n", cmd.Name(), replacement)
	}
}

var documentCmd = &cobra.Command{
	Use: "document", Short: "Import, browse, and maintain documents and their history",
	Long: `Work with files and directories in the virtual document tree.
Start with add, browse with list or tree, inspect with stat, and download with
get. Content changes retain immutable versions. remove moves nodes to trash;
restore brings them back. Select nodes by absolute virtual path or id:N.`,
	GroupID: groupDocuments,
	RunE:    commandGroupHelp,
}

var vaultCmd = &cobra.Command{
	Use: "vault", Short: "Identify, verify, and reclaim space in the selected vault",
	Long: `info identifies the vault selected by DOCBANK_HOME (default ~/.docbank).
verify checks metadata and stored bytes; gc previews unreachable blobs and
requires --run to reclaim them. Data commands start the daemon if needed.`,
	GroupID: groupOperations,
	RunE:    commandGroupHelp,
}

var emailCmd = &cobra.Command{
	Use: "email", Short: "Import mailboxes and inspect or render retained email",
	Long: `mailbox imports explicitly selected MBOX, Takeout ZIP, or EML sources.
documents inspects attachment relationships and publication receipts.
pdf renders one exact retained EML content version as a verified local PDF.`,
	GroupID: groupSources,
	RunE:    commandGroupHelp,
}

func init() {
	rootCmd.AddCommand(documentCmd, vaultCmd, emailCmd)
}

// Register after the owning init has installed all flags and children. Keep the
// original command and handlers canonical; compatibility definitions share flag
// values with those handlers but never Cobra's private execution/tree state.
func registerGroupedCommand(parent, command *cobra.Command, name string) {
	canonical := rootCmd.Name() + " " + name
	if parent != rootCmd {
		canonical = rootCmd.Name() + " " + parent.Name() + " " + name
	}
	legacy := cloneLegacyCommand(command, canonical)
	legacy.Hidden = true
	rootCmd.AddCommand(legacy)
	_, synopsis, _ := strings.Cut(command.Use, " ")
	command.Use = name
	if synopsis != "" {
		command.Use += " " + synopsis
	}
	if parent != rootCmd {
		command.GroupID = ""
	}
	parent.AddCommand(command)
}

func cloneLegacyCommand(source *cobra.Command, canonical string) *cobra.Command {
	target := &cobra.Command{
		Use: source.Use, Aliases: slices.Clone(source.Aliases),
		Short: source.Short, Long: source.Long, Example: source.Example,
		Args: source.Args, GroupID: source.GroupID,
		ValidArgs: slices.Clone(source.ValidArgs), ValidArgsFunction: source.ValidArgsFunction,
		ArgAliases: slices.Clone(source.ArgAliases), Annotations: maps.Clone(source.Annotations),
		PreRun: source.PreRun, PreRunE: source.PreRunE,
		PersistentPreRun: source.PersistentPreRun, PersistentPreRunE: source.PersistentPreRunE,
		Run: source.Run, RunE: source.RunE,
		PostRun: source.PostRun, PostRunE: source.PostRunE,
		PersistentPostRun: source.PersistentPostRun, PersistentPostRunE: source.PersistentPostRunE,
		SilenceUsage: source.SilenceUsage, SilenceErrors: source.SilenceErrors,
		Hidden: source.Hidden,
	}
	if target.Annotations == nil {
		target.Annotations = make(map[string]string)
	}
	target.Annotations[legacyCommandReplacement] = canonical
	target.Long = "Deprecated: use '" + canonical + "' instead.\n\n" + source.Long
	copyFlags := func(from, to *pflag.FlagSet) {
		from.VisitAll(func(flag *pflag.Flag) {
			copied := *flag
			copied.Changed = false
			if flag.Annotations != nil {
				copied.Annotations = make(map[string][]string, len(flag.Annotations))
				for key, values := range flag.Annotations {
					copied.Annotations[key] = slices.Clone(values)
				}
			}
			to.AddFlag(&copied)
		})
	}
	copyFlags(source.LocalNonPersistentFlags(), target.Flags())
	copyFlags(source.PersistentFlags(), target.PersistentFlags())
	for _, child := range source.Commands() {
		target.AddCommand(cloneLegacyCommand(child, canonical+" "+child.Name()))
	}
	return target
}

// Cobra's default group-only command accepts stray arguments when no handler
// runs. Keep discovery useful while treating unknown children as usage errors.
func commandGroupHelp(cmd *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usageError(fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	return cmd.Help()
}
