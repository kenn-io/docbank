package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/report"
)

const reportFileLimit int64 = 512 << 20

func writeReportCoverage(output io.Writer, summary report.Summary) error {
	if summary.State == "needs_review" {
		if _, err := fmt.Fprintln(output, "coverage pending date review; counts are withheld"); err != nil {
			return fmt.Errorf("writing pending report coverage: %w", err)
		}
		return nil
	}
	coverage := summary.Coverage
	if _, err := fmt.Fprintf(output, "coverage: %d scoped, %d searchable, %d missing text, %d incomplete families, %d fallback dates\n",
		coverage.Scoped, coverage.Searchable, coverage.MissingText, coverage.IncompleteFamilies, coverage.FallbackDates); err != nil {
		return fmt.Errorf("writing report coverage: %w", err)
	}
	if coverage.MissingText > 0 || coverage.IncompleteFamilies > 0 {
		if _, err := fmt.Fprintln(output, "warning: counts exclude unavailable text or family evidence"); err != nil {
			return fmt.Errorf("writing report coverage limit: %w", err)
		}
	}
	for index, row := range summary.RowCoverage {
		if index >= len(summary.Terms) {
			break
		}
		if _, err := fmt.Fprintf(output, "term %d coverage: %d searchable, %d missing text, %d incomplete families, %d fallback dates\n",
			summary.Terms[index].Number, row.Searchable, row.MissingText, row.IncompleteFamilies, row.FallbackDates); err != nil {
			return fmt.Errorf("writing term coverage: %w", err)
		}
	}
	for _, warning := range coverage.Warnings {
		if _, err := fmt.Fprintf(output, "warning: %s\n", warning); err != nil {
			return fmt.Errorf("writing report warning: %w", err)
		}
	}
	return nil
}

func readReportJSON(path string, output any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	bytes, err := io.ReadAll(io.LimitReader(file, 8<<20+1))
	if err != nil {
		return err
	}
	if len(bytes) > 8<<20 {
		return errors.New("report JSON exceeds 8 MiB")
	}
	return json.Unmarshal(bytes, output, json.RejectUnknownMembers(true))
}

func verifyReportFile(ctx context.Context, path string) (report.Verification, error) {
	file, err := os.Open(path)
	if err != nil {
		return report.Verification{}, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return report.Verification{}, err
	}
	budget := report.NewBudget(report.DefaultBudgetBytes)
	defer func() { _ = budget.Close() }()
	return report.VerifyBundle(ctx, budget, file, info.Size())
}

