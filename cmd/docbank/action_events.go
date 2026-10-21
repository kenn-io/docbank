package main

import (
	"context"
	"os"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/apiclient"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/telemetry"
	"golang.org/x/term"
)

func agentRun() bool {
	return os.Getenv("CLAUDECODE") != "" || os.Getenv("CODEX_THREAD_ID") != ""
}

func reportAction(cmd *cobra.Command, c *daemonconn.Connection, event, searchType, kind string, agent bool) {
	terminal := false
	for _, stream := range []any{cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()} {
		if file, ok := stream.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
			terminal = true
			break
		}
	}
	if !actionReportingAllowed(terminal, agent) {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 100*time.Millisecond)
	defer cancel()
	_ = postAction(ctx, c, event, "cli", searchType, kind)
}

func postAction(ctx context.Context, c *daemonconn.Connection, event, surface, searchType, kind string) error {
	searchType = telemetry.SearchType(searchType)
	return postTelemetryEvent(ctx, c, event, &apiclient.TelemetryEventProperties{Surface: surface, SearchType: searchType, Kind: kind})
}

func postTelemetryEvent(ctx context.Context, c *daemonconn.Connection, event string, properties *apiclient.TelemetryEventProperties) error {
	_, err := c.API().ReportTelemetryEvent(ctx, &apiclient.ReportTelemetryEventRequestOptions{Body: &apiclient.ReportTelemetryEventBody{
		Event: event, Properties: properties,
	}})
	return err
}

func actionReportingAllowed(terminal, agent bool) bool { return terminal && !agent && !agentRun() }

// reportFailure sends error_shown for a command that ended in err.
func reportFailure(cmd *cobra.Command, c *daemonconn.Connection, kind string, err error, agent bool) {
	if err != nil {
		reportAction(cmd, c, "error_shown", "", kind, agent)
	}
}

func reportSearch(cmd *cobra.Command, c *daemonconn.Connection, searchType string, empty, agent bool) {
	reportAction(cmd, c, "search_run", searchType, "", agent)
	if empty {
		reportAction(cmd, c, "search_empty", searchType, "", agent)
	}
}
