package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/bundle"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
)

const maxExportRequestBytes = 64 << 20

func readExportJSON[T any](path string) (T, error) {
	var request T
	file, err := os.Open(path)
	if err != nil {
		return request, fmt.Errorf("open export request: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxExportRequestBytes+1))
	if err != nil {
		return request, fmt.Errorf("read export request: %w", err)
	}
	if len(data) > maxExportRequestBytes {
		return request, errors.New("export request exceeds 64 MiB")
	}
	if err := json.Unmarshal(data, &request, json.RejectUnknownMembers(true)); err != nil {
		return request, fmt.Errorf("decode export request: %w", err)
	}
	return request, nil
}

func exportConnection(ctx context.Context) (*daemonconn.Connection, error) {
	connection, err := daemonconn.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to export daemon: %w", err)
	}
	return connection, nil
}

func requireExportID(value string) error {
	if !daemonconn.IsCanonicalUUIDv4(value) {
		return usageError(errors.New("export ID must be a canonical UUIDv4"))
	}
	return nil
}

func newExportLifecycleCommands() []*cobra.Command {
	source := &cobra.Command{Use: "source", Short: "Freeze an exact export source from JSON", Args: cobra.NoArgs}
	var sourceInput string
	source.Flags().StringVar(&sourceInput, "input", "", "Bounded source request JSON file")
	source.RunE = func(cmd *cobra.Command, _ []string) error {
		if sourceInput == "" {
			return usageError(errors.New("export source requires --input"))
		}
		request, err := readExportJSON[bundle.SourceRequest](sourceInput)
		if err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().CreateExportSource(cmd.Context(), &apiclient.CreateExportSourceRequestOptions{Body: &request})
		if err != nil {
			return fmt.Errorf("create export source: %w", err)
		}
		if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("write export source: %w", err)
		}
		return nil
	}

	plan := &cobra.Command{Use: "plan", Short: "Create a frozen export plan from JSON", Args: cobra.NoArgs}
	var planInput string
	plan.Flags().StringVar(&planInput, "input", "", "Bounded plan request JSON file")
	plan.RunE = func(cmd *cobra.Command, _ []string) error {
		if planInput == "" {
			return usageError(errors.New("export plan requires --input"))
		}
		request, err := readExportJSON[bundle.PlanRequest](planInput)
		if err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().CreateExportPlan(cmd.Context(), &apiclient.CreateExportPlanRequestOptions{Body: &request})
		if err != nil {
			return fmt.Errorf("create export plan: %w", err)
		}
		if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("write export plan: %w", err)
		}
		return nil
	}

	preview := &cobra.Command{Use: "preview <plan-id>", Short: "Inspect a frozen export plan", Args: cobra.ExactArgs(1)}
	preview.RunE = func(cmd *cobra.Command, args []string) error {
		if err := requireExportID(args[0]); err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().GetExportPlanPreview(cmd.Context(), &apiclient.GetExportPlanPreviewRequestOptions{
			PathParams: &apiclient.GetExportPlanPreviewPath{ID: args[0]},
		})
		if err != nil {
			return fmt.Errorf("read export plan preview: %w", err)
		}
		if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("write export plan preview: %w", err)
		}
		return nil
	}

	start := &cobra.Command{Use: "start", Short: "Start a durable export job from JSON", Args: cobra.NoArgs}
	var jobInput string
	start.Flags().StringVar(&jobInput, "input", "", "Bounded job request JSON file")
	start.RunE = func(cmd *cobra.Command, _ []string) error {
		if jobInput == "" {
			return usageError(errors.New("export start requires --input"))
		}
		request, err := readExportJSON[bundle.JobRequest](jobInput)
		if err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().CreateExportJob(cmd.Context(), &apiclient.CreateExportJobRequestOptions{Body: &request})
		if err != nil {
			return fmt.Errorf("start export job: %w", err)
		}
		if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("write export job: %w", err)
		}
		return nil
	}

	status := &cobra.Command{Use: "status <job-id>", Short: "Read one export job and its retained receipt", Args: cobra.ExactArgs(1)}
	status.RunE = func(cmd *cobra.Command, args []string) error {
		if err := requireExportID(args[0]); err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		result, err := connection.API().GetExportJob(cmd.Context(), &apiclient.GetExportJobRequestOptions{
			PathParams: &apiclient.GetExportJobPath{ID: args[0]},
		})
		if err != nil {
			return fmt.Errorf("read export job: %w", err)
		}
		if err := json.MarshalWrite(cmd.OutOrStdout(), result); err != nil {
			return fmt.Errorf("write export job: %w", err)
		}
		return nil
	}

	cancel := &cobra.Command{Use: "cancel <job-id>", Short: "Cancel one queued or running export job", Args: cobra.ExactArgs(1)}
	cancel.RunE = func(cmd *cobra.Command, args []string) error {
		if err := requireExportID(args[0]); err != nil {
			return err
		}
		connection, err := exportConnection(cmd.Context())
		if err != nil {
			return err
		}
		if _, err := connection.API().CancelExportJob(cmd.Context(), &apiclient.CancelExportJobRequestOptions{
			PathParams: &apiclient.CancelExportJobPath{ID: args[0]},
		}); err != nil {
			return fmt.Errorf("cancel export job: %w", err)
		}
		return nil
	}
	return []*cobra.Command{source, plan, preview, start, status, cancel}
}
