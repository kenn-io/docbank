package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	peopleLimit            int
	peopleCursor           string
	peopleRevision         int64
	peopleAbsorbedRevision int64
	peopleMergeOperationID string
	peopleSplitOperationID string
	peopleDisplayName      string
	peopleIdentityIDs      []string
	peopleAssignmentIDs    []string
	peopleExternal         []string
)

var peopleCmd = &cobra.Command{
	Use:   "people",
	Short: "Manage canonical people",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var peopleListCmd = &cobra.Command{
	Use:   "list [query]",
	Short: "List or search active people",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		query := ""
		if len(args) == 1 {
			query = args[0]
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := c.People(cmd.Context(), query, peopleCursor, peopleLimit)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), page)
	},
}

var peopleShowCmd = &cobra.Command{
	Use:   "show <person-id>",
	Short: "Show one person and its selectors",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		person, err := c.Person(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), person)
	},
}

var peopleCreateCmd = &cobra.Command{
	Use:   "create <display-name>",
	Short: "Create one canonical person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		person, err := c.CreatePerson(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), person)
	},
}

var peopleRenameCmd = &cobra.Command{
	Use:   "rename <person-id> <display-name>",
	Short: "Rename one canonical person",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePeopleRevision(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		person, err := c.RenamePerson(cmd.Context(), args[0], peopleRevision, args[1])
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), person)
	},
}

var peopleRetireCmd = &cobra.Command{
	Use:   "retire <person-id>",
	Short: "Retire one canonical person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePeopleRevision(); err != nil {
			return err
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		person, err := c.RetirePerson(cmd.Context(), args[0], peopleRevision)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), person)
	},
}

var peopleMergeCmd = &cobra.Command{
	Use:   "merge <survivor-id> <absorbed-id>",
	Short: "Merge one person into another",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePeopleRevision(); err != nil {
			return err
		}
		if peopleAbsorbedRevision < 1 {
			return usageError(errors.New("--absorbed-revision must be a positive integer"))
		}
		operationID := peopleMergeOperationID
		if operationID == "" {
			operationID = uuid.NewV4().String()
			// Printed before the request so a retry after a lost response can replay the receipt.
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "operation ID %s (pass --operation-id %s to retry)\n", operationID, operationID); err != nil {
				return fmt.Errorf("writing merge operation ID: %w", err)
			}
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.MergePerson(cmd.Context(), args[0], peopleRevision, args[1], peopleAbsorbedRevision, operationID)
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	},
}

var peopleSplitCmd = &cobra.Command{
	Use:   "split <person-id>",
	Short: "Split selected members into a new person",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := requirePeopleRevision(); err != nil {
			return err
		}
		if peopleDisplayName == "" {
			return usageError(errors.New("--display-name is required"))
		}
		if len(peopleIdentityIDs)+len(peopleAssignmentIDs)+len(peopleExternal) == 0 {
			return usageError(errors.New("split requires at least one identity, assignment, or external identity"))
		}
		external := make([]api.PersonExternalUID, 0, len(peopleExternal))
		for _, raw := range peopleExternal {
			var identity api.PersonExternalUID
			if err := json.Unmarshal([]byte(raw), &identity, json.RejectUnknownMembers(true)); err != nil {
				return usageError(errors.New("--external must be a JSON object with system, archive_id, and uid"))
			}
			external = append(external, identity)
		}
		operationID := peopleSplitOperationID
		if operationID == "" {
			operationID = uuid.NewV4().String()
			// Printed before the request so a retry after a lost response can replay the receipt.
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "operation ID %s (pass --operation-id %s to retry)\n", operationID, operationID); err != nil {
				return fmt.Errorf("writing split operation ID: %w", err)
			}
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		receipt, err := c.SplitPerson(cmd.Context(), args[0], peopleRevision, api.SplitPersonRequest{
			OperationID: operationID, DisplayName: peopleDisplayName, IdentityIDs: peopleIdentityIDs,
			AssignmentIDs: peopleAssignmentIDs, ExternalIdentities: external,
		})
		if err != nil {
			return err
		}
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	},
}

func requirePeopleRevision() error {
	if peopleRevision < 1 {
		return usageError(errors.New("--revision must be a positive integer"))
	}
	return nil
}

func init() {
	peopleCmd.AddCommand(peopleListCmd, peopleShowCmd, peopleCreateCmd, peopleRenameCmd,
		peopleRetireCmd, peopleMergeCmd, peopleSplitCmd)
	rootCmd.AddCommand(peopleCmd)
	peopleListCmd.Flags().IntVar(&peopleLimit, "limit", 100, "maximum number of results")
	peopleListCmd.Flags().StringVar(&peopleCursor, "cursor", "", "opaque continuation cursor")
	for _, command := range []*cobra.Command{peopleRenameCmd, peopleRetireCmd, peopleMergeCmd, peopleSplitCmd} {
		command.Flags().Int64Var(&peopleRevision, "revision", 0, "expected person revision")
		_ = command.MarkFlagRequired("revision")
	}
	peopleMergeCmd.Flags().Int64Var(&peopleAbsorbedRevision, "absorbed-revision", 0, "expected absorbed person revision")
	_ = peopleMergeCmd.MarkFlagRequired("absorbed-revision")
	peopleMergeCmd.Flags().StringVar(&peopleMergeOperationID, "operation-id", "", "merge operation UUID")
	peopleSplitCmd.Flags().StringVar(&peopleSplitOperationID, "operation-id", "", "split operation UUID")
	peopleSplitCmd.Flags().StringVar(&peopleDisplayName, "display-name", "", "new person's display name")
	peopleSplitCmd.Flags().StringSliceVar(&peopleIdentityIDs, "identity", nil, "identity UUID to move")
	peopleSplitCmd.Flags().StringSliceVar(&peopleAssignmentIDs, "assignment", nil, "custodian assignment UUID to move")
	peopleSplitCmd.Flags().StringArrayVar(&peopleExternal, "external", nil, "external identity JSON object to move")
}
