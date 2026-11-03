package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/filepublish"
	"go.kenn.io/docbank/internal/misslog"
)

func newMissLogExportCmd() *cobra.Command {
	return &cobra.Command{Use: "export <eval|cards> <local-file>", Short: "Save labeled search examples to a new private file", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		format := args[0]
		if (format != "eval" && format != "cards") || args[1] == "-" || args[1] == "" {
			return usageError(errors.New("export requires eval or cards and a new local file"))
		}
		exported, skipped := 0, map[string]int{}
		err := writePrivateMissLogExport(args[1], func(w io.Writer) (retErr error) {
			connection, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() {
				if err := connection.Close(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("closing miss log daemon connection: %w", err))
				}
			}()
			limited := &missLogExportWriter{writer: w, remaining: 128 << 20}
			if format == "eval" {
				if err := misslog.WriteEvalCSV(limited, nil); err != nil {
					return err
				}
			}
			offset := 0
			for {
				page, err := connection.ExportMissLog(cmd.Context(), format, 100, offset)
				if err != nil {
					return err
				}
				if format == "eval" {
					err = misslog.WriteEvalCSVRows(limited, page.Items)
				} else {
					err = misslog.WriteCardsJSONL(limited, page.Items)
				}
				if err != nil {
					return err
				}
				exported += len(page.Items)
				for reason, count := range page.Skipped {
					skipped[reason] += count
				}
				if page.Done {
					return nil
				}
				offset = page.NextOffset
				if offset > 100000 {
					return errors.New("miss log export exceeds 100000 scanned records")
				}
			}
		})
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%d examples exported\n", exported); err != nil {
			return fmt.Errorf("writing miss log export summary: %w", err)
		}
		reasons := make([]string, 0, len(skipped))
		for reason := range skipped {
			reasons = append(reasons, reason)
		}
		slices.Sort(reasons)
		for _, reason := range reasons {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "skipped %s: %d\n", reason, skipped[reason]); err != nil {
				return fmt.Errorf("writing miss log export summary: %w", err)
			}
		}
		return nil
	}}
}

type missLogExportWriter struct {
	writer    io.Writer
	remaining int
}

func (w *missLogExportWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("miss log export exceeds 128 MiB")
	}
	n, err := w.writer.Write(p)
	w.remaining -= n
	return n, err
}

// Stage privately, then publish without replacement only after every page and
// sync succeeds. An interrupted or failed export leaves no partial destination.
func writePrivateMissLogExport(path string, write func(io.Writer) error) (retErr error) {
	if _, err := os.Lstat(path); err == nil {
		return usageError(errors.New("miss log export destination already exists"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	destination, err := prepareOutputDestination("miss log export destination", path, false)
	if err != nil {
		return err
	}
	stage, err := filepublish.CreateStage(filepath.Dir(destination), "docbank-miss-log-export-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, stage.Cleanup()) }()
	writeErr := write(stage.File)
	if err := errors.Join(writeErr, stage.File.Sync(), stage.File.Close()); err != nil {
		return err
	}
	_, err = filepublish.Publish(stage.Path(), destination, false)
	return err
}
