package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
)

var jobsJSON bool

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Show daemon background-job status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		items, err := c.Jobs(cmd.Context())
		if err != nil {
			return err
		}
		if jobsJSON {
			return writeJobsJSON(cmd.OutOrStdout(), items)
		}
		return writeJobs(cmd.OutOrStdout(), items)
	},
}

var jobsShowJSON bool

var jobsShowCmd = &cobra.Command{
	Use:   "show <operation-id>",
	Short: "Show one durable storage operation and its receipt",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !client.IsCanonicalUUIDv4(args[0]) {
			return usageError(errors.New("operation ID must be a canonical UUIDv4"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := c.StorageOperation(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if jobsShowJSON {
			encoder := jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  "))
			if err := json.MarshalEncode(encoder, operation); err != nil {
				return fmt.Errorf("writing storage operation JSON: %w", err)
			}
			return nil
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"%s %s: %d/%d object(s), %d byte(s) copied\n",
			operation.Kind, operation.State, operation.CompletedObjects,
			operation.TotalObjects, operation.CopiedBytes)
		if operation.Error != "" {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "error: %s\n", operation.Error)
		}
		return nil
	},
}

var jobsCancelCmd = &cobra.Command{
	Use:   "cancel <operation-id>",
	Short: "Request cancellation at the next durable object boundary",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !client.IsCanonicalUUIDv4(args[0]) {
			return usageError(errors.New("operation ID must be a canonical UUIDv4"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := c.CancelStorageOperation(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"cancellation requested for %s (%s)\n", operation.ID, operation.State)
		return nil
	},
}

func writeJobsJSON(w io.Writer, items []api.Job) error {
	enc := jsontext.NewEncoder(w, jsontext.WithIndent("  "))
	if err := json.MarshalEncode(enc, api.JobList{Items: items}); err != nil {
		return fmt.Errorf("writing job status JSON: %w", err)
	}
	return nil
}

func writeJobs(w io.Writer, items []api.Job) error {
	if len(items) == 0 {
		if _, err := fmt.Fprintln(w, "no background jobs"); err != nil {
			return fmt.Errorf("writing empty job list: %w", err)
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tSTATUS\tSTARTED\tFINISHED\tERROR"); err != nil {
		return fmt.Errorf("writing job list header: %w", err)
	}
	for _, job := range items {
		finished, problem := job.FinishedAt, job.Error
		if finished == "" {
			finished = "-"
		}
		if problem == "" {
			problem = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			job.Name, job.Status, job.StartedAt, finished, problem); err != nil {
			return fmt.Errorf("writing job list row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing job list: %w", err)
	}
	return nil
}

func init() {
	jobsCmd.Flags().BoolVar(&jobsJSON, "json", false, "emit machine-readable JSON")
	jobsShowCmd.Flags().BoolVar(&jobsShowJSON, "json", false, "emit machine-readable JSON")
	jobsCmd.AddCommand(jobsShowCmd, jobsCancelCmd)
	rootCmd.AddCommand(jobsCmd)
}
