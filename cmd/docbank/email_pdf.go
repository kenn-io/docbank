package main

import (
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document"
	"go.kenn.io/docbank/internal/client"
	"path/filepath"
	"time"
)

func init() {
	var paper string
	var overwrite bool
	command := &cobra.Command{Use: "email-pdf <version-id> <local-file>", Short: "Render and download one exact email version as a verified PDF", Args: cobra.ExactArgs(2)}
	command.Flags().StringVar(&paper, "paper", "A4", "Portrait paper: A4 or Letter")
	command.Flags().BoolVar(&overwrite, "overwrite", false, "Replace an existing destination")
	command.RunE = func(cmd *cobra.Command, args []string) (retErr error) {
		if paper != "A4" && paper != "Letter" {
			return errors.New("email PDF paper must be A4 or Letter")
		}
		destination, err := prepareGetDestination(args[1], overwrite)
		if err != nil {
			return err
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		job, err := c.RequestEmailPDF(cmd.Context(), document.EmailPDFRequest{VersionID: args[0], Paper: paper, Consent: true})
		if err != nil {
			return err
		}
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for job.State != "completed" {
			switch job.State {
			case "failed", "operator_required", "cancelled", "canceled", "blocked":
				return fmt.Errorf("email PDF job %s ended %s; inspect processing jobs", job.JobID, job.State)
			}
			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-ticker.C:
			}
			job.State, err = c.EmailPDFJobState(cmd.Context(), job.JobID)
			if err != nil {
				return err
			}
		}
		receipt, err := c.EmailPDFReceipt(cmd.Context(), job.VersionID, job.ProfileFingerprint)
		if err != nil {
			return err
		}
		b, err := c.DownloadEmailPDF(cmd.Context(), receipt)
		if err != nil {
			return err
		}
		staging, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-email-pdf-")
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, staging.removeAll()) }()
		f, path, err := staging.createFile(filepath.Base(destination))
		if err != nil {
			return err
		}
		_, writeErr := f.Write(b)
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return err
		}
		if err = publishGetFile(path, destination, overwrite); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · %d pages · SHA-256 %s\n", destination, receipt.Output.Pages, receipt.Output.PDFSHA256)
		if err != nil {
			return fmt.Errorf("writing PDF download receipt: %w", err)
		}
		return nil
	}
	rootCmd.AddCommand(command)
}
