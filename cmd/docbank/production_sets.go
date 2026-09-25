package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/redaction"
	"go.kenn.io/docbank/internal/daemonconn"
	"uuid"
)

func newProductionRecipesCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "recipes", Short: "List verified production renderer recipes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			catalog, err := connection.ProductionRecipes(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), catalog)
			}
			for _, item := range catalog.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s dpi=%d sha256=%s\n",
					item.ID, item.Recipe.DPI, item.SHA256); err != nil {
					return fmt.Errorf("writing production recipes: %w", err)
				}
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionSetCreateCommand() *cobra.Command {
	var name, instructionsFile, operationID string
	var asJSON bool
	command := &cobra.Command{Use: "create", Short: "Create or replay a production set", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return usageError(errors.New("--name is required"))
			}
			effectiveOperationID := operationID
			if effectiveOperationID == "" {
				effectiveOperationID = uuid.NewV4().String()
			}
			var instructions string
			if instructionsFile != "" {
				value, err := readProductionInstructions(cmd, instructionsFile)
				if err != nil {
					return err
				}
				instructions = value
			}
			request := redaction.CreateRequest{OperationID: effectiveOperationID, Name: name,
				Instructions: instructions}
			if err := redaction.ValidateCreateRequest(request); err != nil {
				return usageError(err)
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			created, err := connection.CreateProductionSet(cmd.Context(), request)
			if err != nil {
				return fmt.Errorf("creating production set (operation %s): %w", effectiveOperationID, err)
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), created)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d operation_id=%s\n",
				created.Set.ID, created.Draft.Revision, created.Draft.ETag, effectiveOperationID)
			if err != nil {
				return fmt.Errorf("writing production set: %w", err)
			}
			return nil
		}}
	command.Flags().StringVar(&name, "name", "", "production set name")
	command.Flags().StringVar(&instructionsFile, "instructions-file", "", "UTF-8 instructions file; use - for stdin")
	command.Flags().StringVar(&operationID, "operation-id", "", "idempotency UUID; generated when omitted")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func readProductionInstructions(cmd *cobra.Command, path string) (string, error) {
	reader := cmd.InOrStdin()
	if path != "-" {
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("opening production instructions: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	data, err := io.ReadAll(io.LimitReader(reader, redaction.MaxInstructionsBytes+1))
	if err != nil {
		return "", fmt.Errorf("reading production instructions: %w", err)
	}
	if len(data) > redaction.MaxInstructionsBytes {
		return "", usageError(fmt.Errorf("production instructions exceed %d bytes", redaction.MaxInstructionsBytes))
	}
	return string(data), nil
}

func newProductionSetListCommand() *cobra.Command {
	var cursor string
	var limit int
	var asJSON bool
	command := &cobra.Command{Use: "list", Short: "List one bounded page of production sets", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > redaction.MaxProductionPage || len(cursor) > 2048 {
				return usageError(errors.New("--limit must be 1-200 and --cursor at most 2048 bytes"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			page, err := connection.ProductionSets(cmd.Context(), cursor, limit)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), page)
			}
			for _, set := range page.Items {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d %s\n",
					set.ID, set.HeadRevision, set.Name); err != nil {
					return fmt.Errorf("writing production sets: %w", err)
				}
			}
			if page.NextCursor != "" {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "next_cursor: %s\n", page.NextCursor); err != nil {
					return fmt.Errorf("writing production set cursor: %w", err)
				}
			}
			return nil
		}}
	command.Flags().StringVar(&cursor, "cursor", "", "continue after a production set page cursor")
	command.Flags().IntVar(&limit, "limit", 100, "maximum sets to list (1-200)")
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionSetShowCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "show <set-id>", Short: "Show one production set", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			set, err := connection.ProductionSet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), set)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d %s\n",
				set.ID, set.HeadRevision, set.Name)
			if err != nil {
				return fmt.Errorf("writing production set: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func newProductionDraftShowCommand() *cobra.Command {
	var asJSON bool
	command := &cobra.Command{Use: "show <set-id> <revision>", Short: "Show one exact production draft", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			revision, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil || revision < 1 {
				return usageError(errors.New("revision must be a positive integer"))
			}
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			draft, err := connection.ProductionDraft(cmd.Context(), args[0], revision)
			if err != nil {
				return err
			}
			if asJSON {
				return writeCLIJSON(cmd.OutOrStdout(), draft)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s revision=%d etag=%d state=%s\n",
				draft.SetID, draft.Revision, draft.ETag, draft.State)
			if err != nil {
				return fmt.Errorf("writing production draft: %w", err)
			}
			return nil
		}}
	command.Flags().BoolVar(&asJSON, "json", false, "emit machine-readable JSON")
	return command
}

func init() {
	production := &cobra.Command{Use: "production", Short: "Create and inspect production work"}
	sets := &cobra.Command{Use: "sets", Short: "Create and inspect production sets"}
	drafts := &cobra.Command{Use: "drafts", Short: "Inspect exact production draft revisions"}
	sets.AddCommand(newProductionSetCreateCommand(), newProductionSetListCommand(), newProductionSetShowCommand())
	drafts.AddCommand(newProductionDraftShowCommand())
	production.AddCommand(newProductionRecipesCommand(), sets, drafts)
	rootCmd.AddCommand(production)
}
