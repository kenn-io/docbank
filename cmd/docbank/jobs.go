package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

var jobsJSON bool

var jobsCmd = &cobra.Command{
	Use:   "jobs",
	Short: "Show daemon background-job status",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		items, err := c.API().ListJobs(cmd.Context())

		if err != nil {
			return err
		}
		if jobsJSON {
			return writeJobsJSON(cmd.OutOrStdout(), items)
		}
		if items.LaneControlsError != "" {
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"warning: lane controls unavailable: %s\n", items.LaneControlsError)
		}
		return writeJobs(cmd.OutOrStdout(), items.Items)
	},
}

var jobsShowJSON bool

var jobsShowCmd = &cobra.Command{
	Use:   "show <operation-id>",
	Short: "Show one durable storage operation and its receipt",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !daemonconn.IsCanonicalUUIDv4(args[0]) {
			return usageError(errors.New("operation ID must be a canonical UUIDv4"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := c.API().GetStorageOperation(cmd.Context(), &apiclient.GetStorageOperationRequestOptions{PathParams: &apiclient.GetStorageOperationPath{OperationID: args[0]}})

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
		if !daemonconn.IsCanonicalUUIDv4(args[0]) {
			return usageError(errors.New("operation ID must be a canonical UUIDv4"))
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		operation, err := c.API().CancelStorageOperation(cmd.Context(), &apiclient.CancelStorageOperationRequestOptions{PathParams: &apiclient.CancelStorageOperationPath{OperationID: args[0]}})

		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(),
			"cancellation requested for %s (%s)\n", operation.ID, operation.State)
		return nil
	},
}

func writeJobsJSON(w io.Writer, list *api.JobList) error {
	enc := jsontext.NewEncoder(w, jsontext.WithIndent("  "))
	if err := json.MarshalEncode(enc, list); err != nil {
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
	if _, err := fmt.Fprintln(tw, "NAME\tSTATUS\tCONTROL\tSTARTED\tFINISHED\tERROR"); err != nil {
		return fmt.Errorf("writing job list header: %w", err)
	}
	for _, job := range items {
		control := "read-only"
		if job.Controllable {
			control = "active"
			if job.Paused {
				control = "paused"
			}
			if job.CanSetConcurrency {
				control += fmt.Sprintf(" limit=%d", job.Concurrency)
			}
			if job.Kind != "" {
				control += " lane=" + job.Kind
			}
		}
		finished, problem := job.FinishedAt, job.Error
		if finished == "" {
			finished = "-"
		}
		if problem == "" {
			problem = "-"
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			job.Name, job.Status, control, job.StartedAt, finished, problem); err != nil {
			return fmt.Errorf("writing job list row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing job list: %w", err)
	}
	return nil
}

func jobsControlCommand(action string) *cobra.Command {
	use := action + " <lane>"
	count := 1
	example := "  docbank jobs " + action + " photo_import"
	if action == "concurrency" {
		use += " <limit>"
		count = 2
		example = "  docbank jobs concurrency derive:visual-previews 3"
	}
	return &cobra.Command{
		Use: use, Short: "Change durable background lane controls", Args: cobra.ExactArgs(count),
		Example: example,
		RunE: func(cmd *cobra.Command, args []string) error {
			lane := args[0]
			limit := 0
			if action == "concurrency" {
				var err error
				limit, err = strconv.Atoi(args[1])
				if err != nil {
					return usageError(fmt.Errorf("concurrency must be an integer: %w", err))
				}
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			control, err := c.API().GetLaneControl(cmd.Context(),
				&apiclient.GetLaneControlRequestOptions{
					PathParams: &apiclient.GetLaneControlPath{Lane: lane},
				})
			if err != nil {
				return err
			}
			request := api.SetLaneControlRequest{
				Paused: control.Paused, Concurrency: control.Concurrency,
			}
			if action == "concurrency" {
				request.Concurrency = limit
			} else {
				request.Paused = action == "pause"
			}
			ifMatch := strconv.FormatInt(control.Revision, 10)
			updated, err := c.API().SetLaneControl(cmd.Context(),
				&apiclient.SetLaneControlRequestOptions{
					PathParams: &apiclient.SetLaneControlPath{Lane: lane}, Body: &request,
					Header: &apiclient.SetLaneControlHeaders{IfMatch: ifMatch},
				})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s paused=%t concurrency=%d\n",
				updated.Lane, updated.Paused, updated.Concurrency)
			if err != nil {
				return fmt.Errorf("writing lane control: %w", err)
			}
			return nil
		},
	}
}

func init() {
	jobsCmd.Flags().BoolVar(&jobsJSON, "json", false, "emit machine-readable JSON")
	jobsShowCmd.Flags().BoolVar(&jobsShowJSON, "json", false, "emit machine-readable JSON")
	jobsCmd.AddCommand(jobsShowCmd, jobsCancelCmd)
	jobsCmd.AddCommand(jobsControlCommand("pause"), jobsControlCommand("resume"))
	jobsCmd.AddCommand(jobsControlCommand("concurrency"))
	rootCmd.AddCommand(jobsCmd)
}
