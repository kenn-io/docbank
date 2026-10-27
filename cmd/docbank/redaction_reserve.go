package main

import (
	"errors"
	"fmt"
	"strings"
	"uuid"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/internal/daemonconn"
	"go.kenn.io/docbank/internal/processing"
)

func newRedactionReserveCommand() *cobra.Command {
	var namespace string
	var start int64
	var retain, asJSON bool
	cmd := &cobra.Command{Use: "reserve <preparation.json> <masks.json> <review.json>",
		Example: "  docbank redaction reserve preparation.json masks.json review.json --namespace <namespace-id> --start 1 --retain-original --json",
		Short:   "Retain the original indefinitely and reserve Bates numbers", Args: cobra.ExactArgs(3),
		Long: "Reserve numbers for one reviewed PDF. The original is retained indefinitely,\nincluding unfinished reservations. No release is available. Retry with the same\nnamespace, start and review after a lost reply; numbers are never reused.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !retain {
				return usageError(errors.New("--retain-original is required: the original is retained indefinitely with no release"))
			}
			if !daemonconn.IsCanonicalUUIDv4(namespace) || start < 1 {
				return usageError(errors.New("--namespace must be a canonical UUIDv4 and --start must be positive"))
			}
			inputs, err := readRedactionInputs(args[0], args[1])
			if err != nil {
				return err
			}
			review, err := readRedactionReview(args[2], inputs)
			if err != nil {
				return err
			}
			c, err := daemonconn.Ensure(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			result, err := c.ReserveNumberedPDF(cmd.Context(), processing.NumberedReservationRequest{Preparation: inputs.Preparation,
				Masks: inputs.Masks, ReviewSHA256: review.SHA256, NamespaceID: uuid.MustParse(namespace), StartAt: start, RetainOriginal: true})
			if err != nil {
				if errors.Is(err, daemonconn.ErrNumberedReservationUnknown) {
					return numberedReplayError(err, args, namespace, start)
				}
				return redactionCLIError(err)
			}
			if asJSON {
				err = writeCLIJSON(cmd.OutOrStdout(), result)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Allocation: %s (%s)\nSnapshot: %s\nRecipe SHA-256: %s\nRequest SHA-256: %s\nNumbers: %s through %s\nOriginal retained indefinitely; no release is available.\n",
					result.AllocationID, result.State, result.SnapshotID, result.RecipeSHA256, result.RequestSHA256, result.Pages[0].Label, result.Pages[len(result.Pages)-1].Label)
			}
			if err != nil {
				return numberedReplayError(err, args, namespace, start)
			}
			return nil
		}}
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace UUID for Bates numbers")
	cmd.Flags().Int64Var(&start, "start", 0, "explicit first number; use the same value when retrying")
	cmd.Flags().BoolVar(&retain, "retain-original", false, "keep the unredacted original indefinitely; no release is available")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON reservation and retention terms")
	return cmd
}

func numberedReplayError(err error, paths []string, namespace string, start int64) error {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
	}
	return fmt.Errorf("reservation may already be retained; repeat exactly: docbank redaction reserve %s --namespace %s --start %d --retain-original --json: %w",
		strings.Join(quoted, " "), namespace, start, err)
}
