package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/document/transfer"
)

var (
	transferVerifyArchiveID string
	transferVerifyJSON      bool
)

var transferVerifyCmd = &cobra.Command{
	Use:   "verify PACKAGE",
	Short: "Verify a local transfer package without starting the daemon",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reader, err := openLocalTransfer(cmd.Context(), args[0], transferVerifyArchiveID)
		if err != nil {
			return err
		}
		report, validateErr := transfer.Validate(cmd.Context(), reader)
		closeErr := reader.Close()
		if closeErr != nil || errors.Is(validateErr, transfer.ErrValidationIncomplete) ||
			validateErr != nil && !errors.Is(validateErr, transfer.ErrInvalidPackage) {
			return errors.Join(validateErr, closeErr)
		}
		if err := writeTransferVerification(cmd.OutOrStdout(), report, transferVerifyJSON); err != nil {
			return err
		}
		if validateErr != nil || !report.Valid {
			if validateErr == nil {
				validateErr = errors.New("transfer package is invalid")
			}
			return integrityError(validateErr)
		}
		return nil
	},
}

func openLocalTransfer(ctx context.Context, path, archiveID string) (transfer.PackageReader, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect transfer package: %w", err)
	}
	if info.IsDir() {
		return transfer.OpenDirectory(ctx, path)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("transfer verify: package must be a regular file or directory")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open transfer package: %w", err)
	}
	var magic [4]byte
	read, readErr := file.ReadAt(magic[:], 0)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(fmt.Errorf("transfer verify: cannot inspect package format: %w", readErr), file.Close())
	}
	if read >= 2 && bytes.Equal(magic[:2], []byte{'P', 'K'}) {
		return transfer.OpenZip(ctx, file, info.Size())
	}
	if archiveID == "" {
		_ = file.Close()
		return nil, usageError(transfer.ErrLegacyArchiveRequired)
	}
	reader, normalizeErr := transfer.ReadLegacyExportWithArchive(ctx, file,
		transfer.LegacyArchiveBinding{ArchiveID: archiveID})
	closeErr := file.Close()
	if normalizeErr != nil {
		if errors.Is(normalizeErr, transfer.ErrLegacyArchiveInvalid) {
			return nil, usageError(normalizeErr)
		}
		return nil, errors.Join(normalizeErr, closeErr)
	}
	if closeErr != nil {
		return nil, errors.Join(fmt.Errorf("transfer verify: close legacy input: %w", closeErr), reader.Close())
	}
	return reader, nil
}

func writeTransferVerification(w io.Writer, report transfer.Report, asJSON bool) error {
	if asJSON {
		return writeCLIJSON(w, report)
	}
	if report.Valid {
		status := "valid"
		if report.Partial {
			status += " partial"
		}
		_, err := fmt.Fprintf(w, "%s %s package %s for archive %s (%s authority)\n",
			status, report.Format, report.PackageID, report.ArchiveID, report.PackageAuthority)
		if err != nil {
			return fmt.Errorf("write transfer verification: %w", err)
		}
		if report.Partial {
			if _, err := fmt.Fprintf(w, "next_cursor: %q\n", report.NextCursor); err != nil {
				return fmt.Errorf("write transfer verification: %w", err)
			}
		}
		return nil
	}
	if _, err := fmt.Fprintf(w, "invalid %s package: %d finding(s)\n", report.Format, report.FindingsTotal); err != nil {
		return fmt.Errorf("write transfer verification: %w", err)
	}
	for _, finding := range report.Findings {
		if _, err := fmt.Fprintf(w, "%s: %s (%s)\n", finding.Path, finding.Detail, finding.Code); err != nil {
			return fmt.Errorf("write transfer verification: %w", err)
		}
	}
	return nil
}

func init() {
	transferVerifyCmd.Flags().StringVar(&transferVerifyArchiveID, "archive-id", "",
		"registered archive ID to bind when verifying a legacy JSONL export")
	transferVerifyCmd.Flags().BoolVar(&transferVerifyJSON, "json", false, "emit machine-readable JSON")
}
