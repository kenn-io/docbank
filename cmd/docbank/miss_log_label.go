package main

import (
	"errors"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/misslog"
)

func newMissLogLabelCmd() *cobra.Command {
	var verdict, note string
	var references []string
	cmd := &cobra.Command{Use: "label <record-id>", Short: "Label a private search receipt (operator only)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(references) > 20 {
			return usageError(errors.New("at most 20 expected documents are allowed"))
		}
		feedback := misslog.Feedback{Verdict: verdict, Note: note}
		for _, reference := range references {
			value, ok := strings.CutPrefix(reference, "node:")
			if !ok {
				return usageError(errors.New("expected must be node:<id> or node:<id>@<content-version-id>"))
			}
			nodeText, version, _ := strings.Cut(value, "@")
			node, err := strconv.ParseInt(nodeText, 10, 64)
			if err != nil || node < 1 {
				return usageError(errors.New("expected document requires a positive node ID"))
			}
			feedback.Expected = append(feedback.Expected, misslog.Identity{NodeID: node, ContentVersionID: version})
		}
		if err := misslog.ValidateFeedback(feedback); err != nil {
			return usageError(err)
		}
		c, err := daemonconn.Ensure(cmd.Context())
		if err != nil {
			return err
		}
		return c.LabelMissLogRecord(cmd.Context(), args[0], feedback)
	}}
	cmd.Flags().StringVar(&verdict, "verdict", "", "wrong, missing, low_confidence, or none_in_vault")
	cmd.Flags().StringVar(&note, "note", "", "private correction note (at most 1000 characters)")
	cmd.Flags().StringArrayVar(&references, "expected", nil, "expected node:<id>[@<content-version-id>] (repeat up to 20 times)")
	_ = cmd.MarkFlagRequired("verdict")
	cmd.Example = "  docbank miss-log label <record-id> --verdict wrong --expected node:42\n  docbank miss-log label <record-id> --verdict none_in_vault"
	return cmd
}
