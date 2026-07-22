package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/store"
)

var (
	getJSON      bool
	getOverwrite bool
	getProgress  string
)

type getReceipt struct {
	NodeID    int64  `json:"node_id"`
	VersionID string `json:"version_id"`
	BlobHash  string `json:"blob_hash"`
	Size      int64  `json:"size"`
	Output    string `json:"output"`
}

var getCmd = &cobra.Command{
	Use:   "get <path-or-id> <local-file>",
	Short: "Download one file with end-to-end verification",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGet(cmd, args[0], args[1])
	},
}

func runGet(cmd *cobra.Command, rawSelector, rawOutput string) (retErr error) {
	selector, err := parseNodeSelector(rawSelector)
	if err != nil {
		return err
	}
	outputPath, err := prepareGetDestination(rawOutput, getOverwrite)
	if err != nil {
		return err
	}
	mode := backupProgressAuto
	if !getJSON {
		mode, err = progressModeFromFlag("get", getProgress)
		if err != nil {
			return err
		}
	}
	progressOutput := cmd.ErrOrStderr()
	if getJSON {
		progressOutput = io.Discard
	}
	renderer := newBackupProgressRenderer(progressOutput, mode)
	defer renderer.finish()

	staging, err := makePrivateStagingDirAt(filepath.Dir(outputPath), "docbank-get-")
	if err != nil {
		return fmt.Errorf("creating private download staging: %w", err)
	}
	published := false
	defer func() {
		if cleanupErr := staging.removeAll(); cleanupErr != nil {
			if published {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: downloaded file is complete, but staging cleanup failed: %v\n",
					cleanupErr)
				return
			}
			retErr = errors.Join(retErr,
				fmt.Errorf("removing private download staging: %w", cleanupErr))
		}
	}()

	c, err := client.Ensure(cmd.Context())
	if err != nil {
		return err
	}
	node, err := selector.resolveIncludingTrash(cmd.Context(), c)
	if err != nil {
		return err
	}
	if node.Kind != "file" {
		return fmt.Errorf("%q: %w", rawSelector, store.ErrNotFile)
	}
	stream, err := c.VersionContent(cmd.Context(), node.CurrentVersionID)
	if err != nil {
		return err
	}
	if err := validateGetStreamAuthority(stream, node); err != nil {
		_ = stream.Close()
		return err
	}
	written, err := stageAndPublishGet(cmd.Context(), staging, stream, node.Size,
		outputPath, getOverwrite, renderer)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("destination %s already exists; pass --overwrite to replace it: %w",
				strconv.Quote(outputPath), os.ErrExist)
		}
		if errors.Is(err, client.ErrIntegrity) {
			return integrityError(fmt.Errorf("downloading %q: %w", rawSelector, err))
		}
		return err
	}
	published = true

	receipt := getReceipt{
		NodeID: node.ID, VersionID: node.CurrentVersionID, BlobHash: node.BlobHash,
		Size: written, Output: outputPath,
	}
	if getJSON {
		return writeCLIJSON(cmd.OutOrStdout(), receipt)
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "downloaded %s version %s to %s (%s, sha256 %s)\n",
		formatNodeSelector(node.ID), node.CurrentVersionID, strconv.Quote(outputPath),
		formatBackupBytes(written), node.BlobHash)
	if err != nil {
		return fmt.Errorf("writing download receipt: %w", err)
	}
	return nil
}

func stageAndPublishGet(
	ctx context.Context,
	staging *privateStaging,
	stream *client.ContentStream,
	total int64,
	outputPath string,
	overwrite bool,
	renderer *backupProgressRenderer,
) (written int64, retErr error) {
	streamOpen := true
	defer func() {
		if streamOpen {
			retErr = errors.Join(retErr, stream.Close())
		}
	}()
	file, stagedPath, err := staging.createFile(filepath.Base(outputPath))
	if err != nil {
		return 0, err
	}
	fileOpen := true
	defer func() {
		if fileOpen {
			retErr = errors.Join(retErr, file.Close())
		}
	}()
	progress := newEditProgressWriter(ctx, file, total, renderer)
	written, err = stream.CopyVerified(progress)
	if err != nil {
		return written, err
	}
	progress.finish()
	if err := file.Sync(); err != nil {
		return written, fmt.Errorf("syncing downloaded file: %w", err)
	}
	closeErr := file.Close()
	fileOpen = false
	if closeErr != nil {
		return written, fmt.Errorf("closing downloaded file: %w", closeErr)
	}
	closeErr = stream.Close()
	streamOpen = false
	if closeErr != nil {
		return written, fmt.Errorf("closing verified download: %w", closeErr)
	}
	if err := publishGetFile(stagedPath, outputPath, overwrite); err != nil {
		return written, fmt.Errorf("publishing downloaded file: %w", err)
	}
	return written, nil
}

func prepareGetDestination(raw string, overwrite bool) (string, error) {
	if !utf8.ValidString(raw) {
		return "", usageError(fmt.Errorf("download destination %s is not valid UTF-8",
			strconv.QuoteToASCII(raw)))
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolving download destination %q: %w", raw, err)
	}
	info, err := os.Lstat(abs)
	if err == nil {
		if info.IsDir() {
			return "", usageError(fmt.Errorf("download destination %s is a directory",
				strconv.Quote(abs)))
		}
		if !overwrite {
			return "", usageError(fmt.Errorf(
				"download destination %s already exists; pass --overwrite to replace it",
				strconv.Quote(abs)))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("checking download destination %s: %w", strconv.Quote(abs), err)
	}
	parent, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("checking download destination directory: %w", err)
	}
	if !parent.IsDir() {
		return "", usageError(fmt.Errorf("download destination parent is not a directory: %s",
			strconv.Quote(filepath.Dir(abs))))
	}
	return abs, nil
}

func validateGetStreamAuthority(stream *client.ContentStream, node api.Node) error {
	if stream.VersionID == node.CurrentVersionID && stream.BlobHash == node.BlobHash &&
		stream.Size == node.Size {
		return nil
	}
	return integrityError(fmt.Errorf(
		"version authority %s/%s/%d disagrees with node authority %s/%s/%d",
		stream.VersionID, stream.BlobHash, stream.Size,
		node.CurrentVersionID, node.BlobHash, node.Size,
	))
}

func init() {
	getCmd.Flags().BoolVar(&getOverwrite, "overwrite", false,
		"replace an existing local file after the download verifies")
	getCmd.Flags().BoolVar(&getJSON, "json", false,
		"emit a machine-readable download receipt (progress suppressed)")
	getCmd.Flags().StringVar(&getProgress, "progress", "auto",
		"progress output mode: auto, bar, or plain (suppressed by --json)")
	rootCmd.AddCommand(getCmd)
}
