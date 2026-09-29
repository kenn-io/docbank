package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/daemonconn"
)

var (
	photoImportDestination string
	photoImportJSON        bool
	photoImportRawAssetID  string
	photoImportRawFileID   string
	photoImportRawPath     string
	photoImportRawHash     string
	photoImportGroupKey    string
	photoImportRevision    int64
)

var photoImportCmd = &cobra.Command{
	Use:   "import <source-root> [destination]",
	Short: "Import grouped camera files from a folder",
	Args:  cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		destination := photoImportDestination
		if len(args) == 2 {
			destination = args[1]
		}
		if destination == "" {
			destination = "/"
		}
		choice := photoImportChoiceFromFlags()
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.StartPhotoImport(cmd.Context(), args[0], destination, choice)
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

var photoImportsCmd = &cobra.Command{
	Use:   "imports",
	Short: "Inspect or cancel photo imports",
	Args:  cobra.NoArgs,
	RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
}

var photoImportShowCmd = &cobra.Command{
	Use:   "show <run-id>",
	Short: "Show one photo import run",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		run, err := connection.PhotoImport(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if err := maybeChoosePhotoImport(cmd, connection, &run); err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

var photoImportCancelCmd = &cobra.Command{
	Use:   "cancel <run-id>",
	Short: "Request cancellation of a photo import",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		revision := photoImportRevision
		if revision == 0 {
			run, getErr := connection.PhotoImport(cmd.Context(), args[0])
			if getErr != nil {
				return getErr
			}
			revision = run.Revision
		}
		run, err := connection.CancelPhotoImport(cmd.Context(), args[0], revision)
		if err != nil {
			return err
		}
		return writePhotoImportOutput(cmd, run)
	},
}

func photoImportChoiceFromFlags() *api.PhotoImportChoice {
	if photoImportRawAssetID == "" && photoImportRawFileID == "" && photoImportRawPath == "" && photoImportRawHash == "" {
		return nil
	}
	return &api.PhotoImportChoice{GroupKey: photoImportGroupKey, RawAssetID: photoImportRawAssetID, RawFileID: photoImportRawFileID,
		RawSourcePath: photoImportRawPath, RawBlobHash: photoImportRawHash, AssetRevision: photoImportRevision}
}

func writePhotoImportOutput(cmd *cobra.Command, run api.PhotoImportRun) error {
	if photoImportJSON {
		return writeCLIJSON(cmd.OutOrStdout(), run)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "run: %s\nstate: %s\nprogress: %d/%d groups\nadded: %d\nskipped: %d\nfailed: %d\nambiguous: %d\n",
		run.ID, run.State, run.CompletedGroups, run.TotalGroups, run.AddedGroups, run.SkippedGroups, run.FailedGroups, run.AmbiguousGroups)
	if err != nil {
		return fmt.Errorf("writing photo import output: %w", err)
	}
	for index, ambiguity := range run.Ambiguities {
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "ambiguity %d group-key: %s\n", index+1, ambiguity.GroupKey); err != nil {
			return fmt.Errorf("writing photo import ambiguity: %w", err)
		}
		for candidateIndex, candidate := range ambiguity.Candidates {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %d) %s\n", candidateIndex+1, candidate.SourcePath); err != nil {
				return fmt.Errorf("writing photo import candidate: %w", err)
			}
		}
	}
	return nil
}

func maybeChoosePhotoImport(cmd *cobra.Command, connection *daemonconn.Connection, run *api.PhotoImportRun) error {
	if !photoImportInteractive(cmd) {
		return nil
	}
	for run.State == "ambiguous" && len(run.Ambiguities) > 0 {
		ambiguity := run.Ambiguities[0]
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Choose a RAW to pair:")
		for index, candidate := range ambiguity.Candidates {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%d) %s\n", index+1, candidate.SourcePath)
		}
		_, _ = fmt.Fprint(cmd.OutOrStdout(), "RAW number: ")
		choiceReader := bufio.NewReader(cmd.InOrStdin())
		line, err := choiceReader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("reading photo import choice: %w", err)
		}
		selection, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || selection < 1 || selection > len(ambiguity.Candidates) {
			return errors.New("photo import RAW choice is out of range")
		}
		candidate := ambiguity.Candidates[selection-1]
		choice := &api.PhotoImportChoice{GroupKey: ambiguity.GroupKey, RawAssetID: candidate.AssetID,
			RawFileID: candidate.FileID, AssetRevision: candidate.Revision,
			RawSourcePath: candidate.SourcePath, RawBlobHash: candidate.BlobHash}
		newRun, err := connection.StartPhotoImport(cmd.Context(), run.SourceRoot, run.Destination, choice)
		if err != nil {
			return err
		}
		*run = newRun
		if err := waitPhotoImportRun(cmd.Context(), connection, run); err != nil {
			return err
		}
	}
	return nil
}

func waitPhotoImportRun(ctx context.Context, connection *daemonconn.Connection, run *api.PhotoImportRun) error {
	if run.FinishedAt != "" {
		return nil
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			latest, err := connection.PhotoImport(ctx, run.ID)
			if err != nil {
				return err
			}
			*run = latest
			if run.FinishedAt != "" {
				return nil
			}
		}
	}
}

func photoImportInteractive(cmd *cobra.Command) bool {
	if photoImportJSON {
		return false
	}
	in, inOK := cmd.InOrStdin().(*os.File)
	out, outOK := cmd.OutOrStdout().(*os.File)
	return inOK && outOK && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

func init() {
	photosCmd.AddCommand(photoImportCmd, photoImportsCmd)
	photoImportsCmd.AddCommand(photoImportShowCmd, photoImportCancelCmd)
	photoImportCmd.Flags().StringVar(&photoImportDestination, "destination", "/", "vault destination path")
	for _, command := range []*cobra.Command{photoImportCmd, photoImportShowCmd, photoImportCancelCmd} {
		command.Flags().BoolVar(&photoImportJSON, "json", false, "emit machine-readable JSON")
	}
	photoImportCmd.Flags().StringVar(&photoImportRawAssetID, "raw-asset-id", "", "candidate RAW asset ID")
	photoImportCmd.Flags().StringVar(&photoImportRawFileID, "raw-file-id", "", "candidate RAW file ID")
	photoImportCmd.Flags().StringVar(&photoImportRawPath, "raw-source-path", "", "candidate RAW source path")
	photoImportCmd.Flags().StringVar(&photoImportRawHash, "raw-blob-hash", "", "candidate RAW blob hash")
	photoImportCmd.Flags().StringVar(&photoImportGroupKey, "group-key", "", "ambiguous import group key")
	photoImportCmd.Flags().Int64Var(&photoImportRevision, "revision", 0, "candidate RAW asset revision")
	photoImportCancelCmd.Flags().Int64Var(&photoImportRevision, "revision", 0, "expected run revision")
}
