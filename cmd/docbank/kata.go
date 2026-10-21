package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/kataevidence"
)

func newKataCommand() *cobra.Command {
	command := &cobra.Command{Use: "kata", Short: "Quote document passages into Kata issues", Long: "Create or append verified document passages with cite.\nUse context to read saved quotes and check their current source state.\nEnable [integrations.kata] before using these commands.", GroupID: groupInterfaces}
	var start, end, offset int
	var title, brief, issue, key, quote, citationFile, expectedVersion string
	var asJSON bool
	cite := &cobra.Command{Use: "cite <path-or-id>", Short: "Create a Kata issue or append one verified passage", Args: cobra.ExactArgs(1),
		Example: "  docbank kata cite /notes.txt --start 0 --end 20 --title 'Follow up' --idempotency-key follow-up-1\n  docbank kata cite id:12 --citation citation.json --issue docbank#abcd",
		Long:    "Quote at most 1,000 Unicode code points. --start is included; --end is excluded.\nOriginals must be UTF-8 text/JSON of at most 16 MiB. For a retained rendition,\npass its saved TextCitation JSON with --citation.\nNo processing is started.\nCreate with --title and --idempotency-key, or append with --issue.\nEnable [integrations.kata]; credentials use an env or file source.",
		RunE: func(cmd *cobra.Command, args []string) error {
			selection, err := kataCiteSelector(cmd, args[0], kataevidence.Selector{StartRune: start, EndRune: end, Quote: quote, ExpectedVersion: expectedVersion}, citationFile)
			if err != nil {
				return err
			}
			// Keep caller input stable: the daemon resolves paths after replay reconciliation.
			request := kataevidence.CiteRequest{Selector: selection, Title: title, Brief: brief, Issue: issue, IdempotencyKey: key}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			result, err := connection.CiteKataEvidence(cmd.Context(), request)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", result.QualifiedRef, result.Title); err != nil {
				return fmt.Errorf("writing Kata issue: %w", err)
			}
			return nil
		}}
	cite.Flags().IntVar(&start, "start", 0, "first Unicode code-point offset (included)")
	cite.Flags().IntVar(&end, "end", 0, "last Unicode code-point offset (excluded), at most 1000 after start")
	cite.Flags().StringVar(&expectedVersion, "expected-version", "", "require this content version; keep it unchanged on retries")
	cite.Flags().StringVar(&quote, "quote", "", "require these exact selected words")
	cite.Flags().StringVar(&citationFile, "citation", "", "JSON file containing an exact retained text citation")
	cite.Flags().StringVar(&title, "title", "", "title for a new Kata issue")
	cite.Flags().StringVar(&brief, "brief", "", "short context before a new issue's quotation")
	cite.Flags().StringVar(&issue, "issue", "", "append evidence to this existing issue instead of creating")
	cite.Flags().StringVar(&key, "idempotency-key", "", "stable key for retrying a new issue request")
	cite.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	contextCmd := &cobra.Command{Use: "context <issue>", Short: "Read an issue's Docbank evidence and current source states", Args: cobra.ExactArgs(1),
		Example: "  docbank kata context docbank#abcd --json\n  docbank kata context abcd --offset 10",
		Long:    "Read up to ten evidence entries, including saved quotes and exact references.\nReports available, changed, unavailable, unreachable, unsupported, or failed.\nUse the returned next_offset to continue. Reads never start processing.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if offset < 0 || offset > kataevidence.MaxContextOffset {
				return usageError(errors.New("offset is out of range"))
			}
			request := kataevidence.ContextRequest{Issue: args[0], Offset: offset}
			connection, err := daemonconn.Ensure(cmd.Context())
			var result kataevidence.Context
			if err == nil {
				result, err = connection.KataContext(cmd.Context(), request)
			}
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), result)
			}
			return writeKataContext(cmd.OutOrStdout(), result)
		}}
	contextCmd.Flags().IntVar(&offset, "offset", 0, "evidence entry offset from the preceding page")
	contextCmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	command.AddCommand(cite, contextCmd)
	return command
}

func kataCiteSelector(cmd *cobra.Command, arg string, selection kataevidence.Selector, citationFile string) (kataevidence.Selector, error) {
	selector, err := parseNodeSelector(arg)
	if err != nil {
		return selection, err
	}
	selection.NodeID, selection.Path = selector.id, selector.path
	if citationFile != "" {
		if cmd.Flags().Changed("start") || cmd.Flags().Changed("end") {
			return selection, usageError(errors.New("--citation supplies the range; omit --start and --end"))
		}
		citation, err := readKataCitation(citationFile)
		if err != nil {
			return selection, err
		}
		selection.Citation = &citation
		if selection.ExpectedVersion == "" {
			selection.ExpectedVersion = citation.ContentVersionID.String()
		}
		selection.StartRune, selection.EndRune = citation.Start, citation.End
	} else if !cmd.Flags().Changed("end") {
		return selection, usageError(errors.New("range requires --end"))
	}
	return selection, nil
}

func readKataCitation(path string) (document.TextCitation, error) {
	file, err := os.Open(path)
	if err != nil {
		return document.TextCitation{}, fmt.Errorf("reading citation file: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return document.TextCitation{}, usageError(errors.New("citation file exceeds 16 KiB or could not be read"))
	}
	var citation document.TextCitation
	if json.Unmarshal(data, &citation, json.RejectUnknownMembers(true)) != nil {
		return citation, usageError(errors.New("citation file must contain one exact text citation"))
	}
	return citation, nil
}

func init() { rootCmd.AddCommand(newKataCommand()) }

func writeKataContext(out io.Writer, result kataevidence.Context) error {
	if _, err := fmt.Fprintf(out, "%s %s\n", result.Issue.QualifiedRef, result.Issue.Title); err != nil {
		return fmt.Errorf("writing Kata context: %w", err)
	}
	for _, passage := range result.Passages {
		p := passage.Evidence.Reference.Docbank
		var err error
		if p != nil {
			_, err = fmt.Fprintf(out, "%s id:%d [%d,%d) version %s\n", passage.State, p.NodeID, p.StartRune, p.EndRune, p.ContentVersionID)
		} else if c := passage.Evidence.Reference.Rendition; c != nil {
			_, err = fmt.Fprintf(out, "%s id:%d [%d,%d) version %s\n", passage.State, c.NodeID, c.Start, c.End, c.ContentVersionID)
		} else {
			_, err = fmt.Fprintln(out, passage.State)
		}
		if err != nil {
			return fmt.Errorf("writing Kata passage: %w", err)
		}
		if passage.SavedQuote != "" {
			if _, err := fmt.Fprintln(out, passage.SavedQuote); err != nil {
				return fmt.Errorf("writing Kata quote: %w", err)
			}
		}
	}
	if result.NextOffset != 0 {
		if _, err := fmt.Fprintf(out, "Continue with --offset %d\n", result.NextOffset); err != nil {
			return fmt.Errorf("writing Kata continuation: %w", err)
		}
	}
	return nil
}