func publishReportOutput(rawOutput string, overwrite bool, write func(*os.File) error) (retErr error) {
	destination, err := prepareGetDestination(rawOutput, overwrite)
	if err != nil {
		return err
	}
	staging, err := makePrivateStagingDirAt(filepath.Dir(destination), "docbank-report-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, staging.removeAll()) }()
	file, stagedPath, err := staging.createFile(filepath.Base(destination))
	if err != nil {
		return err
	}
	writeErr := write(file)
	closeErr := errors.Join(file.Sync(), file.Close())
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return publishGetFile(stagedPath, destination, overwrite)
}

func downloadReportPacket(ctx context.Context, connection *daemonconn.Connection,
	id, output string, overwrite bool,
) error {
	return publishReportOutput(output, overwrite, func(file *os.File) error {
		stream, err := connection.OpenTermReport(ctx, id, "bundle")
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		if _, err := stream.CopyVerified(file); err != nil {
			return err
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		budget := report.NewBudget(report.DefaultBudgetBytes)
		defer func() { _ = budget.Close() }()
		_, err = report.VerifyBundle(ctx, budget, file, stream.Size)
		return err
	})
}

func downloadReportArtifactFile(ctx context.Context, connection *daemonconn.Connection,
	id, format, output string, overwrite bool,
) error {
	if format == "bundle" {
		return downloadReportPacket(ctx, connection, id, output, overwrite)
	}
	return publishReportOutput(output, overwrite, func(file *os.File) error {
		stream, err := connection.OpenTermReport(ctx, id, format)
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		_, err = stream.CopyVerified(file)
		return err
	})
}

func init() {
	root := &cobra.Command{Use: "search-export", Short: "Export search counts and review saved evidence"}
	terms := &cobra.Command{Use: "create", Short: "Export search counts from the current vault", Args: cobra.NoArgs}
	var input, output string
	var overwrite bool
	terms.Flags().StringVar(&input, "input", "", "Versioned report request JSON file")
	terms.Flags().StringVar(&output, "output", "", "Destination for the evidence packet")
	terms.Flags().BoolVar(&overwrite, "overwrite", false, "Replace an existing destination")
	terms.RunE = func(cmd *cobra.Command, _ []string) error {
		if input == "" || output == "" {
			return usageError(errors.New("search-export create requires --input and --output"))
		}
		if _, err := prepareGetDestination(output, overwrite); err != nil {
			return err
		}
		var request report.Request
		if err := readReportJSON(input, &request); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		summary, err := connection.CreateTermReport(cmd.Context(), request)
		if err != nil {
			return err
		}
		if err := writeReportCoverage(cmd.OutOrStdout(), summary); err != nil {
			return err
		}
		if summary.State == "needs_review" {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "export %s needs date review; run docbank search-export dates %s, then docbank search-export revise %s --choices choices.json --output %s\n",
				summary.ID, summary.ID, summary.ID, output)
			if err != nil {
				return fmt.Errorf("writing report review instructions: %w", err)
			}
			return nil
		}
		if err := downloadReportPacket(cmd.Context(), connection, summary.ID, output, overwrite); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · export %s · internally verified; source evidence not checked offline\n", output, summary.ID)
		if err != nil {
			return fmt.Errorf("writing report output: %w", err)
		}
		return nil
	}

	verify := &cobra.Command{Use: "verify <report.zip>", Short: "Check a report packet without opening a vault", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := verifyReportFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "internally consistent: %t; source verified: %t\n", result.InternallyConsistent, result.SourceVerified)
			if err != nil {
				return fmt.Errorf("writing report verification: %w", err)
			}
			return nil
		}}

	download := &cobra.Command{Use: "download <report-id>", Short: "Download a frozen report artifact", Args: cobra.ExactArgs(1)}
	var downloadFormat, downloadOutput string
	var downloadOverwrite bool
	download.Flags().StringVar(&downloadFormat, "format", "", "Artifact format: csv or bundle")
	download.Flags().StringVar(&downloadOutput, "output", "", "Destination for the artifact")
	download.Flags().BoolVar(&downloadOverwrite, "overwrite", false, "Replace an existing destination")
	download.RunE = func(cmd *cobra.Command, args []string) error {
		if (downloadFormat != "csv" && downloadFormat != "bundle") || downloadOutput == "" {
			return usageError(errors.New("search-export download requires --format csv|bundle and --output"))
		}
		if _, err := prepareGetDestination(downloadOutput, downloadOverwrite); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return downloadReportArtifactFile(cmd.Context(), connection, args[0], downloadFormat,
			downloadOutput, downloadOverwrite)
	}

	csv := &cobra.Command{Use: "csv <report.zip>", Short: "Extract verified report counts without opening a vault", Args: cobra.ExactArgs(1)}
	var csvOutput string
	var csvOverwrite bool
	csv.Flags().StringVar(&csvOutput, "output", "", "Destination for search-export.csv")
	csv.Flags().BoolVar(&csvOverwrite, "overwrite", false, "Replace an existing destination")
	csv.RunE = func(cmd *cobra.Command, args []string) error {
		if csvOutput == "" {
			return usageError(errors.New("search-export csv requires --output"))
		}
		return publishReportOutput(csvOutput, csvOverwrite, func(file *os.File) error {
			packet, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = packet.Close() }()
			info, err := packet.Stat()
			if err != nil {
				return err
			}
			if info.Size() > reportFileLimit {
				return report.ErrInvalidPacket
			}
			budget := report.NewBudget(report.DefaultBudgetBytes)
			defer func() { _ = budget.Close() }()
			content, err := report.ExtractVerifiedCSV(cmd.Context(), budget, packet, info.Size())
			if err != nil {
				return err
			}
			_, err = file.Write(content)
			return err
		})
	}

	dates := &cobra.Command{Use: "dates <report-id>", Short: "Inspect frozen date candidates", Args: cobra.ExactArgs(1)}
	var cursor string
	var limit int
	dates.Flags().StringVar(&cursor, "cursor", "", "Continue from a date-page cursor")
	dates.Flags().IntVar(&limit, "limit", 50, "Maximum members on this page (1–100)")
	dates.RunE = func(cmd *cobra.Command, args []string) error {
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		page, err := connection.TermReportDates(cmd.Context(), args[0], report.DatePageRequest{Cursor: cursor, Limit: limit})
		if err != nil {
			return err
		}
		return json.MarshalWrite(cmd.OutOrStdout(), page)
	}

	revise := &cobra.Command{Use: "revise <report-id>", Short: "Apply reviewed dates to a frozen report", Args: cobra.ExactArgs(1)}
	var choicesPath, revisionOutput string
	var revisionOverwrite bool
	revise.Flags().StringVar(&choicesPath, "choices", "", "JSON array of evidence-bound date choices")
	revise.Flags().StringVar(&revisionOutput, "output", "", "Destination for the revised evidence packet")
	revise.Flags().BoolVar(&revisionOverwrite, "overwrite", false, "Replace an existing destination")
	revise.RunE = func(cmd *cobra.Command, args []string) error {
		if choicesPath == "" || revisionOutput == "" {
			return usageError(errors.New("search-export revise requires --choices and --output"))
		}
		if _, err := prepareGetDestination(revisionOutput, revisionOverwrite); err != nil {
			return err
		}
		var choices []report.DateChoice
		if err := readReportJSON(choicesPath, &choices); err != nil {
			return err
		}
		connection, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		summary, err := connection.ReviseTermReport(cmd.Context(), args[0], choices)
		if err != nil {
			return err
		}
		if err := writeReportCoverage(cmd.OutOrStdout(), summary); err != nil {
			return err
		}
		if summary.State == "needs_review" {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "export %s still needs date review; run docbank search-export dates %s\n", summary.ID, summary.ID)
			if err != nil {
				return fmt.Errorf("writing report revision status: %w", err)
			}
			return nil
		}
		if err := downloadReportPacket(cmd.Context(), connection, summary.ID, revisionOutput, revisionOverwrite); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s · export %s · internally verified; source evidence not checked offline\n", revisionOutput, summary.ID)
		if err != nil {
			return fmt.Errorf("writing report revision status: %w", err)
		}
		return nil
	}
	root.AddCommand(terms, verify, csv, dates, revise, download)
	rootCmd.AddCommand(root)
}
