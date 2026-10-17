package main

import (
	"context"
	"os"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/telemetry"
)

func interactiveAction(cmd *cobra.Command) bool {
	if flag := cmd.Flags().Lookup("agent"); flag != nil && flag.Value.String() == "true" {
		return false
	}
	if f, ok := cmd.InOrStdin().(*os.File); ok {
		return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
	}
	return false
}

func reportAction(cmd *cobra.Command, c *daemonconn.Connection, event, value string) {
	if !interactiveAction(cmd) {
		return
	}
	props := &apiclient.TelemetryEventProperties{Surface: "cli"}
	if event == telemetry.EventSearchRun {
		props.SearchType = telemetry.SearchType(value)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), 100*time.Millisecond)
	defer cancel()
	_, _ = c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: &apiclient.ReportTelemetryEventBody{Event: event, Properties: props}})
}

func reportOpened(cmd *cobra.Command, c *daemonconn.Connection) {
	reportAction(cmd, c, telemetry.EventDocumentOpened, "")
}

// writeOpenedJSON prints an inspected record, then counts the deliberate open.
func writeOpenedJSON(cmd *cobra.Command, c *daemonconn.Connection, v any) error {
	if err := writeCLIJSON(cmd.OutOrStdout(), v); err != nil {
		return err
	}
	reportOpened(cmd, c)
	return nil
}

// actionContext attributes work an interactive command starts to the CLI.
func actionContext(cmd *cobra.Command) context.Context {
	if !interactiveAction(cmd) {
		return cmd.Context()
	}
	return daemonconn.WithActionSurface(cmd.Context(), "cli")
}
