package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"go.kenn.io/docbank/internal/api"
	"go.kenn.io/docbank/internal/client"
	"go.kenn.io/docbank/internal/jsontext"
	"go.kenn.io/docbank/internal/store"
)

var mvJSON bool

const maxBatchMovePlanBytes = 1 << 20

type batchMovePlanFile struct {
	Moves []batchMovePlanItem `json:"moves"`
}

type batchMovePlanItem struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

var mvCmd = &cobra.Command{
	Use:   "mv <source-path-or-id> <dest-path>",
	Short: "Move or rename a node (metadata only; bytes never move)",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		source, err := parseNodeSelector(args[0])
		if err != nil {
			return err
		}
		if !strings.HasPrefix(args[1], "/") {
			return usageError(errors.New("move destination must be an absolute virtual path"))
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		var moved api.Node
		if source.isID() {
			current, resolveErr := source.resolve(cmd.Context(), c)
			if resolveErr != nil {
				return resolveErr
			}
			moved, err = c.MoveToPath(
				cmd.Context(), current.ID, current.Revision, args[1],
			)
		} else {
			moved, err = c.MovePath(cmd.Context(), source.path, args[1])
		}
		if err != nil {
			return err
		}
		if mvJSON {
			return writeCLIJSON(cmd.OutOrStdout(), moved)
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "moved [%s] %s\n",
			formatNodeSelector(moved.ID), moved.Path)
		return nil
	},
}

var mvBatchCmd = &cobra.Command{
	Use:   "batch <plan.json|->",
	Short: "Apply an all-or-nothing reorganization plan",
	Long: `Apply one bounded JSON plan as a single metadata transaction.

Each item has "source" (an absolute path or id:<number>) and an absolute
"destination". Unlike ordinary mv, each destination is an exact final
coordinate—even when that coordinate initially holds a directory. A dash reads
the plan from standard input. Every destination is interpreted against the tree
as it existed before the transaction, so a plan can safely express file or
directory swaps and nested reorganizations.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		plan, err := readBatchMovePlan(cmd, args[0])
		if err != nil {
			return usageError(err)
		}
		c, err := client.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		moves := make([]api.BatchMoveItem, len(plan.Moves))
		for index, item := range plan.Moves {
			selector, parseErr := parseNodeSelector(item.Source)
			if parseErr != nil {
				return fmt.Errorf("move %d source: %w", index, parseErr)
			}
			move := api.BatchMoveItem{DestinationPath: item.Destination}
			if selector.isID() {
				node, resolveErr := selector.resolve(cmd.Context(), c)
				if resolveErr != nil {
					return resolveErr
				}
				move.NodeID, move.Revision = node.ID, node.Revision
			} else {
				move.SourcePath = selector.path
			}
			moves[index] = move
		}
		report, err := c.BatchMove(cmd.Context(), moves)
		if err != nil {
			return err
		}
		if mvJSON {
			return writeCLIJSON(cmd.OutOrStdout(), report)
		}
		for _, receipt := range report.Items {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "moved [%s] %q -> %q\n",
				formatNodeSelector(receipt.Node.ID), receipt.FromPath, receipt.Node.Path); err != nil {
				return fmt.Errorf("writing batch move receipt: %w", err)
			}
		}
		return nil
	},
}

func readBatchMovePlan(cmd *cobra.Command, path string) (batchMovePlanFile, error) {
	var reader = cmd.InOrStdin()
	var file *os.File
	if path != "-" {
		var err error
		file, err = os.Open(path)
		if err != nil {
			return batchMovePlanFile{}, fmt.Errorf("opening batch move plan: %w", err)
		}
		defer func() { _ = file.Close() }()
		reader = file
	}
	raw, err := io.ReadAll(io.LimitReader(reader, maxBatchMovePlanBytes+1))
	if err != nil {
		return batchMovePlanFile{}, fmt.Errorf("reading batch move plan: %w", err)
	}
	if len(raw) > maxBatchMovePlanBytes {
		return batchMovePlanFile{}, fmt.Errorf("batch move plan exceeds %d bytes", maxBatchMovePlanBytes)
	}
	if err := jsontext.Validate(raw, "batch move plan"); err != nil {
		return batchMovePlanFile{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var plan batchMovePlanFile
	if err := decoder.Decode(&plan); err != nil {
		return batchMovePlanFile{}, fmt.Errorf("decoding batch move plan: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return batchMovePlanFile{}, fmt.Errorf("decoding batch move plan: %w", err)
	}
	if len(plan.Moves) == 0 || len(plan.Moves) > store.MaxBatchMoves {
		return batchMovePlanFile{}, fmt.Errorf(
			"batch move plan requires 1-%d moves", store.MaxBatchMoves)
	}
	for index, move := range plan.Moves {
		if move.Source == "" || !strings.HasPrefix(move.Destination, "/") {
			return batchMovePlanFile{}, fmt.Errorf(
				"move %d requires source and an absolute destination", index)
		}
	}
	return plan, nil
}

func init() {
	mvCmd.Flags().BoolVar(&mvJSON, "json", false, "emit a machine-readable node receipt")
	mvBatchCmd.Flags().BoolVar(&mvJSON, "json", false, "emit machine-readable move receipts")
	mvCmd.AddCommand(mvBatchCmd)
	rootCmd.AddCommand(mvCmd)
}
