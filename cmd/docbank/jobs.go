package main

import (
	"encoding/json"
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

func writeJobsJSON(w io.Writer, items []api.Job) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(api.JobList{Items: items}); err != nil {
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
	rootCmd.AddCommand(jobsCmd)
}
