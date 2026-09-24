package main

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
)

func writeAgentSessionBinding(output string, binding daemonconn.AgentSessionFile) (retErr error) {
	destination, err := prepareGetDestination(output, false)
	if err != nil {
		return err
	}
	stage, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-agent-session-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.removeAll()) }()
	file, stagedPath, err := stage.createFile(filepath.Base(destination))
	if err != nil {
		return err
	}
	raw, err := json.Marshal(binding)
	if err != nil {
		return errors.Join(err, file.Close())
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := secureAgentSessionStagedFile(stagedPath); err != nil {
		return err
	}
	return publishGetFile(stagedPath, destination, false)
}

func init() {
	root := &cobra.Command{Use: "agent-session", Short: "Issue or revoke bounded read-only daemon access"}
	issue := &cobra.Command{Use: "issue", Short: "Write a private read-only session file", Args: cobra.NoArgs}
	var sourceIDs []string
	var ttl time.Duration
	var output string
	issue.Flags().StringSliceVar(&sourceIDs, "source-id", nil, "Exact retained source version ID (repeatable)")
	issue.Flags().DurationVar(&ttl, "ttl", 15*time.Minute, "Session lifetime, at most one hour")
	issue.Flags().StringVar(&output, "output", "", "New private session file")
	issue.RunE = func(cmd *cobra.Command, _ []string) error {
		if len(sourceIDs) == 0 || output == "" || ttl <= 0 || ttl > time.Hour || ttl%time.Second != 0 {
			return usageError(errors.New("agent-session issue requires --source-id, --output and a whole-second --ttl of at most one hour"))
		}
		if _, err := prepareGetDestination(output, false); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		binding, id, err := connection.IssueAgentSession(cmd.Context(), sourceIDs, ttl)
		if err != nil {
			return err
		}
		if err := writeAgentSessionBinding(output, binding); err != nil {
			return errors.Join(err, connection.RevokeAgentSession(cmd.Context(), id))
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "session %s expires %s; file %s\n",
			id, binding.ExpiresAt.UTC().Format(time.RFC3339), output)
		if err != nil {
			return fmt.Errorf("printing agent session receipt: %w", err)
		}
		return nil
	}

	revoke := &cobra.Command{Use: "revoke <id>", Short: "Revoke a read-only session", Args: cobra.ExactArgs(1)}
	revoke.RunE = func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		if err := connection.RevokeAgentSession(cmd.Context(), args[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "revoked session %s\n", args[0])
		if err != nil {
			return fmt.Errorf("printing agent session revocation: %w", err)
		}
		return nil
	}
	root.AddCommand(issue, revoke)
	rootCmd.AddCommand(root)
}
